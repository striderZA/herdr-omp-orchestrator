package coord

// Task 6 tests: Herdr event hooks, public pane moves, durable inbox delivery and
// the startup reconciliation pass.
//
// Two kinds of evidence are used together:
//
//   - The event payloads are built from shapes `herdr api schema --json`
//     documents (see testdata/herdr-events.json and testdata/events/*.json), and
//     the move payloads are taken from the stand-in Herdr CLI's own
//     `pane move --new-workspace` answer, so no field name is invented here.
//   - Every identity decision is asserted on the real SQLite store and on the
//     recorded argv of the stand-in CLI, so "nothing was sent", "nothing was
//     started" and "one note exists" are observations, not intentions.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// schemaExcerpt is the sanitized `herdr api schema --json` excerpt this package
// checks the manifest's hook names and the adapter's payloads against.
type schemaExcerpt struct {
	Comment           string   `json:"_comment"`
	Protocol          int      `json:"protocol"`
	EventKinds        []string `json:"event_kinds"`
	PaneInfoRequired  []string `json:"pane_info_required"`
	PaneInfoOptional  []string `json:"pane_info_optional"`
	AgentStatuses     []string `json:"agent_statuses"`
	AgentSessionKinds []string `json:"agent_session_kinds"`
	HookEvents        map[string]struct {
		DataType   string            `json:"data_type"`
		Required   []string          `json:"required"`
		Properties map[string]string `json:"properties"`
	} `json:"hook_events"`
}

func loadSchemaExcerpt(t *testing.T) schemaExcerpt {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "herdr-events.json"))
	if err != nil {
		t.Fatalf("read the schema excerpt: %v", err)
	}
	var excerpt schemaExcerpt
	if err := json.Unmarshal(data, &excerpt); err != nil {
		t.Fatalf("decode the schema excerpt: %v", err)
	}
	return excerpt
}

// eventFixture reads one sanitized event payload, substituting the recorded
// ids for the fixture's placeholders. The replacements are raw text, so a caller
// may substitute either a value or a whole JSON fragment.
func eventFixture(t *testing.T, name string, subs ...string) []byte {
	t.Helper()
	return substituteFixture(t, name, false, subs...)
}

// eventFixtureJSON is eventFixture with the replacements JSON-encoded, which is
// what a captured fixture needs: a Windows checkout path or a session file is a
// value that must be escaped to stay valid JSON.
func eventFixtureJSON(t *testing.T, name string, subs ...string) []byte {
	t.Helper()
	return substituteFixture(t, name, true, subs...)
}

func substituteFixture(t *testing.T, name string, encode bool, subs ...string) []byte {
	t.Helper()
	if len(subs)%2 != 0 {
		t.Fatalf("eventFixture(%s): substitutions must be pairs", name)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "events", name))
	if err != nil {
		t.Fatalf("read the %s fixture: %v", name, err)
	}
	text := string(data)
	for i := 0; i < len(subs); i += 2 {
		value := subs[i+1]
		if encode {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("encode %q: %v", value, err)
			}
			value = string(encoded[1 : len(encoded)-1])
		}
		text = strings.ReplaceAll(text, subs[i], value)
	}
	if !json.Valid([]byte(text)) {
		t.Fatalf("the %s fixture is not JSON after substitution", name)
	}
	return []byte(text)
}

// statusEventPayload renders the `pane.agent_status_changed` payload Herdr
// delivers for one pane, in the standalone event-data shape.
func statusEventPayload(t *testing.T, pane, status string) []byte {
	t.Helper()
	event := map[string]any{
		"type":          eventKindAgentStatus,
		"pane_id":       pane,
		"workspace_id":  workspaceOfPane(pane),
		"agent_status":  status,
		"agent":         "omp",
		"display_agent": "omp",
		"title":         "omp",
		"state_labels":  map[string]string{},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode the status event: %v", err)
	}
	return encoded
}

// movePayload renders the `pane.moved` payload for one recorded worker task:
// the pane it was bound to moves to `newPane`, in `newWorkspace`, keeping the
// terminal and session the task recorded - exactly what Herdr's move event
// carries for a genuine move of that pane.
func movePayload(t *testing.T, st task.Task, newPane, newWorkspace string) map[string]any {
	t.Helper()
	pane := map[string]any{
		"pane_id":        newPane,
		"terminal_id":    st.TerminalID,
		"workspace_id":   newWorkspace,
		"tab_id":         newWorkspace + ":t1",
		"label":          "",
		"title":          "pwsh",
		"agent":          "omp",
		"display_agent":  "omp",
		"agent_status":   "idle",
		"cwd":            st.WorktreePath,
		"foreground_cwd": st.WorktreePath,
		"focused":        false,
		"revision":       9,
	}
	if strings.TrimSpace(st.OMPSessionID) != "" {
		pane["agent_session"] = map[string]any{
			"source": "herdr:omp",
			"agent":  "omp",
			"kind":   task.SessionKindID,
			"value":  st.OMPSessionID,
		}
	}
	return map[string]any{
		"type":                  eventKindPaneMoved,
		"previous_pane_id":      st.PublicPaneID,
		"previous_workspace_id": st.CurrentWorkspaceID,
		"previous_tab_id":       st.CurrentWorkspaceID + ":t1",
		"pane":                  pane,
		"closed_workspace_id":   nil,
		"closed_tab_id":         nil,
	}
}

// encodeEvent renders one event payload.
func encodeEvent(t *testing.T, event map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	return encoded
}

// moveResult is the authoritative body of `pane move --new-workspace` as the
// stand-in CLI answered it.
func (f *coordFixture) moveResult(t *testing.T, pane string) map[string]any {
	t.Helper()
	raw, err := f.client.Call(context.Background(), "pane", "move", pane, "--new-workspace")
	if err != nil {
		t.Fatalf("pane move: %v", err)
	}
	var res struct {
		Type       string         `json:"type"`
		MoveResult map[string]any `json:"move_result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode the move result: %v", err)
	}
	if res.Type != "pane_move" {
		t.Fatalf("move result type = %q, want pane_move", res.Type)
	}
	return res.MoveResult
}

// eventFromMoveResult renders the `pane.moved` payload Herdr would deliver for
// one move: the move result's members, named by the schema's event variant.
func eventFromMoveResult(t *testing.T, move map[string]any) []byte {
	t.Helper()
	event := map[string]any{"type": eventKindPaneMoved}
	for _, key := range []string{
		"previous_pane_id", "previous_workspace_id", "previous_tab_id",
		"pane", "closed_workspace_id", "closed_tab_id",
	} {
		value, ok := move[key]
		if !ok {
			t.Fatalf("the move result carries no %s", key)
		}
		event[key] = value
	}
	return encodeEvent(t, event)
}

// ---------------------------------------------------------------------------
// fixture helpers
// ---------------------------------------------------------------------------

// rewriteState edits the stand-in Herdr session directly, which is how a test
// expresses what happens to a terminal outside the plugin: a restart, a closed
// pane, an agent that exited, or another OMP session taking over a pane.
func (f *coordFixture) rewriteState(mutate func(*fixtureState)) {
	f.t.Helper()
	var current fixtureState
	if err := readJSONFile(filepath.Join(f.dir, fixtureStateName), &current); err != nil {
		f.t.Fatalf("read state: %v", err)
	}
	current.ensure()
	mutate(&current)
	if err := writeJSONFile(filepath.Join(f.dir, fixtureStateName), &current); err != nil {
		f.t.Fatalf("write state: %v", err)
	}
}

// setAgentStatus changes the status Herdr reports for one pane's agent.
func (f *coordFixture) setAgentStatus(t *testing.T, pane, status string) {
	t.Helper()
	found := false
	f.rewriteState(func(currentState *fixtureState) {
		if agent := currentState.Agents[pane]; agent != nil {
			agent.Status = status
			found = true
		}
	})
	if !found {
		t.Fatalf("no agent is running in pane %q", pane)
	}
}

// inboxRows reads every durable note, with its delivery state.
func (f *coordFixture) inboxRows(t *testing.T) []task.Inbox {
	t.Helper()
	path := filepath.Join(stateDirOf(), "orchestrator.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatalf("open the state database read-only: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, project_key, task_id, kind, summary, delivery, delivery_state, delivery_detail
		   FROM inbox ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read the inbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []task.Inbox
	for rows.Next() {
		var in task.Inbox
		if err := rows.Scan(&in.ID, &in.ProjectKey, &in.TaskID, &in.Kind, &in.Summary,
			&in.Delivery, &in.DeliveryState, &in.DeliveryDetail); err != nil {
			t.Fatalf("scan the inbox: %v", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the inbox: %v", err)
	}
	return out
}

// notesOfKind filters the durable notes by kind.
func (f *coordFixture) notesOfKind(t *testing.T, kind string) []task.Inbox {
	t.Helper()
	var out []task.Inbox
	for _, note := range f.inboxRows(t) {
		if note.Kind == kind {
			out = append(out, note)
		}
	}
	return out
}

// noteState reads one note's delivery state.
func (f *coordFixture) noteState(t *testing.T, id string) string {
	t.Helper()
	note, err := f.store.Inbox(context.Background(), id)
	if err != nil {
		t.Fatalf("read note %q: %v", id, err)
	}
	return note.State()
}

// promptCall is one recorded `agent prompt` invocation.
type promptCall struct {
	Pane string
	Text string
}

// promptCalls lists every recorded `agent prompt` invocation, including the
// dispatcher's own brief submission.
func (f *coordFixture) promptCalls(t *testing.T) []promptCall {
	t.Helper()
	var out []promptCall
	for _, call := range f.calls() {
		if len(call.Argv) >= 4 && call.Argv[0]+" "+call.Argv[1] == "agent prompt" {
			out = append(out, promptCall{Pane: call.Argv[2], Text: call.Argv[3]})
		}
	}
	return out
}

// promptsTo counts the prompts submitted to one pane, so a notification can be
// told apart from the brief the dispatcher submitted to the worker's own pane.
func (f *coordFixture) promptsTo(t *testing.T, pane string) []promptCall {
	t.Helper()
	var out []promptCall
	for _, call := range f.promptCalls(t) {
		if call.Pane == pane {
			out = append(out, call)
		}
	}
	return out
}

// boundProject reads the bound project's facts.
func (f *coordFixture) boundProject(t *testing.T) task.Project {
	t.Helper()
	project, err := f.store.Project(context.Background(), f.projectKey())
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	return project
}

// taskByID reads one task's durable facts.
func (f *coordFixture) taskByID(t *testing.T, id string) task.Task {
	t.Helper()
	st, err := f.store.Task(context.Background(), id)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return st
}

// boundWorker dispatches one worker and returns its durable facts.
func (f *coordFixture) boundWorker(t *testing.T) task.Task {
	t.Helper()
	return f.mustDispatch("worker task", "do the work")
}

// mustHandle applies one event, failing the test when the hook refuses it.
func (f *coordFixture) mustHandle(t *testing.T, eventName string, payload []byte) {
	t.Helper()
	if err := f.coord.HandleEvent(context.Background(), eventName, payload); err != nil {
		t.Fatalf("HandleEvent(%s) err = %v", eventName, err)
	}
}

// requeue is the note ID `Progress` writes for one summary, computed with the
// coordinator's own recipe so a test can address the row it just queued.
func (f *coordFixture) requeue(t *testing.T, st task.Task, kind, summary, delivery string) string {
	t.Helper()
	return f.coord.noteFor(st, kind, summary, delivery).ID
}

// queueNote writes the durable note a worker's write leaves behind, without the
// delivery attempt that write also makes.
//
// A test that needs a note waiting in the inbox asks for exactly that: the row a
// worker's report creates is delivered by the same call now, so a pending note is
// written the way the store sees it - one durable row, untouched by delivery.
func (f *coordFixture) queueNote(t *testing.T, st task.Task, kind, summary, delivery string) task.Inbox {
	t.Helper()
	note := f.coord.noteFor(st, kind, summary, delivery)
	if _, err := f.store.AddInbox(context.Background(), note); err != nil {
		t.Fatalf("AddInbox(%s): %v", note.ID, err)
	}
	return note
}

// ---------------------------------------------------------------------------
// the event adapter
// ---------------------------------------------------------------------------

func TestEventPayloadAdapterAcceptsBothShapesAndRefusesContradictions(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	f.setAgentStatus(t, worker.PublicPaneID, "blocked")
	pane := worker.PublicPaneID
	workspace := workspaceOfPane(pane)

	// The standalone event-data shape the schema's EventData defines.
	standalone := eventFixture(t, "pane_agent_status_changed.json",
		"w2:p1", pane, `"workspace_id": "w2"`, `"workspace_id": "`+workspace+`"`)
	f.mustHandle(t, EventAgentStatus, standalone)
	recorded := f.taskByID(t, worker.ID)
	if recorded.Activity != "blocked" {
		t.Fatalf("activity = %q, want blocked", recorded.Activity)
	}

	// The socket event envelope shape (`{"event":..., "data":{...}}`), which the
	// same observation may arrive in.
	f.setAgentStatus(t, pane, "idle")
	envelope := eventFixture(t, "pane_agent_status_changed_envelope.json",
		"w2:p1", pane, `"workspace_id": "w2"`, `"workspace_id": "`+workspace+`"`)
	f.mustHandle(t, EventAgentStatus, envelope)
	if recorded := f.taskByID(t, worker.ID); recorded.Activity != "idle" {
		t.Fatalf("activity = %q, want idle", recorded.Activity)
	}

	refusals := []struct {
		name    string
		event   string
		payload []byte
	}{
		{"an unknown event name", "pane.created", []byte(`{"type":"pane_created"}`)},
		{"a payload that is not JSON", EventAgentStatus, []byte(`{"pane_id":`)},
		{"an empty payload", EventAgentStatus, nil},
		{"an oversized payload", EventPaneMoved, []byte(`{"pad":"` + strings.Repeat("x", MaxEventPayloadBytes) + `"}`)},
		{"a contradicting type", EventAgentStatus, []byte(`{"type":"pane_moved"}`)},
		{"a contradicting envelope", EventAgentStatus, []byte(`{"event":"pane_moved","data":{}}`)},
		{"a missing pane", EventAgentStatus, []byte(`{"type":"pane_agent_status_changed","workspace_id":"w2","agent_status":"idle"}`)},
		{"a missing workspace", EventAgentStatus, []byte(`{"type":"pane_agent_status_changed","pane_id":"w2:p1","agent_status":"idle"}`)},
		{"an unknown status", EventAgentStatus, []byte(`{"type":"pane_agent_status_changed","pane_id":"w2:p1","workspace_id":"w2","agent_status":"panicking"}`)},
		{"a move without a previous pane", EventPaneMoved, []byte(`{"type":"pane_moved","pane":{"pane_id":"w3:p1","terminal_id":"t3","workspace_id":"w3"}}`)},
		{"a move without a terminal", EventPaneMoved, []byte(`{"type":"pane_moved","previous_pane_id":"w2:p1","pane":{"pane_id":"w3:p1","workspace_id":"w3"}}`)},
		{"a move whose pane id disagrees with its workspace", EventPaneMoved, []byte(`{"type":"pane_moved","previous_pane_id":"w2:p1","pane":{"pane_id":"w3:p1","terminal_id":"t3","workspace_id":"w9"}}`)},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			err := f.coord.HandleEvent(context.Background(), refusal.event, refusal.payload)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}

	// An event about a pane this plugin does not own is a no-op, not a failure:
	// Herdr's session is shared with the user's own terminals.
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, "zz:p9", "working"))
	f.mustHandle(t, EventPaneMoved, encodeEvent(t, map[string]any{
		"type":                  eventKindPaneMoved,
		"previous_pane_id":      "zz:p9",
		"previous_workspace_id": "zz",
		"previous_tab_id":       "zz:t1",
		"pane": map[string]any{
			"pane_id": "zy:p1", "terminal_id": "term-zy", "workspace_id": "zy", "tab_id": "zy:t1",
		},
	}))
	if rows := f.inboxRows(t); len(rows) != 2 {
		t.Fatalf("inbox rows = %d, want the two activity notes and nothing else (%v)", len(rows), rows)
	}
}

func TestEventFixturesMatchTheRecordedSchemaShapes(t *testing.T) {
	excerpt := loadSchemaExcerpt(t)
	status := excerpt.HookEvents[EventAgentStatus]
	moved := excerpt.HookEvents[EventPaneMoved]

	// The standalone fixture is the schema's EventData variant: every required
	// member is present.
	var eventData map[string]any
	if err := json.Unmarshal(eventFixture(t, "pane_agent_status_changed.json"), &eventData); err != nil {
		t.Fatalf("decode the status fixture: %v", err)
	}
	for _, field := range status.Required {
		if _, ok := eventData[field]; !ok {
			t.Fatalf("the status fixture carries no required field %q", field)
		}
	}
	if got := eventData["type"]; got != status.DataType {
		t.Fatalf("status fixture type = %v, want %v", got, status.DataType)
	}

	// The move fixture carries the schema's required members, and its pane
	// object carries every field the schema requires of a PaneInfo.
	var movedData map[string]any
	if err := json.Unmarshal(eventFixture(t, "pane_moved.json"), &movedData); err != nil {
		t.Fatalf("decode the move fixture: %v", err)
	}
	for _, field := range moved.Required {
		if _, ok := movedData[field]; !ok {
			t.Fatalf("the move fixture carries no required field %q", field)
		}
	}
	pane, ok := movedData["pane"].(map[string]any)
	if !ok {
		t.Fatalf("the move fixture carries no pane object")
	}
	for _, field := range excerpt.PaneInfoRequired {
		if _, ok := pane[field]; !ok {
			t.Fatalf("the move fixture's pane carries no required %q", field)
		}
	}

	// The build only records statuses the schema defines.
	for status := range herdrAgentStatuses {
		if !containsString(excerpt.AgentStatuses, status) {
			t.Fatalf("this build records the status %q, which the installed schema does not define", status)
		}
	}
	// The event kinds the adapter names are the schema's kinds.
	for _, kind := range []string{eventKindAgentStatus, eventKindPaneMoved} {
		if !containsString(excerpt.EventKinds, kind) {
			t.Fatalf("the adapter names the event kind %q, which the installed schema does not define", kind)
		}
	}
}

// TestCapturedHookPayloadsDriveTheHandlers feeds the sanitized payloads a real
// Herdr session delivered to this plugin's hooks - captured during Task 6's
// integration gate (see testdata/events/captured_*.json) - through the handlers,
// so the adapter is proven against what Herdr actually sends: an envelope that
// names the event and a data object that names its own type, ids in
// "workspace:pane" form, a session reference of kind "path" carrying the OMP
// session file, and the closed source workspace a cross-workspace move reports.
func TestCapturedHookPayloadsDriveTheHandlers(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	workspace := workspaceOfPane(worker.PublicPaneID)

	// The status payload Herdr delivered for a real idle -> working transition.
	f.setAgentStatus(t, worker.PublicPaneID, "working")
	status := eventFixtureJSON(t, "captured_pane_agent_status_changed.json",
		"{{pane}}", worker.PublicPaneID,
		"{{workspace}}", workspace)
	f.mustHandle(t, EventAgentStatus, status)
	observed := f.taskByID(t, worker.ID)
	if observed.Activity != "working" {
		t.Fatalf("activity = %q, want working", observed.Activity)
	}
	if notes := f.notesOfKind(t, noteKindActivity); len(notes) != 1 {
		t.Fatalf("activity notes = %d, want 1", len(notes))
	}

	// The move payload Herdr delivered for a real cross-workspace move whose
	// source workspace it closed. The stand-in's own state is moved the same way
	// first: the plugin re-proves the pane against Herdr, so a payload alone is
	// not enough. The payload's session reference is the session *file* Herdr
	// reports (kind "path"), while the binding recorded the native id - the
	// cross-form pair the confirmed announcement has to relate.
	f.movedWorkerState(t, observed, "w7:p1", "w7", observed.OMPSessionID)
	move := eventFixtureJSON(t, "captured_pane_moved.json",
		"{{prev_pane}}", observed.PublicPaneID,
		"{{prev_workspace}}", observed.CurrentWorkspaceID,
		"{{pane}}", "w7:p1",
		"{{workspace}}", "w7",
		"{{terminal}}", observed.TerminalID,
		"{{checkout}}", observed.WorktreePath,
		"{{session_file}}", sessionFileFor(observed.OMPSessionID))
	f.mustHandle(t, EventPaneMoved, move)
	after := f.taskByID(t, worker.ID)
	switch {
	case after.PublicPaneID != "w7:p1":
		t.Fatalf("public pane = %q, want w7:p1", after.PublicPaneID)
	case after.CurrentWorkspaceID != "w7":
		t.Fatalf("current workspace = %q, want w7", after.CurrentWorkspaceID)
	case after.LaunchPaneID != observed.LaunchPaneID:
		t.Fatalf("launch pane = %q, want the recorded %q", after.LaunchPaneID, observed.LaunchPaneID)
	case after.WorktreeWorkspaceID != observed.WorktreeWorkspaceID:
		t.Fatalf("worktree workspace = %q, want the original %q", after.WorktreeWorkspaceID, observed.WorktreeWorkspaceID)
	case after.TerminalID != observed.TerminalID:
		t.Fatalf("terminal = %q, want the recorded %q", after.TerminalID, observed.TerminalID)
	case after.Attention != "" || after.Blocker != "":
		t.Fatalf("the captured closed source workspace raised attention=%q blocker=%q", after.Attention, after.Blocker)
	}
}

// TestManifestDeclaresTheInstalledHookEvents reads the manifest the way Herdr
// validates it, and checks that every hook it declares is one this build
// handles and one the installed schema defines.
func TestManifestDeclaresTheInstalledHookEvents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "herdr-plugin.toml"))
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	startups, events := parseHooks(t, string(data))
	if len(startups) != 1 {
		t.Fatalf("startup hooks = %d, want 1 (%v)", len(startups), startups)
	}
	if want := `["./herdr-omp-orchestrator.exe", "startup"]`; startups[0].command != want {
		t.Fatalf("startup command = %s, want %s", startups[0].command, want)
	}
	if len(events) != 2 {
		t.Fatalf("event hooks = %d, want 2 (%v)", len(events), events)
	}
	declared := map[string]bool{}
	for _, event := range events {
		if want := `["./herdr-omp-orchestrator.exe", "event"]`; event.command != want {
			t.Fatalf("event %q command = %s, want %s", event.on, event.command, want)
		}
		declared[event.on] = true
	}
	excerpt := loadSchemaExcerpt(t)
	for _, name := range []string{EventAgentStatus, EventPaneMoved} {
		if !declared[name] {
			t.Fatalf("the manifest does not declare the %s hook this build handles", name)
		}
		entry, ok := excerpt.HookEvents[name]
		if !ok {
			t.Fatalf("the installed schema defines no %s hook", name)
		}
		if hookKind(name) != entry.DataType {
			t.Fatalf("the adapter maps %s to %q, but the schema names its data %q", name, hookKind(name), entry.DataType)
		}
	}
	if len(declared) != len(events) {
		t.Fatalf("the manifest declares a duplicate hook: %v", events)
	}
}

// manifestHook is one parsed `[[startup]]` or `[[events]]` entry.
type manifestHook struct {
	on      string
	command string
}

// parseHooks reads the hook sections of a plugin manifest. It is deliberately a
// small reader rather than a dependency: the manifest is this repository's own
// file and only two fields of two section kinds matter here.
func parseHooks(t *testing.T, manifest string) (startups, events []manifestHook) {
	t.Helper()
	section := ""
	for _, raw := range strings.Split(manifest, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "[[startup]]":
			section = "startup"
			startups = append(startups, manifestHook{})
			continue
		case line == "[[events]]":
			section = "event"
			events = append(events, manifestHook{})
			continue
		case strings.HasPrefix(line, "[["):
			section = ""
			continue
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case section == "startup" && key == "command":
			startups[len(startups)-1].command = value
		case section == "event" && key == "command":
			events[len(events)-1].command = value
		case section == "event" && key == "on":
			events[len(events)-1].on = strings.Trim(value, `"`)
		}
	}
	return startups, events
}

// ---------------------------------------------------------------------------
// worker status hooks
// ---------------------------------------------------------------------------

func TestEventWorkerStatusIsRecordedOncePerTransitionAndNeverCompletes(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	pane := worker.PublicPaneID

	// The first observation: the worker went idle.
	f.setAgentStatus(t, pane, "idle")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, pane, "idle"))
	first := f.taskByID(t, worker.ID)
	if first.Activity != "idle" || first.ActivitySeenAt == 0 {
		t.Fatalf("activity = %q at %d, want idle with a timestamp", first.Activity, first.ActivitySeenAt)
	}
	if first.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q: a status observation never moves a task's phase", first.Phase, task.PromptSubmitted)
	}
	notes := f.notesOfKind(t, noteKindActivity)
	if len(notes) != 1 {
		t.Fatalf("activity notes = %d, want 1 (%v)", len(notes), notes)
	}
	if !strings.Contains(notes[0].Summary, "idle") {
		t.Fatalf("note summary = %q, want the observed transition", notes[0].Summary)
	}

	// Replaying the same event is the same fact: one note, no write.
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, pane, "idle"))
	replayed := f.taskByID(t, worker.ID)
	if replayed.Revision != first.Revision {
		t.Fatalf("replaying the same event moved the revision from %d to %d", first.Revision, replayed.Revision)
	}
	if notes := f.notesOfKind(t, noteKindActivity); len(notes) != 1 {
		t.Fatalf("activity notes after replay = %d, want 1", len(notes))
	}

	// A later transition is news again, and `working` is not a completion.
	f.setAgentStatus(t, pane, "working")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, pane, "working"))
	f.setAgentStatus(t, pane, "done")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, pane, "done"))
	final := f.taskByID(t, worker.ID)
	if final.Activity != "done" {
		t.Fatalf("activity = %q, want done", final.Activity)
	}
	if final.Report != nil || final.Disposition != "" {
		t.Fatalf("Herdr activity completed a task: report=%v disposition=%q", final.Report, final.Disposition)
	}
	if stage := task.StageOf(final); stage != task.Running {
		t.Fatalf("stage = %q, want %q: terminal activity is not task state", stage, task.Running)
	}
	if notes := f.notesOfKind(t, noteKindActivity); len(notes) != 3 {
		t.Fatalf("activity notes = %d, want 3 (idle, working, done)", len(notes))
	}
	// The worker's own status transitions must never be sent into its pane: the
	// only prompt it ever receives is the brief the dispatcher submitted.
	if prompts := f.promptsTo(t, pane); len(prompts) != 1 {
		t.Fatalf("prompts to the worker pane = %d, want only the dispatched brief (%v)", len(prompts), prompts)
	}
}

func TestEventPrimaryStatusIsNotAWorkerNoticeAndIdleFlushesOnce(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID

	// One durable worker note is queued, exactly as a worker's own report would
	// leave it.
	noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID
	if state := f.noteState(t, noteID); state != task.DeliveryPending {
		t.Fatalf("queued note state = %q, want %q", state, task.DeliveryPending)
	}
	before := len(f.inboxRows(t))

	// The primary's own transition is not a worker notice.
	f.setAgentStatus(t, primary, "working")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "working"))
	if rows := f.inboxRows(t); len(rows) != before {
		t.Fatalf("a primary transition wrote %d notes, want none", len(rows)-before)
	}
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("a working primary was prompted %d times, want 0", len(prompts))
	}

	// A verified idle transition flushes the queued worker note once.
	f.setAgentStatus(t, primary, "idle")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "idle"))
	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want exactly 1 (%v)", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0].Text, worker.ID) || !strings.Contains(prompts[0].Text, "halfway") {
		t.Fatalf("prompt text = %q, want the task id and the note", prompts[0].Text)
	}
	if state := f.noteState(t, noteID); state != task.DeliveryDelivered {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
	}

	// Repeating the idle event delivers nothing again.
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "idle"))
	if prompts := f.promptsTo(t, primary); len(prompts) != 1 {
		t.Fatalf("prompts after the repeat = %d, want 1", len(prompts))
	}
}

// ---------------------------------------------------------------------------
// pane moves
// ---------------------------------------------------------------------------

func TestMoveWorkerUpdatesOnlyThePublicIdentityAndKeepsReporting(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	launch := worker.LaunchPaneID
	worktreeWorkspace := worker.WorktreeWorkspaceID

	move := f.moveResult(t, worker.PublicPaneID)
	movedPane, _ := move["pane"].(map[string]any)
	newPane, _ := movedPane["pane_id"].(string)
	newWorkspace, _ := movedPane["workspace_id"].(string)
	if newPane == "" || newWorkspace == "" {
		t.Fatalf("the move result names no new pane: %v", move)
	}

	f.mustHandle(t, EventPaneMoved, eventFromMoveResult(t, move))
	after := f.taskByID(t, worker.ID)
	switch {
	case after.PublicPaneID != newPane:
		t.Fatalf("public pane = %q, want %q", after.PublicPaneID, newPane)
	case after.CurrentWorkspaceID != newWorkspace:
		t.Fatalf("current workspace = %q, want %q", after.CurrentWorkspaceID, newWorkspace)
	case after.LaunchPaneID != launch:
		t.Fatalf("launch pane = %q, want the recorded %q", after.LaunchPaneID, launch)
	case after.WorktreeWorkspaceID != worktreeWorkspace:
		t.Fatalf("worktree workspace = %q, want the original %q", after.WorktreeWorkspaceID, worktreeWorkspace)
	case after.TerminalID != worker.TerminalID:
		t.Fatalf("terminal = %q, want the recorded %q", after.TerminalID, worker.TerminalID)
	case after.WorktreePath != worker.WorktreePath:
		t.Fatalf("checkout = %q, want the recorded %q", after.WorktreePath, worker.WorktreePath)
	case after.Attention != "" || after.Blocker != "":
		t.Fatalf("the move raised attention=%q blocker=%q; a closed source workspace is expected", after.Attention, after.Blocker)
	}

	// Replaying the move changes nothing.
	f.mustHandle(t, EventPaneMoved, eventFromMoveResult(t, move))
	if replayed := f.taskByID(t, worker.ID); replayed.Revision != after.Revision {
		t.Fatalf("replaying the move moved the revision from %d to %d", after.Revision, replayed.Revision)
	}

	// Reporting from the process's own launch-time pane still resolves: the
	// bridge keys a caller by the pane its terminal inherited, and the move never
	// re-keyed that.
	if _, err := f.coord.Progress(context.Background(), launch, "still here"); err != nil {
		t.Fatalf("Progress from the launch pane after the move: %v", err)
	}
	if _, err := f.coord.Question(context.Background(), launch, "which contract should this use?"); err != nil {
		t.Fatalf("Question from the launch pane after the move: %v", err)
	}
	if notes := f.notesOfKind(t, "question"); len(notes) != 1 {
		t.Fatalf("question notes = %d, want 1", len(notes))
	}
}

func TestMovePrimaryUpdatesTheProjectAndFlushUsesTheNewPane(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	before := f.boundProject(t)

	move := f.moveResult(t, before.PublicPaneID)
	movedPane, _ := move["pane"].(map[string]any)
	newPane, _ := movedPane["pane_id"].(string)

	f.mustHandle(t, EventPaneMoved, eventFromMoveResult(t, move))
	after := f.boundProject(t)
	switch {
	case after.PublicPaneID != newPane:
		t.Fatalf("public pane = %q, want %q", after.PublicPaneID, newPane)
	case after.LaunchPaneID != before.LaunchPaneID:
		t.Fatalf("launch pane = %q, want the recorded %q", after.LaunchPaneID, before.LaunchPaneID)
	case after.Key != before.Key:
		t.Fatalf("project key = %q, want %q", after.Key, before.Key)
	case after.TerminalID != before.TerminalID:
		t.Fatalf("terminal = %q, want the recorded %q", after.TerminalID, before.TerminalID)
	case after.OMPSessionID != before.OMPSessionID:
		t.Fatalf("session = %q, want the recorded %q", after.OMPSessionID, before.OMPSessionID)
	}

	// A queued note is delivered to the moved pane, and an idle transition on the
	// new public id resolves the same project.
	if _, err := f.coord.Progress(context.Background(), worker.LaunchPaneID, "halfway"); err != nil {
		t.Fatalf("Progress: %v", err)
	}
	f.setAgentStatus(t, newPane, "idle")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, newPane, "idle"))
	prompts := f.promptsTo(t, newPane)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1 (%v)", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0].Text, worker.ID) {
		t.Fatalf("prompt text = %q, want the worker's task id", prompts[0].Text)
	}
}

func TestMoveRejectsMismatchedTerminalSessionOrCheckout(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(payload map[string]any)
		reason string
	}{
		{
			name: "a different terminal",
			mutate: func(payload map[string]any) {
				payload["pane"].(map[string]any)["terminal_id"] = "term-someone-else"
			},
			reason: "terminal",
		},
		{
			name: "a different session",
			mutate: func(payload map[string]any) {
				payload["pane"].(map[string]any)["agent_session"] = map[string]any{
					"source": "herdr:omp", "agent": "omp", "kind": "id", "value": "someone-elses-session",
				}
			},
			reason: "session",
		},
		{
			name: "a different checkout",
			mutate: func(payload map[string]any) {
				pane := payload["pane"].(map[string]any)
				pane["cwd"] = filepath.Join("C:", "somewhere", "else")
				pane["foreground_cwd"] = filepath.Join("C:", "somewhere", "else")
			},
			reason: "checkout",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			worker := f.boundWorker(t)
			payload := movePayload(t, worker, "w9:p1", "w9")
			testCase.mutate(payload)

			err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
			if !errors.Is(err, ErrAgentChanged) {
				t.Fatalf("err = %v, want ErrAgentChanged", err)
			}
			after := f.taskByID(t, worker.ID)
			if after.PublicPaneID != worker.PublicPaneID || after.CurrentWorkspaceID != worker.CurrentWorkspaceID {
				t.Fatalf("a rejected move re-keyed the task: pane %q workspace %q", after.PublicPaneID, after.CurrentWorkspaceID)
			}
			if after.Attention == "" {
				t.Fatalf("a rejected move left no attention for a human")
			}
			if !strings.Contains(after.Attention, testCase.reason) {
				t.Fatalf("attention = %q, want it to name the %s mismatch", after.Attention, testCase.reason)
			}
			if stage := task.StageOf(after); stage != task.NeedsInput {
				t.Fatalf("stage = %q, want %q", stage, task.NeedsInput)
			}
			if notes := f.notesOfKind(t, noteKindAttention); len(notes) != 1 {
				t.Fatalf("attention notes = %d, want 1", len(notes))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// startup reconciliation
// ---------------------------------------------------------------------------

func TestReconcileRecoversAMissedMoveFromTheSnapshot(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)

	// The pane moves and the event is lost; only Herdr's live state knows.
	move := f.moveResult(t, worker.PublicPaneID)
	newPane, _ := move["pane"].(map[string]any)["pane_id"].(string)
	newWorkspace, _ := move["pane"].(map[string]any)["workspace_id"].(string)

	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	after := f.taskByID(t, worker.ID)
	switch {
	case after.PublicPaneID != newPane:
		t.Fatalf("public pane = %q, want %q", after.PublicPaneID, newPane)
	case after.CurrentWorkspaceID != newWorkspace:
		t.Fatalf("current workspace = %q, want %q", after.CurrentWorkspaceID, newWorkspace)
	case after.LaunchPaneID != worker.LaunchPaneID:
		t.Fatalf("launch pane = %q, want the unchanged %q: the process kept its terminal", after.LaunchPaneID, worker.LaunchPaneID)
	case after.WorktreeWorkspaceID != worker.WorktreeWorkspaceID:
		t.Fatalf("worktree workspace = %q, want the original %q", after.WorktreeWorkspaceID, worker.WorktreeWorkspaceID)
	case after.TerminalID != worker.TerminalID:
		t.Fatalf("terminal = %q, want the recorded %q", after.TerminalID, worker.TerminalID)
	case after.Attention != "" || after.Blocker != "":
		t.Fatalf("recovery raised attention=%q blocker=%q", after.Attention, after.Blocker)
	}
	// A second pass over the same state writes nothing new.
	stable := f.taskByID(t, worker.ID)
	primary := f.boundProject(t).PublicPaneID
	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again := f.taskByID(t, worker.ID); again.Revision != stable.Revision {
		t.Fatalf("a repeated startup pass moved the revision from %d to %d", stable.Revision, again.Revision)
	}
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("a repeated startup pass prompted the primary %d times", len(prompts))
	}

	// Reporting from the process's own launch-time pane still resolves.
	if _, err := f.coord.Progress(context.Background(), worker.LaunchPaneID, "still here"); err != nil {
		t.Fatalf("Progress after recovery: %v", err)
	}
}

func TestReconcileAdoptsAVerifiedRestoredSessionInANewTerminal(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	startsBefore := f.countCalls("agent start")

	// Herdr restarts: the OMP session is restored in a new terminal, in a new
	// workspace that holds the same checkout.
	oldPane := worker.PublicPaneID
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[oldPane]
		if agent == nil {
			t.Fatalf("no agent in %q", oldPane)
		}
		delete(current.Agents, oldPane)
		delete(current.Workspaces, workspaceOfPane(oldPane))
		agent.Terminal = "term-restored"
		current.Workspaces["restored"] = worker.WorktreePath
		current.Agents["restored:p1"] = agent
	})

	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	after := f.taskByID(t, worker.ID)
	switch {
	case after.LaunchPaneID != "restored:p1":
		t.Fatalf("launch pane = %q, want the verified restored pane restored:p1", after.LaunchPaneID)
	case after.PublicPaneID != "restored:p1":
		t.Fatalf("public pane = %q, want restored:p1", after.PublicPaneID)
	case after.TerminalID != "term-restored":
		t.Fatalf("terminal = %q, want the restored terminal", after.TerminalID)
	case after.OMPSessionID != worker.OMPSessionID:
		t.Fatalf("session = %q, want the recorded %q", after.OMPSessionID, worker.OMPSessionID)
	case after.WorktreePath != worker.WorktreePath:
		t.Fatalf("checkout = %q, want the recorded %q", after.WorktreePath, worker.WorktreePath)
	case after.Attention != "" || after.Blocker != "":
		t.Fatalf("a verified restore raised attention=%q blocker=%q", after.Attention, after.Blocker)
	}
	if f.countCalls("agent start") != startsBefore {
		t.Fatalf("startup started an agent: %d start calls before, %d after", startsBefore, f.countCalls("agent start"))
	}
}

func TestReconcileRefusesToRekeyAnUnknownOccupantAndNeverRespawns(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	startsBefore := f.countCalls("agent start")
	oldPane := worker.PublicPaneID

	// Another OMP session now occupies a pane in the task's checkout: the
	// recorded worker is not there, and nothing may be re-keyed or started.
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[oldPane]
		if agent == nil {
			t.Fatalf("no agent in %q", oldPane)
		}
		delete(current.Agents, oldPane)
		delete(current.Workspaces, workspaceOfPane(oldPane))
		agent.Terminal = "term-someone-else"
		agent.Session = "someone-elses-session"
		current.Workspaces["other"] = worker.WorktreePath
		current.Agents["other:p1"] = agent
	})

	if err := f.coord.Reconcile(context.Background()); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, worker.ID)
	if stage := task.StageOf(after); stage != task.NeedsInput {
		t.Fatalf("stage = %q, want %q", stage, task.NeedsInput)
	}
	switch {
	case after.LaunchPaneID != worker.LaunchPaneID || after.PublicPaneID != worker.PublicPaneID:
		t.Fatalf("an unknown occupant re-keyed the task: launch %q public %q", after.LaunchPaneID, after.PublicPaneID)
	case after.OMPSessionID != worker.OMPSessionID:
		t.Fatalf("session = %q, want the recorded %q", after.OMPSessionID, worker.OMPSessionID)
	case after.Attention == "":
		t.Fatalf("an unknown occupant raised no attention")
	}
	if f.countCalls("agent start") != startsBefore {
		t.Fatalf("startup started an agent: %d start calls before, %d after", startsBefore, f.countCalls("agent start"))
	}
	if notes := f.notesOfKind(t, noteKindAttention); len(notes) != 1 {
		t.Fatalf("attention notes = %d, want 1", len(notes))
	}
}

func TestReconcileQueuesAnExitForAVanishedWorkerWithoutRespawning(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	startsBefore := f.countCalls("agent start")
	oldPane := worker.PublicPaneID

	// The agent process exited; the terminal (its pane) is still there.
	f.rewriteState(func(current *fixtureState) {
		delete(current.Agents, oldPane)
	})

	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	after := f.taskByID(t, worker.ID)
	switch {
	case after.Phase != worker.Phase:
		t.Fatalf("phase = %q, want the recorded %q", after.Phase, worker.Phase)
	case after.WorktreePath != worker.WorktreePath || after.Branch != worker.Branch:
		t.Fatalf("an exited worker lost its checkout provenance: path %q branch %q", after.WorktreePath, after.Branch)
	case after.Attention != "" || after.Blocker != "":
		t.Fatalf("a vanished worker raised attention=%q blocker=%q", after.Attention, after.Blocker)
	}
	if f.countCalls("agent start") != startsBefore {
		t.Fatalf("startup started an agent in a vanished worker's pane: %d start calls before, %d after", startsBefore, f.countCalls("agent start"))
	}
	notes := f.notesOfKind(t, noteKindExit)
	if len(notes) != 1 {
		t.Fatalf("exit notes = %d, want 1 (%v)", len(notes), notes)
	}
	if !strings.Contains(notes[0].Summary, worker.ID) {
		t.Fatalf("exit note = %q, want it to name the task", notes[0].Summary)
	}
	// Queued once: a second startup pass does not repeat it.
	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if notes := f.notesOfKind(t, noteKindExit); len(notes) != 1 {
		t.Fatalf("exit notes after a repeated pass = %d, want 1", len(notes))
	}
}

func TestReconcileKeepsInboxPendingUntilThePrimaryIsIdleAndVerified(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

	// A blocked primary cannot take the note, and the pass says so by doing
	// nothing: the note stays pending and no prompt is submitted.
	f.setAgentStatus(t, primary, "blocked")
	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile with a blocked primary: %v", err)
	}
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("a blocked primary was prompted %d times, want 0", len(prompts))
	}
	if state := f.noteState(t, noteID); state != task.DeliveryPending {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryPending)
	}

	// Repeating the pass changes nothing.
	stable := f.taskByID(t, worker.ID)
	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again := f.taskByID(t, worker.ID); again.Revision != stable.Revision {
		t.Fatalf("a repeated pass moved the revision from %d to %d", stable.Revision, again.Revision)
	}

	// Once the primary is verified idle, the note is submitted exactly once.
	f.setAgentStatus(t, primary, "idle")
	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile with an idle primary: %v", err)
	}
	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1 (%v)", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0].Text, worker.ID) {
		t.Fatalf("prompt text = %q, want the task id", prompts[0].Text)
	}
	if state := f.noteState(t, noteID); state != task.DeliveryDelivered {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
	}
}

func TestPrimaryLossKeepsWorkersAndInboxWithoutRespawning(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	before := f.boundProject(t)
	startsBefore := f.countCalls("agent start")
	noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

	// The primary's OMP process is gone; its workspace (and pane) remain.
	f.rewriteState(func(current *fixtureState) {
		delete(current.Agents, before.PublicPaneID)
	})

	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	after := f.boundProject(t)
	switch {
	case after.LaunchPaneID != before.LaunchPaneID || after.PublicPaneID != before.PublicPaneID:
		t.Fatalf("a lost primary was re-keyed: launch %q public %q", after.LaunchPaneID, after.PublicPaneID)
	case after.OMPSessionID != before.OMPSessionID || after.TerminalID != before.TerminalID:
		t.Fatalf("a lost primary was re-identified: session %q terminal %q", after.OMPSessionID, after.TerminalID)
	}
	if f.countCalls("agent start") != startsBefore {
		t.Fatalf("startup started an agent after a primary loss: %d start calls before, %d after", startsBefore, f.countCalls("agent start"))
	}
	if state := f.noteState(t, noteID); state != task.DeliveryPending {
		t.Fatalf("note state = %q, want %q: a lost primary's inbox is retained", state, task.DeliveryPending)
	}
	if prompts := f.promptsTo(t, before.PublicPaneID); len(prompts) != 0 {
		t.Fatalf("a lost primary was prompted %d times, want 0", len(prompts))
	}
	if kept := f.taskByID(t, worker.ID); kept.Phase != worker.Phase || kept.WorktreePath != worker.WorktreePath {
		t.Fatalf("a primary loss discarded an unfinished worker: phase %q path %q", kept.Phase, kept.WorktreePath)
	}
}

// ---------------------------------------------------------------------------
// durable inbox delivery
// ---------------------------------------------------------------------------

func TestInboxDeliveryIsClaimedOnceAndNeverBlindlyResent(t *testing.T) {
	t.Run("a claim has exactly one owner", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

		claimed, err := f.store.ClaimInbox(context.Background(), []string{noteID})
		if err != nil {
			t.Fatalf("ClaimInbox: %v", err)
		}
		if len(claimed) != 1 {
			t.Fatalf("first claim took %d notes, want 1", len(claimed))
		}
		second, err := f.store.ClaimInbox(context.Background(), []string{noteID})
		if err != nil {
			t.Fatalf("second ClaimInbox: %v", err)
		}
		if len(second) != 0 {
			t.Fatalf("second claim took %d notes, want 0: a claim is exclusive", len(second))
		}
		// A claimed note is not pending, so no flush submits it again.
		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox: %v", err)
		}
		if prompts := f.promptsTo(t, f.boundProject(t).PublicPaneID); len(prompts) != 0 {
			t.Fatalf("a claimed note was delivered %d times, want 0", len(prompts))
		}
	})

	t.Run("an ambiguous prompt becomes uncertain and is not resent", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		// The knob describes the notification prompt, not the dispatcher's brief,
		// so it is set only once the worker is dispatched.
		f.updatePlan(func(plan *fixturePlan) { plan.PromptAmbiguous = true })
		noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

		err := f.coord.FlushInbox(context.Background(), f.projectKey())
		if !errors.Is(err, ErrAmbiguousStep) {
			t.Fatalf("FlushInbox err = %v, want ErrAmbiguousStep", err)
		}
		if state := f.noteState(t, noteID); state != task.DeliveryUncertain {
			t.Fatalf("note state = %q, want %q", state, task.DeliveryUncertain)
		}
		if prompts := f.promptsTo(t, f.boundProject(t).PublicPaneID); len(prompts) != 1 {
			t.Fatalf("prompt attempts = %d, want 1", len(prompts))
		}
		note, err := f.store.Inbox(context.Background(), noteID)
		if err != nil {
			t.Fatalf("read the note: %v", err)
		}
		if !strings.Contains(note.DeliveryDetail, "agent get") {
			t.Fatalf("uncertain detail = %q, want the inspection guidance", note.DeliveryDetail)
		}
		// The uncertain note is inspected, never resent.
		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("second FlushInbox: %v", err)
		}
		if prompts := f.promptsTo(t, f.boundProject(t).PublicPaneID); len(prompts) != 1 {
			t.Fatalf("prompt attempts after the retry = %d, want 1", len(prompts))
		}
	})

	t.Run("a definite refusal keeps the note pending", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		// The refusal applies to the notification prompt only.
		f.updatePlan(func(plan *fixturePlan) { plan.PromptError = "agent_blocked" })
		noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err == nil {
			t.Fatalf("FlushInbox succeeded, want the refusal")
		}
		if state := f.noteState(t, noteID); state != task.DeliveryPending {
			t.Fatalf("note state = %q, want %q: nothing was written, so it may be retried", state, task.DeliveryPending)
		}
		if recorded := f.state().Prompts[f.boundProject(t).PublicPaneID]; len(recorded) != 0 {
			t.Fatalf("a refused prompt was recorded as delivered: %v", recorded)
		}
		// With the refusal gone, the same note is delivered exactly once.
		f.updatePlan(func(plan *fixturePlan) { plan.PromptError = "" })
		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox after the refusal: %v", err)
		}
		if state := f.noteState(t, noteID); state != task.DeliveryDelivered {
			t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
		}
		// Two attempts were made - the refused one and the accepted one - but only
		// the accepted one reached the primary's conversation.
		primary := f.boundProject(t).PublicPaneID
		if attempts := f.promptsTo(t, primary); len(attempts) != 2 {
			t.Fatalf("prompt attempts = %d, want the refusal and the delivery", len(attempts))
		}
		if delivered := f.state().Prompts[primary]; len(delivered) != 1 {
			t.Fatalf("recorded deliveries = %d, want 1 (%v)", len(delivered), delivered)
		}
	})

	t.Run("a claim interrupted by a stop becomes uncertain at startup", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

		// The plugin claimed the note and stopped before the prompt's outcome was
		// recorded.
		if claimed, err := f.store.ClaimInbox(context.Background(), []string{noteID}); err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimInbox = %d notes, err %v", len(claimed), err)
		}
		if err := f.coord.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if state := f.noteState(t, noteID); state != task.DeliveryUncertain {
			t.Fatalf("note state = %q, want %q", state, task.DeliveryUncertain)
		}
		if prompts := f.promptsTo(t, f.boundProject(t).PublicPaneID); len(prompts) != 0 {
			t.Fatalf("an interrupted claim was resent %d times, want 0", len(prompts))
		}
	})
}

// TestStoreRecoveryLeavesSettledNotesAlone pins the recovery pass to the notes a
// previous process actually interrupted.
func TestStoreRecoveryLeavesSettledNotesAlone(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	workerNote := f.coord.noteFor(worker, "progress", "one", "worker progress")
	if _, err := f.store.AddInbox(context.Background(), workerNote); err != nil {
		t.Fatalf("AddInbox pending: %v", err)
	}
	delivered := f.coord.noteFor(worker, "progress", "two", "worker progress")
	if _, err := f.store.AddInbox(context.Background(), delivered); err != nil {
		t.Fatalf("AddInbox delivered: %v", err)
	}
	claimed, err := f.store.ClaimInbox(context.Background(), []string{delivered.ID})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimInbox = %d, err %v", len(claimed), err)
	}
	if _, err := f.store.SettleInbox(context.Background(), []string{delivered.ID}, task.DeliveryDelivered, "prompted"); err != nil {
		t.Fatalf("SettleInbox: %v", err)
	}

	recovered, err := f.store.RecoverInbox(context.Background())
	if err != nil {
		t.Fatalf("RecoverInbox: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("RecoverInbox touched %d notes, want 0", recovered)
	}
	if state := f.noteState(t, workerNote.ID); state != task.DeliveryPending {
		t.Fatalf("pending note state = %q, want %q", state, task.DeliveryPending)
	}
	if state := f.noteState(t, delivered.ID); state != task.DeliveryDelivered {
		t.Fatalf("delivered note state = %q, want %q", state, task.DeliveryDelivered)
	}
}

// TestFlushInboxRequiresAProbeablePrimary covers the refusals a flush makes
// before it claims anything.
func TestFlushInboxRequiresAProbeablePrimary(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID

	if err := f.coord.FlushInbox(context.Background(), ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("FlushInbox(\"\") err = %v, want ErrInvalidRequest", err)
	}
	if err := f.coord.FlushInbox(context.Background(), "not-a-project"); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("FlushInbox(unknown) err = %v, want ErrProjectUnknown", err)
	}
	// A missing primary pane keeps the note pending, silently.
	primary := f.boundProject(t).PublicPaneID
	f.rewriteState(func(current *fixtureState) {
		delete(current.Agents, primary)
	})
	if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
		t.Fatalf("FlushInbox with a missing primary: %v", err)
	}
	if state := f.noteState(t, noteID); state != task.DeliveryPending {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryPending)
	}
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("a missing primary was prompted %d times, want 0", len(prompts))
	}
}

// TestInboxPromptIsBoundedAndNamesEveryTask pins the one prompt a flush submits.
func TestInboxPromptIsBoundedAndNamesEveryTask(t *testing.T) {
	notes := []task.Inbox{
		{ID: "1", ProjectKey: "repo", TaskID: "orch_a", Kind: "result", Summary: strings.Repeat("x", 5000)},
		{ID: "2", ProjectKey: "repo", TaskID: "orch_b", Kind: "question", Summary: "which contract?"},
		{ID: "3", ProjectKey: "repo", Kind: "blocked", Summary: "the bind failed"},
	}
	text := inboxPrompt(notes)
	if len(text) > inboxPromptLimit+len("…") {
		t.Fatalf("prompt is %d bytes, want at most %d", len(text), inboxPromptLimit)
	}
	for _, want := range []string{"orch_a", "orch_b", "which contract?", "the bind failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt %q does not name %q", text, want)
		}
	}
}

// ---------------------------------------------------------------------------
// projections the coordinator exposes to the board
// ---------------------------------------------------------------------------
// follow-up regressions (Task 6 review)
// ---------------------------------------------------------------------------

// movedWorkerState moves the stand-in's worker pane into a new workspace and
// keeps the checkout, the way a real `pane move` does. The terminal is the same
// process; session and pane id are what the caller says they are.
func (f *coordFixture) movedWorkerState(t *testing.T, st task.Task, pane, workspace, session string) {
	t.Helper()
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[st.PublicPaneID]
		if agent == nil {
			t.Fatalf("no agent is running in pane %q", st.PublicPaneID)
		}
		delete(current.Agents, st.PublicPaneID)
		delete(current.Workspaces, workspaceOfPane(st.PublicPaneID))
		agent.Session = session
		current.Workspaces[workspace] = st.WorktreePath
		current.Agents[pane] = agent
	})
}

// TestFlushInboxDeliversEveryClaimedNoteAndLeavesTheRestPending pins the batch a
// flush is allowed to claim: a note is either named whole in the prompt that
// carries it, or left pending for a later batch. Truncating the prompt while
// settling every claimed note would silently lose notes.
func TestFlushInboxDeliversEveryClaimedNoteAndLeavesTheRestPending(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	const notes = 25
	for i := 1; i <= notes; i++ {
		f.queueNote(t, worker, "progress", fmt.Sprintf("step %02d", i), "worker progress")
	}
	pending, err := f.store.PendingInbox(ctx, f.projectKey())
	if err != nil {
		t.Fatalf("PendingInbox: %v", err)
	}
	if len(pending) != notes {
		t.Fatalf("pending notes = %d, want %d", len(pending), notes)
	}

	if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
		t.Fatalf("FlushInbox: %v", err)
	}
	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	text := prompts[0].Text
	delivered, remaining := f.notesByState(t, task.DeliveryDelivered), f.notesByState(t, task.DeliveryPending)
	if len(delivered) != inboxPromptNoteLimit {
		t.Fatalf("delivered notes = %d, want the %d note batch", len(delivered), inboxPromptNoteLimit)
	}
	if len(remaining) != notes-len(delivered) {
		t.Fatalf("pending notes after the flush = %d, want %d", len(remaining), notes-len(delivered))
	}
	for _, note := range delivered {
		if !strings.Contains(text, note.Summary) {
			t.Fatalf("the prompt dropped the delivered note %q: %s", note.Summary, text)
		}
	}
	for _, note := range remaining {
		if strings.Contains(text, note.Summary) {
			t.Fatalf("the prompt named %q but its note was left pending", note.Summary)
		}
	}

	// The next flush carries the rest, and no note is ever delivered twice.
	f.setAgentStatus(t, primary, "idle")
	if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
		t.Fatalf("second FlushInbox: %v", err)
	}
	if got := len(f.notesByState(t, task.DeliveryPending)); got != 0 {
		t.Fatalf("pending notes after the second flush = %d, want 0", got)
	}
	seen := 0
	for _, prompt := range f.promptsTo(t, primary) {
		for i := 1; i <= notes; i++ {
			if strings.Contains(prompt.Text, fmt.Sprintf("step %02d", i)) {
				seen++
			}
		}
	}
	if seen != notes {
		t.Fatalf("the prompts named %d of %d notes, want every note exactly once", seen, notes)
	}
}

// TestFlushInboxBatchesBySizeAndLeavesTheRestPending is the byte-bound half of
// the same rule: the batch is chosen so the whole prompt fits, not truncated
// after the notes were already claimed.
func TestFlushInboxBatchesBySizeAndLeavesTheRestPending(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	const notes = 12
	summary := strings.Repeat("x", 400)
	for i := range notes {
		f.queueNote(t, worker, "progress", fmt.Sprintf("%02d %s", i, summary), "worker progress")
	}
	if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
		t.Fatalf("FlushInbox: %v", err)
	}
	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	first := len(f.notesByState(t, task.DeliveryDelivered))
	if first == 0 || first >= notes {
		t.Fatalf("first batch delivered %d of %d notes, want a partial batch", first, notes)
	}
	if len(prompts[0].Text) > inboxPromptLimit {
		t.Fatalf("prompt is %d bytes, larger than the %d byte bound", len(prompts[0].Text), inboxPromptLimit)
	}
	// Draining the queue needs more batches, and every note is named once.
	for i := 0; i < notes && len(f.notesByState(t, task.DeliveryPending)) > 0; i++ {
		f.setAgentStatus(t, primary, "idle")
		if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
			t.Fatalf("FlushInbox %d: %v", i, err)
		}
	}
	if got := len(f.notesByState(t, task.DeliveryPending)); got != 0 {
		t.Fatalf("pending notes after draining = %d, want 0", got)
	}
	named := 0
	for i := range notes {
		marker := fmt.Sprintf("%02d x", i)
		appearances := 0
		for _, prompt := range f.promptsTo(t, primary) {
			if strings.Contains(prompt.Text, marker) {
				appearances++
			}
		}
		if appearances != 1 {
			t.Fatalf("note %d was named in %d prompts, want exactly 1", i, appearances)
		}
		named++
	}
	if named != notes {
		t.Fatalf("named %d of %d notes", named, notes)
	}
}

// TestMoveRequiresALiveProofOfTheRecordedSession pins that a public pane rebind
// is committed only for a pane Herdr itself proves is the recorded worker.
// Trusting the move payload alone would accept a pane the event describes as
// ours even when the live pane holds a different session.
func TestMoveRequiresALiveProofOfTheRecordedSession(t *testing.T) {
	t.Run("the payload carries no session but the live pane proves it", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", worker.OMPSessionID)

		payload := movePayload(t, worker, "w9:p1", "w9")
		delete(payload["pane"].(map[string]any), "agent_session")
		f.mustHandle(t, EventPaneMoved, encodeEvent(t, payload))

		after := f.taskByID(t, worker.ID)
		if after.PublicPaneID != "w9:p1" || after.CurrentWorkspaceID != "w9" {
			t.Fatalf("a live-proven move was not recorded: pane %q workspace %q", after.PublicPaneID, after.CurrentWorkspaceID)
		}
		if after.LaunchPaneID != worker.LaunchPaneID || after.TerminalID != worker.TerminalID {
			t.Fatalf("the move re-keyed launch %q terminal %q", after.LaunchPaneID, after.TerminalID)
		}
	})

	t.Run("the payload carries no session and the live pane differs", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", "someone-elses-session")

		payload := movePayload(t, worker, "w9:p1", "w9")
		delete(payload["pane"].(map[string]any), "agent_session")
		err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
		if !errors.Is(err, ErrAgentChanged) {
			t.Fatalf("err = %v, want ErrAgentChanged", err)
		}
		assertMoveRefused(t, f, worker)
	})

	t.Run("the payload claims the recorded session but the live pane differs", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", "someone-elses-session")

		payload := movePayload(t, worker, "w9:p1", "w9")
		err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
		if !errors.Is(err, ErrAgentChanged) {
			t.Fatalf("err = %v, want ErrAgentChanged", err)
		}
		assertMoveRefused(t, f, worker)
	})

	t.Run("the live terminal is not the recorded one", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", worker.OMPSessionID)
		f.rewriteState(func(current *fixtureState) {
			// The same session and checkout, but a different terminal: the pane is
			// not the process this task was dispatched with.
			current.Agents["w9:p1"].Terminal = "term-somewhere-else"
		})

		payload := movePayload(t, worker, "w9:p1", "w9")
		err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
		if !errors.Is(err, ErrAgentChanged) {
			t.Fatalf("err = %v, want ErrAgentChanged", err)
		}
		assertMoveRefused(t, f, worker)
	})

	t.Run("the live pane reports no checkout directory", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", worker.OMPSessionID)
		f.updatePlan(func(plan *fixturePlan) { plan.BlankCwdFor = "w9:p1" })

		payload := movePayload(t, worker, "w9:p1", "w9")
		err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
		if !errors.Is(err, ErrAgentChanged) {
			t.Fatalf("err = %v, want ErrAgentChanged", err)
		}
		assertMoveRefused(t, f, worker)
	})

	t.Run("the live pane works in a different checkout", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", worker.OMPSessionID)
		f.rewriteState(func(current *fixtureState) {
			// The moved workspace holds the repository root, not the task's
			// worktree.
			current.Workspaces["w9"] = f.root
		})

		payload := movePayload(t, worker, "w9:p1", "w9")
		err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
		if !errors.Is(err, ErrAgentChanged) {
			t.Fatalf("err = %v, want ErrAgentChanged", err)
		}
		assertMoveRefused(t, f, worker)
	})

	t.Run("the recorded terminal and checkout are proven live", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		f.movedWorkerState(t, worker, "w9:p1", "w9", worker.OMPSessionID)

		f.mustHandle(t, EventPaneMoved, encodeEvent(t, movePayload(t, worker, "w9:p1", "w9")))
		after := f.taskByID(t, worker.ID)
		if after.PublicPaneID != "w9:p1" || after.CurrentWorkspaceID != "w9" {
			t.Fatalf("a fully proven move was not recorded: pane %q workspace %q", after.PublicPaneID, after.CurrentWorkspaceID)
		}
		if after.LaunchPaneID != worker.LaunchPaneID || after.TerminalID != worker.TerminalID {
			t.Fatalf("the move re-keyed launch %q terminal %q", after.LaunchPaneID, after.TerminalID)
		}
	})
}

// assertMoveRefused checks the durable outcome of a refused move: the binding is
// untouched, a human is told, and nothing was started in the pane.
func assertMoveRefused(t *testing.T, f *coordFixture, worker task.Task) {
	t.Helper()
	after := f.taskByID(t, worker.ID)
	if after.PublicPaneID != worker.PublicPaneID || after.CurrentWorkspaceID != worker.CurrentWorkspaceID {
		t.Fatalf("a refused move re-keyed the task: pane %q workspace %q", after.PublicPaneID, after.CurrentWorkspaceID)
	}
	if after.Attention == "" {
		t.Fatalf("a refused move left no attention for a human")
	}
	if stage := task.StageOf(after); stage != task.NeedsInput {
		t.Fatalf("stage = %q, want %q", stage, task.NeedsInput)
	}
	if starts := f.countCalls("agent start"); starts != 1 {
		t.Fatalf("agent start calls = %d, want only the dispatch's own (%v)", starts, tasksStarted(f))
	}
}

// TestReconcileTreatsAMissingSessionReferenceAsUnproven pins the probe rule: a
// recorded session that Herdr does not report back cannot be proven, even on the
// terminal the task was dispatched with. Adopting such a pane would let an
// unrelated OMP session inherit a worker's task.
func TestReconcileTreatsAMissingSessionReferenceAsUnproven(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	f.updatePlan(func(plan *fixturePlan) { plan.SuppressSessionFor = worker.PublicPaneID })

	identity, err := f.coord.identityOf(ctx, worker.LaunchPaneID, worker.OMPSessionID)
	if err != nil {
		t.Fatalf("identityOf: %v", err)
	}
	probe, err := f.coord.probePane(ctx, worker.PublicPaneID, identity, worker.TerminalID, worker.WorktreePath, false)
	if err != nil {
		t.Fatalf("probePane: %v", err)
	}
	switch {
	case probe.Ours:
		t.Fatalf("a pane reporting no session reference was accepted as the worker")
	case probe.Refutes:
		t.Fatalf("an unpublished reference is not a contradiction, it is unproven")
	case probe.Vacant:
		t.Fatalf("the pane holds an agent, so it is not vacant")
	}

	if err := f.coord.Reconcile(ctx); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.Attention == "" {
		t.Fatalf("an unprovable worker left no attention for a human")
	}
	if after.PublicPaneID != worker.PublicPaneID || after.LaunchPaneID != worker.LaunchPaneID {
		t.Fatalf("an unprovable worker was re-keyed: launch %q public %q", after.LaunchPaneID, after.PublicPaneID)
	}
	if starts := f.countCalls("agent start"); starts != 1 {
		t.Fatalf("agent start calls = %d, want only the dispatch's own", starts)
	}

	// The acceptance case is unchanged: once Herdr reports the recorded session,
	// the same pane is proven and reconciles silently.
	f.updatePlan(func(plan *fixturePlan) { plan.SuppressSessionFor = "" })
	restored, err := f.coord.identityOf(ctx, worker.LaunchPaneID, worker.OMPSessionID)
	if err != nil {
		t.Fatalf("identityOf after the reference returned: %v", err)
	}
	proven, err := f.coord.probePane(ctx, worker.PublicPaneID, restored, worker.TerminalID, worker.WorktreePath, false)
	if err != nil {
		t.Fatalf("probePane after the reference returned: %v", err)
	}
	if !proven.Ours || !proven.SessionOK {
		t.Fatalf("the recorded session was not accepted: ours=%v sessionOK=%v refusal=%q", proven.Ours, proven.SessionOK, proven.Refusal)
	}
}

// TestEventPayloadSizeIsCheckedBeforeTrimming pins that the bound applies to the
// bytes Herdr delivered, not to what is left after whitespace was stripped: a
// payload of mostly legal JSON whitespace with a small valid object inside must
// be refused whole.
func TestEventPayloadSizeIsCheckedBeforeTrimming(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	inner := `{"type":"pane_agent_status_changed","pane_id":"` + worker.PublicPaneID +
		`","workspace_id":"` + workspaceOfPane(worker.PublicPaneID) + `","agent_status":"idle"}`

	oversized := inner + strings.Repeat(" ", MaxEventPayloadBytes)
	err := f.coord.HandleEvent(context.Background(), EventAgentStatus, []byte(oversized))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized payload err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized payload error = %q, want the size refusal", err)
	}

	whitespace := strings.Repeat(" ", 64)
	err = f.coord.HandleEvent(context.Background(), EventAgentStatus, []byte(whitespace))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("whitespace-only payload err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "no payload") {
		t.Fatalf("whitespace-only payload error = %q, want the empty-payload refusal", err)
	}

	// A payload at the bound passes the size gate and is applied: the padding is
	// legal JSON whitespace around a real event.
	atBound := inner + strings.Repeat(" ", MaxEventPayloadBytes-len(inner))
	if err := f.coord.HandleEvent(context.Background(), EventAgentStatus, []byte(atBound)); err != nil {
		t.Fatalf("payload at the %d byte bound err = %v", MaxEventPayloadBytes, err)
	}
	if after := f.taskByID(t, worker.ID); after.Activity != "idle" {
		t.Fatalf("activity = %q, want the padded payload to have been applied", after.Activity)
	}
}

// notesByState lists the project's notes in one delivery state.
func (f *coordFixture) notesByState(t *testing.T, state string) []task.Inbox {
	t.Helper()
	var out []task.Inbox
	for _, note := range f.inboxRows(t) {
		if note.State() == state {
			out = append(out, note)
		}
	}
	return out
}

// tasksStarted names the `agent start` argv the stand-in recorded.
func tasksStarted(f *coordFixture) []string {
	var out []string
	for _, call := range f.calls() {
		if len(call.Argv) >= 2 && call.Argv[0]+" "+call.Argv[1] == "agent start" {
			out = append(out, strings.Join(call.Argv, " "))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// cross-form session identity (found by the real Task 8 run)
// ---------------------------------------------------------------------------

// crossFormFixture is the identity shape a real Herdr session has: the bridge
// records the OMP native session id for a worker, while Herdr's agent
// integration reports the same session as its file path (kind "path"). Both are
// identities of one session, so every path that proves a worker must accept the
// pair.
func crossFormFixture(t *testing.T) *coordFixture {
	t.Helper()
	return newCoordFixture(t, fixturePlan{SessionRefKind: task.SessionKindPath, AdoptBeforeRecord: true})
}

// assertCrossFormRecorded pins the shape these regressions depend on: the durable
// binding holds the native id, and Herdr reports that session as a path.
func assertCrossFormRecorded(t *testing.T, f *coordFixture, st task.Task) string {
	t.Helper()
	agent := f.state().Agents[st.PublicPaneID]
	if agent == nil {
		t.Fatalf("no agent is running in pane %q", st.PublicPaneID)
	}
	if st.OMPSessionID != agent.Session {
		t.Fatalf("recorded session = %q, want the native id %q", st.OMPSessionID, agent.Session)
	}
	if strings.ContainsAny(st.OMPSessionID, `/\`) {
		t.Fatalf("recorded session %q looks like a path, want the native id", st.OMPSessionID)
	}
	return sessionFileFor(agent.Session)
}

func TestEventStatusAcceptsTheRecordedIDWhenHerdrReportsThePath(t *testing.T) {
	f := crossFormFixture(t)
	worker := f.boundWorker(t)
	file := assertCrossFormRecorded(t, f, worker)

	f.setAgentStatus(t, worker.PublicPaneID, "blocked")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, worker.PublicPaneID, "blocked"))
	after := f.taskByID(t, worker.ID)
	if after.Activity != "blocked" {
		t.Fatalf("activity = %q, want %q: the session was recorded as the id %q and reported as the path %q",
			after.Activity, "blocked", worker.OMPSessionID, file)
	}
	if after.Attention != "" {
		t.Fatalf("attention = %q, want none for one session in two forms", after.Attention)
	}
	if notes := f.notesOfKind(t, noteKindActivity); len(notes) != 1 {
		t.Fatalf("activity notes = %d, want 1", len(notes))
	}
}

func TestMoveAcceptsTheRecordedIDWhenHerdrReportsThePath(t *testing.T) {
	f := crossFormFixture(t)
	worker := f.boundWorker(t)
	assertCrossFormRecorded(t, f, worker)

	move := f.moveResult(t, worker.PublicPaneID)
	newPane, _ := move["pane"].(map[string]any)["pane_id"].(string)
	f.mustHandle(t, EventPaneMoved, eventFromMoveResult(t, move))
	after := f.taskByID(t, worker.ID)
	if after.PublicPaneID != newPane || after.CurrentWorkspaceID == worker.CurrentWorkspaceID {
		t.Fatalf("the move was not recorded: pane %q workspace %q", after.PublicPaneID, after.CurrentWorkspaceID)
	}
	if after.Attention != "" || after.Blocker != "" {
		t.Fatalf("attention = %q blocker = %q, want none for the same session in two forms", after.Attention, after.Blocker)
	}
	if after.LaunchPaneID != worker.LaunchPaneID || after.TerminalID != worker.TerminalID {
		t.Fatalf("the move re-keyed launch %q terminal %q", after.LaunchPaneID, after.TerminalID)
	}
}

func TestReconcileAcceptsTheRecordedIDWhenHerdrReportsThePath(t *testing.T) {
	ctx := context.Background()
	f := crossFormFixture(t)
	worker := f.boundWorker(t)
	assertCrossFormRecorded(t, f, worker)

	// The pane moves and the event is lost; only Herdr's live state knows.
	move := f.moveResult(t, worker.PublicPaneID)
	newPane, _ := move["pane"].(map[string]any)["pane_id"].(string)
	if err := f.coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.PublicPaneID != newPane {
		t.Fatalf("public pane = %q, want %q", after.PublicPaneID, newPane)
	}
	if after.Attention != "" || after.Blocker != "" {
		t.Fatalf("attention = %q blocker = %q, want none for the same session in two forms", after.Attention, after.Blocker)
	}
	if after.LaunchPaneID != worker.LaunchPaneID {
		t.Fatalf("launch pane = %q, want the unchanged %q", after.LaunchPaneID, worker.LaunchPaneID)
	}
}

func TestFlushAcceptsTheRecordedIDWhenHerdrReportsThePath(t *testing.T) {
	ctx := context.Background()
	f := crossFormFixture(t)
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	assertCrossFormRecorded(t, f, worker)

	// A real note, queued without depending on the worker reporting path.
	note := f.coord.noteFor(worker, "progress", "halfway", "worker progress")
	if _, err := f.store.AddInbox(ctx, note); err != nil {
		t.Fatalf("AddInbox: %v", err)
	}
	if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
		t.Fatalf("FlushInbox: %v", err)
	}
	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1: the recorded primary id and the reported path name one session", len(prompts))
	}
	if !strings.Contains(prompts[0].Text, worker.ID) {
		t.Fatalf("prompt text = %q, want the worker's task id", prompts[0].Text)
	}
	if state := f.noteState(t, note.ID); state != task.DeliveryDelivered {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
	}
}

func TestCrossFormIdentityStillRefusesADifferentSession(t *testing.T) {
	ctx := context.Background()
	f := crossFormFixture(t)
	worker := f.boundWorker(t)
	assertCrossFormRecorded(t, f, worker)

	// A different session now runs in the worker's pane. Its path is just as
	// path-shaped as the recorded one's, so accepting a path because the binding
	// holds an id would hand this task to a stranger.
	f.rewriteState(func(current *fixtureState) {
		current.Agents[worker.PublicPaneID].Session = "someone-elses-session"
	})
	f.setAgentStatus(t, worker.PublicPaneID, "blocked")
	err := f.coord.HandleEvent(ctx, EventAgentStatus, statusEventPayload(t, worker.PublicPaneID, "blocked"))
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("status event err = %v, want ErrAgentChanged", err)
	}
	refused := f.taskByID(t, worker.ID)
	if refused.Attention == "" {
		t.Fatalf("a different session left no attention for a human")
	}
	if refused.Activity == "blocked" {
		t.Fatalf("a different session's status was recorded as the worker's")
	}

	if err := f.coord.Reconcile(ctx); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	if after := f.taskByID(t, worker.ID); after.PublicPaneID != worker.PublicPaneID {
		t.Fatalf("a different session re-keyed the task to %q", after.PublicPaneID)
	}
}

// TestSessionsAreProvenOnlyThroughAConfirmedHandshake pins the contract's second
// half: Store.AnnouncedSession and Store.Ready are both required, so a pane whose
// confirmation no longer describes its announcement - or a pane that never
// announced - proves nothing and is refused instead of adopted.
func TestSessionsAreProvenOnlyThroughAConfirmedHandshake(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)

	// The pane announces a different session, and Herdr has not confirmed that
	// announcement: the recorded confirmation no longer describes it.
	if err := f.store.RecordReady(ctx, worker.LaunchPaneID,
		task.Announcement{ID: "reannounced", File: sessionFileFor("reannounced")}, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	identity, err := f.coord.identityOf(ctx, worker.LaunchPaneID, worker.OMPSessionID)
	if err != nil {
		t.Fatalf("identityOf: %v", err)
	}
	if identity.Proven {
		t.Fatalf("an unconfirmed announcement proved session %q", worker.OMPSessionID)
	}

	f.setAgentStatus(t, worker.PublicPaneID, "blocked")
	err = f.coord.HandleEvent(ctx, EventAgentStatus, statusEventPayload(t, worker.PublicPaneID, "blocked"))
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("status event err = %v, want ErrAgentChanged", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.Activity == "blocked" {
		t.Fatalf("a status observation was recorded from a session that could not be proven")
	}
	if after.Attention == "" {
		t.Fatalf("an unprovable session left no attention for a human")
	}

	// A launch pane that never announced at all proves nothing either.
	unknown, err := f.coord.identityOf(ctx, "w9:p9", worker.OMPSessionID)
	if err != nil {
		t.Fatalf("identityOf(unknown pane): %v", err)
	}
	if unknown.Proven || unknown.proves(task.SessionRef{Kind: task.SessionKindID, Value: worker.OMPSessionID}) {
		t.Fatalf("a pane that never announced a session proved one")
	}
}

// TestRefreshedAnnouncementCannotBridgeAStaleIDToACurrentPane pins the
// announcement/readiness window: reading the announcement and the confirmation as
// two queries let a concurrent re-announcement refresh the row in between, so a
// pair of an earlier id and a later confirmation could be assembled and could
// accept the pane Herdr reports now. The verified handshake is read in one store
// read, so the identity a caller holds is always the row that is actually
// confirmed.
//
// The interleaving itself is not drivable from a test (it happens between two
// store calls inside one function); it was demonstrated with a probe whose output
// is recorded in the report. This test pins the consequence an interleaving would
// have produced.
func TestRefreshedAnnouncementCannotBridgeAStaleIDToACurrentPane(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	agent := f.state().Agents[worker.PublicPaneID]
	file := sessionFileFor(agent.Session)

	// The pane re-announces the same session file under a new id, and Herdr
	// confirms that refreshed pair by path (the confirmed path still matches).
	if err := f.store.RecordReady(ctx, worker.LaunchPaneID,
		task.Announcement{ID: "omp-session-refreshed", File: file}, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	if err := f.store.ConfirmReady(ctx, worker.LaunchPaneID,
		task.SessionRef{Kind: task.SessionKindPath, Value: file}); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}

	// The refreshed pane Herdr reports now.
	current := task.SessionRef{Kind: task.SessionKindPath, Value: file}

	// A binding that recorded the id the row no longer carries must not be able to
	// claim that pane: the stale id and the shared file are not one session.
	stale, err := f.coord.identityOf(ctx, worker.LaunchPaneID, worker.OMPSessionID)
	if err != nil {
		t.Fatalf("identityOf: %v", err)
	}
	if stale.proves(current) {
		t.Fatalf("the stale id %q was bridged to the refreshed pane via the shared file", worker.OMPSessionID)
	}
	if !stale.Proven {
		// Either answer is safe; failing to prove keeps the binding on attention.
		t.Logf("the refreshed row no longer confirms the stale id %q (unproven, as intended)", worker.OMPSessionID)
	}

	// The row's own identities still prove the pane it confirms.
	fresh, err := f.coord.identityOf(ctx, worker.LaunchPaneID, "omp-session-refreshed")
	if err != nil {
		t.Fatalf("identityOf(refreshed): %v", err)
	}
	if !fresh.Proven || !fresh.proves(task.SessionRef{Kind: task.SessionKindID, Value: "omp-session-refreshed"}) {
		t.Fatalf("the refreshed row did not prove its own id: proven=%v", fresh.Proven)
	}

	// And the identity a caller holds is exactly the row that is confirmed: one
	// read, so no earlier announcement can be paired with a later confirmation.
	confirmed, verified, err := f.store.VerifiedAnnouncement(ctx, worker.LaunchPaneID)
	if err != nil {
		t.Fatalf("VerifiedAnnouncement: %v", err)
	}
	if !verified {
		t.Fatalf("the refreshed row is not verified")
	}
	if fresh.Announced != confirmed {
		t.Fatalf("identityOf used %+v, but the confirmed row is %+v", fresh.Announced, confirmed)
	}
}

// ---------------------------------------------------------------------------
// note creation triggers delivery (found by the acceptance run)
// ---------------------------------------------------------------------------

// TestAWorkerNoteReachesAnAlreadyIdlePrimary pins the delivery trigger a worker's
// own write makes: a question recorded while the primary is already idle must
// reach it, because waiting for the primary's *next* status transition waits for
// an event that never comes.
func TestAWorkerNoteReachesAnAlreadyIdlePrimary(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("the primary was prompted %d times before the question", len(prompts))
	}

	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "which error contract should this use?"); err != nil {
		t.Fatalf("Question: %v", err)
	}
	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want the queued question delivered to the already-idle primary", len(prompts))
	}
	if !strings.Contains(prompts[0].Text, worker.ID) || !strings.Contains(prompts[0].Text, "which error contract") {
		t.Fatalf("prompt text = %q, want the task id and the question", prompts[0].Text)
	}
	note := f.requeue(t, worker, "question", "which error contract should this use?", "worker question")
	if state := f.noteState(t, note); state != task.DeliveryDelivered {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
	}
}

// TestAWorkerNoteStaysPendingWhileThePrimaryIsNotIdle is the other half: the
// trigger never sends to a primary that is not verified idle, and the note is
// delivered on the next safe turn.
func TestAWorkerNoteStaysPendingWhileThePrimaryIsNotIdle(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	f.setAgentStatus(t, primary, "blocked")

	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "which error contract should this use?"); err != nil {
		t.Fatalf("Question: %v", err)
	}
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("a blocked primary was prompted %d times, want 0", len(prompts))
	}
	note := f.requeue(t, worker, "question", "which error contract should this use?", "worker question")
	if state := f.noteState(t, note); state != task.DeliveryPending {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryPending)
	}

	// The next safe turn delivers it.
	f.setAgentStatus(t, primary, "idle")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "idle"))
	if prompts := f.promptsTo(t, primary); len(prompts) != 1 {
		t.Fatalf("prompts after the primary went idle = %d, want 1", len(prompts))
	}
	if state := f.noteState(t, note); state != task.DeliveryDelivered {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
	}
}

// TestADuplicateWorkerNoteIsNeverDeliveredTwice pins the trigger's interaction
// with the note deduplication FinalReport's semantics depend on: re-attempting a
// note that is already durable may deliver it once, never twice.
func TestADuplicateWorkerNoteIsNeverDeliveredTwice(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID

	for i := 1; i <= 2; i++ {
		if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "same question"); err != nil {
			t.Fatalf("Question %d: %v", i, err)
		}
		notes := f.notesOfKind(t, "question")
		if len(notes) != 1 {
			t.Fatalf("question notes = %d, want the one durable note", len(notes))
		}
		if state := f.noteState(t, notes[0].ID); state != task.DeliveryDelivered {
			t.Fatalf("note state after attempt %d = %q, want %q", i, state, task.DeliveryDelivered)
		}
		if prompts := f.promptsTo(t, primary); len(prompts) != 1 {
			t.Fatalf("prompts after attempt %d = %d, want exactly 1", i, len(prompts))
		}
	}
}

// TestAWorkerNoteReachesAnIdlePrimaryAcrossSessionForms is the acceptance
// sequence run 2 traced: the note is recorded while the primary is *already*
// idle, so no status transition will ever trigger a flush, and the primary's
// recorded session is its native id while Herdr reports the session file. The
// note's own write must deliver it through the verified cross-form handshake:
// observe the primary, prompt it, and settle the delivery.
func TestAWorkerNoteReachesAnIdlePrimaryAcrossSessionForms(t *testing.T) {
	ctx := context.Background()
	f := crossFormFixture(t)
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	assertCrossFormRecorded(t, f, worker)
	if recorded := f.boundProject(t).OMPSessionID; strings.ContainsAny(recorded, `/\`) {
		t.Fatalf("the primary recorded %q, want the native id Herdr announced", recorded)
	}

	// The primary has been idle since before the note existed: nothing changes its
	// status afterwards, so the note's own write is the only opportunity.
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("the primary was prompted %d times with nothing queued", len(prompts))
	}
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "which error contract should this use?"); err != nil {
		t.Fatalf("Question: %v", err)
	}

	prompts := f.promptsTo(t, primary)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want the queued question delivered through the verified cross-form handshake", len(prompts))
	}
	if !strings.Contains(prompts[0].Text, worker.ID) || !strings.Contains(prompts[0].Text, "which error contract") {
		t.Fatalf("prompt text = %q, want the task id and the question", prompts[0].Text)
	}
	note := f.requeue(t, worker, "question", "which error contract should this use?", "worker question")
	if state := f.noteState(t, note); state != task.DeliveryDelivered {
		t.Fatalf("note state = %q, want %q", state, task.DeliveryDelivered)
	}

	// The delivery is settled, so the next idle observation submits nothing again.
	f.setAgentStatus(t, primary, "idle")
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "idle"))
	if prompts := f.promptsTo(t, primary); len(prompts) != 1 {
		t.Fatalf("prompts after the next idle observation = %d, want 1", len(prompts))
	}
}

// ---------------------------------------------------------------------------
// startup recheck of queued work and interrupted steps
// ---------------------------------------------------------------------------

// TestReconcileProvisionsAQueuedTaskWhoseGateOpened pins the startup caller the
// spec's recheck contract needs: a task queued behind an unreviewed prerequisite
// becomes reachable at the moment the prerequisite is recorded, without another
// dispatch.
func TestReconcileProvisionsAQueuedTaskWhoseGateOpened(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	first, err := f.dispatchSpec(f.coord, spec{title: "first", checks: checks, reserved: []string{"src/first.txt"}})
	if err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	queued, err := f.dispatchSpec(f.coord, spec{
		title: "second", checks: checks, deps: []string{first.ID}, reserved: []string{"src/second.txt"},
	})
	if err != nil {
		t.Fatalf("dispatch second: %v", err)
	}
	if queued.Phase != task.Intent {
		t.Fatalf("phase = %q, want %q: an unreviewed prerequisite must queue the task", queued.Phase, task.Intent)
	}
	created := f.countCalls("worktree create")

	// The prerequisite is reported, which opens the gate. Nothing dispatches again.
	f.satisfyReview(first)
	if err := f.coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after := f.taskByID(t, queued.ID)
	if after.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q: a queued task must be reachable after a restart", after.Phase, task.PromptSubmitted)
	}
	if after.WorktreePath == "" || after.PublicPaneID == "" || after.AgentName == "" {
		t.Fatalf("the drained task records no resources: %+v", after)
	}
	if got := f.countCalls("worktree create"); got != created+1 {
		t.Fatalf("worktree create calls = %d, want %d (one for the drained task)", got, created+1)
	}
	if prompts := f.prompts(after.PublicPaneID); len(prompts) != 1 {
		t.Fatalf("briefs = %v, want exactly one", prompts)
	}
}

// TestReconcileLeavesAQueuedTaskWhoseGateIsStillClosed is the guard on that
// caller: a queued task whose prerequisite is not reviewed yet, or whose budget
// is full, is not admitted by a restart.
func TestReconcileLeavesAQueuedTaskWhoseGateIsStillClosed(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	first, err := f.dispatchSpec(f.coord, spec{title: "first", checks: checks, reserved: []string{"src/first.txt"}})
	if err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	queued, err := f.dispatchSpec(f.coord, spec{
		title: "second", checks: checks, deps: []string{first.ID}, reserved: []string{"src/second.txt"},
	})
	if err != nil {
		t.Fatalf("dispatch second: %v", err)
	}
	created := f.countCalls("worktree create")

	if err := f.coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	after := f.taskByID(t, queued.ID)
	if after.Phase != task.Intent || after.WorktreePath != "" || after.PublicPaneID != "" {
		t.Fatalf("a still-gated task was provisioned: %+v", after)
	}
	if got := f.countCalls("worktree create"); got != created {
		t.Fatalf("worktree create calls = %d, want %d", got, created)
	}
}

// TestReconcileCompletesAnInterruptedProvisioningStep pins the other half of the
// recheck: a step that stopped before the brief is continued by the next startup,
// from the recorded phase, without repeating anything.
//
// This coordinator takes a short step lease on purpose: the failed attempt left its
// durable step claim behind (today only the worktree-create path releases it), and
// a resumed step may not steal a live claim. Recovery after a crash is therefore
// gated by that lease, which is what this test waits out.
func TestReconcileCompletesAnInterruptedProvisioningStep(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{SuppressReady: true})
	coord := f.shortLeaseCoordinator(t)
	worker, err := coord.Dispatch(ctx, f.projectKey(), "interrupted", "brief interrupted",
		[]string{"go test ./... ok"}, nil, nil, f.base)
	if err == nil {
		t.Fatalf("dispatch succeeded, want the readiness wait to stop the step")
	}
	interrupted := f.taskByID(t, worker.ID)
	if interrupted.Phase != task.AgentStarted {
		t.Fatalf("phase = %q, want %q", interrupted.Phase, task.AgentStarted)
	}
	if prompts := f.prompts(interrupted.PublicPaneID); len(prompts) != 0 {
		t.Fatalf("briefs = %v, want none before the interrupted step is continued", prompts)
	}

	// The cause is fixed and the failed attempt's claim has expired: a restart
	// finishes the recorded step.
	f.updatePlan(func(plan *fixturePlan) { plan.SuppressReady = false })
	time.Sleep(150 * time.Millisecond)
	if err := coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The interrupted attempt already tried to start an agent and was refused; the
	// resumed step starts the one it owes.
	assertStepCompleted(t, f, worker.ID, 1, 1)
}

// shortLeaseCoordinator builds a second coordinator over the fixture's store and
// Herdr stand-in with a short step lease, so a test that continues a step a failed
// attempt left behind does not wait out the default two-minute lease.
func (f *coordFixture) shortLeaseCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	coord, err := New(f.store, f.client, Options{
		Concurrency:      3,
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
		StepLease:        50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New (short lease): %v", err)
	}
	return coord
}

// assertStepCompleted checks the durable outcome of continuing an interrupted
// step: the recorded phase reached its end, exactly briefs briefs were submitted,
// and no step ran more often than the interrupted attempt plus this one.
func assertStepCompleted(t *testing.T, f *coordFixture, taskID string, briefs, starts int) {
	t.Helper()
	after := f.taskByID(t, taskID)
	if after.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q: the recorded step must be continued", after.Phase, task.PromptSubmitted)
	}
	if after.Attention != "" || after.Blocker != "" {
		t.Fatalf("attention = %q blocker = %q, want a completed step", after.Attention, after.Blocker)
	}
	if after.OMPSessionID == "" {
		t.Fatalf("the completed step recorded no session")
	}
	if prompts := f.prompts(after.PublicPaneID); len(prompts) != briefs {
		t.Fatalf("briefs = %v, want exactly %d", prompts, briefs)
	}
	if got := f.countCalls("agent start"); got != starts {
		t.Fatalf("agent start calls = %d, want %d: the recorded step starts no agent the attempt did not owe", got, starts)
	}
}

// TestReconcileLeavesAnUnprovenInterruptedStepAlone pins the gate on continuing a
// step: only a pane this pass proved may be touched, so a stranger's OMP session
// is never adopted - and the brief the recorded phase still owes is not sent to it.
func TestReconcileLeavesAnUnprovenInterruptedStepAlone(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	pane := worker.PublicPaneID
	// The recorded phase says the brief was never submitted; a different session
	// now holds the pane.
	f.block(worker, func(next *task.Task) { next.Phase = task.AgentStarted })
	f.rewriteState(func(current *fixtureState) {
		current.Agents[pane].Session = "someone-elses-session"
	})
	before := len(f.promptsTo(t, pane))

	if err := f.coord.Reconcile(ctx); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.Phase != task.AgentStarted {
		t.Fatalf("phase = %q, want the recorded %q", after.Phase, task.AgentStarted)
	}
	if after.Attention == "" {
		t.Fatalf("an unprovable step left no attention for a human")
	}
	if got := len(f.promptsTo(t, pane)); got != before {
		t.Fatalf("the owed brief was submitted to a pane this pass could not prove (%d -> %d)", before, got)
	}
	if starts := f.countCalls("agent start"); starts != 1 {
		t.Fatalf("agent start calls = %d, want only the dispatch's own", starts)
	}
}

// TestReconcileLeavesAnInterruptedStepWithNoSessionOnAStrangeTerminalAlone pins
// the terminal rule a resume must not lean on: when the binding recorded no
// session at all, only the recorded terminal proves the pane, so an agent on a
// different terminal is never adopted.
func TestReconcileLeavesAnInterruptedStepWithNoSessionOnAStrangeTerminalAlone(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	f.block(worker, func(next *task.Task) {
		next.Phase = task.AgentStarted
		next.OMPSessionID = ""
	})
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[worker.PublicPaneID]
		delete(current.Agents, worker.PublicPaneID)
		delete(current.Workspaces, workspaceOfPane(worker.PublicPaneID))
		agent.Terminal = "term-somewhere-else"
		current.Workspaces["w9"] = worker.WorktreePath
		current.Agents["w9:p1"] = agent
	})
	before := len(f.promptsTo(t, worker.PublicPaneID))

	if err := f.coord.Reconcile(ctx); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.Phase != task.AgentStarted {
		t.Fatalf("phase = %q, want the recorded %q", after.Phase, task.AgentStarted)
	}
	if after.PublicPaneID != worker.PublicPaneID {
		t.Fatalf("a pane with no recorded session was re-keyed to %q", after.PublicPaneID)
	}
	if got := len(f.promptsTo(t, worker.PublicPaneID)); got != before {
		t.Fatalf("a brief was submitted to an unprovable pane (%d -> %d)", before, got)
	}
}

// TestReconcileDoesNotAdmitQueuedWorkAfterAnIdentityFinding pins the review's
// second finding: the drain runs only for a project whose identities this pass
// stood behind, so a project with an unreconciled worker does not create new
// worktrees and workers on top of a ledger nobody verified.
func TestReconcileDoesNotAdmitQueuedWorkAfterAnIdentityFinding(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	var workers []task.Task
	for i, title := range []string{"one", "two", "three"} {
		st, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}})
		if err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
		workers = append(workers, st)
	}
	// The budget is full, so this one queues - with no dependency, so nothing but
	// the budget holds it.
	queued, err := f.dispatchSpec(f.coord, spec{title: "four", checks: checks, reserved: []string{"src/four.txt"}})
	if err != nil {
		t.Fatalf("dispatch four: %v", err)
	}
	if queued.Phase != task.Intent {
		t.Fatalf("phase = %q, want %q with the budget full", queued.Phase, task.Intent)
	}
	// A slot frees, so the queue is now drainable - but another worker's identity no
	// longer matches what Herdr reports.
	f.block(workers[0], func(next *task.Task) { next.Disposition = "integrated" })
	f.rewriteState(func(current *fixtureState) {
		current.Agents[workers[1].PublicPaneID].Session = "someone-elses-session"
	})
	created := f.countCalls("worktree create")

	if err := f.coord.Reconcile(ctx); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, queued.ID)
	if after.Phase != task.Intent || after.WorktreePath != "" || after.PublicPaneID != "" {
		t.Fatalf("a project whose identity this pass could not repair admitted queued work: %+v", after)
	}
	if got := f.countCalls("worktree create"); got != created {
		t.Fatalf("worktree create calls = %d, want %d: no work is admitted on an unverified ledger", got, created)
	}
}

// TestReconcileNeverAdoptsAnUnrecordedAgentForAnInterruptedStep pins the review's
// first finding: when the binding recorded no session (the interrupted start), the
// pane's terminal alone must not let a resume adopt whatever agent runs there,
// because that agent would receive the task's brief.
func TestReconcileNeverAdoptsAnUnrecordedAgentForAnInterruptedStep(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{SuppressSession: true})
	coord := f.shortLeaseCoordinator(t)
	worker, err := coord.Dispatch(ctx, f.projectKey(), "interrupted", "brief interrupted",
		[]string{"go test ./... ok"}, nil, nil, f.base)
	if err == nil {
		t.Fatalf("dispatch succeeded, want the readiness wait to stop the step")
	}
	interrupted := f.taskByID(t, worker.ID)
	if interrupted.Phase != task.WorktreeCreated || interrupted.OMPSessionID != "" {
		t.Fatalf("interrupted state = phase %q session %q, want worktree_created with no session",
			interrupted.Phase, interrupted.OMPSessionID)
	}
	// The agent that runs there belongs to a session this binding never recorded.
	f.rewriteState(func(current *fixtureState) {
		current.Agents[interrupted.PublicPaneID].Session = "someone-elses-session"
	})
	before := len(f.promptsTo(t, interrupted.PublicPaneID))

	f.updatePlan(func(plan *fixturePlan) { plan.SuppressSession = false })
	time.Sleep(150 * time.Millisecond)
	if err := coord.Reconcile(ctx); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.Phase != task.WorktreeCreated {
		t.Fatalf("phase = %q, want the recorded %q", after.Phase, task.WorktreeCreated)
	}
	if after.Attention == "" {
		t.Fatalf("an agent the binding never recorded left no attention for a human")
	}
	if got := len(f.promptsTo(t, interrupted.PublicPaneID)); got != before {
		t.Fatalf("the brief was submitted to an agent this binding never recorded (%d -> %d)", before, got)
	}
	if after.OMPSessionID != "" {
		t.Fatalf("a stranger's session was recorded as task %s's worker: %q", after.ID, after.OMPSessionID)
	}
}

// TestReconcileStartsTheAgentOfAnInterruptedStepInItsOwnEmptyPane is the safe half
// of the same rule: an interrupted step whose pane is empty holds nothing to adopt,
// so a restart starts the agent the task never got.
func TestReconcileStartsTheAgentOfAnInterruptedStepInItsOwnEmptyPane(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	coord := f.shortLeaseCoordinator(t)
	worker, err := coord.Dispatch(ctx, f.projectKey(), "interrupted", "brief interrupted",
		[]string{"go test ./... ok"}, nil, nil, f.base)
	if err == nil {
		t.Fatalf("dispatch succeeded, want the refused agent start to stop the step")
	}
	interrupted := f.taskByID(t, worker.ID)
	if interrupted.Phase != task.WorktreeCreated || interrupted.PublicPaneID == "" {
		t.Fatalf("interrupted state = phase %q pane %q, want worktree_created on a recorded pane",
			interrupted.Phase, interrupted.PublicPaneID)
	}
	if interrupted.OMPSessionID != "" {
		t.Fatalf("session = %q, want none: the agent never started", interrupted.OMPSessionID)
	}

	f.updatePlan(func(plan *fixturePlan) { plan.AgentStartError = "" })
	time.Sleep(150 * time.Millisecond)
	if err := coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStepCompleted(t, f, worker.ID, 1, 2)
}

func TestReconcileAttentionNamesTheTaskAndKeepsAnExistingBlocker(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	blocked := f.block(worker, func(next *task.Task) {
		next.Blocker = "the dispatch step could not be identified"
	})

	// An unknown occupant arrives while the task already carries a blocker.
	oldPane := blocked.PublicPaneID
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[oldPane]
		if agent == nil {
			t.Fatalf("no agent in %q", oldPane)
		}
		delete(current.Agents, oldPane)
		delete(current.Workspaces, workspaceOfPane(oldPane))
		agent.Session = "someone-elses-session"
		agent.Terminal = "term-someone-else"
		current.Workspaces["other"] = blocked.WorktreePath
		current.Agents["other:p1"] = agent
	})
	if err := f.coord.Reconcile(context.Background()); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("Reconcile err = %v, want the recorded refusal", err)
	}
	after := f.taskByID(t, worker.ID)
	if after.Blocker != blocked.Blocker {
		t.Fatalf("blocker = %q, want the recorded %q", after.Blocker, blocked.Blocker)
	}
	if after.Attention != "" {
		t.Fatalf("attention = %q, want none while a blocker is recorded", after.Attention)
	}
	notes := f.notesOfKind(t, noteKindAttention)
	if len(notes) != 1 {
		t.Fatalf("attention notes = %d, want 1", len(notes))
	}
	if !strings.Contains(notes[0].Summary, worker.ID) {
		t.Fatalf("attention note = %q, want it to name the task", notes[0].Summary)
	}
}
