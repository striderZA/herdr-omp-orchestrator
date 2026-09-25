package board

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// This file tests the board against two kinds of real evidence:
//
//   - A real SQLite store the board only ever reads.
//   - A real child process: TestMain turns this test binary into a stand-in
//     Herdr CLI chosen by BOARD_HERDR_FIXTURE, so every Herdr call the board
//     makes (and every call it must not make) is exercised through exec and
//     recorded verbatim. Verification, refusals and the single focus command
//     are therefore asserted on argv, never on a mock's return value.

// ---------------------------------------------------------------------------
// fixture child process
// ---------------------------------------------------------------------------

const (
	fixtureEnv      = "BOARD_HERDR_FIXTURE"
	fixtureEnvDir   = "BOARD_HERDR_FIXTURE_DIR"
	fixturePlanName = "herdr.json"
	fixtureCalls    = "calls.jsonl"
)

// fixturePane is what the stand-in Herdr reports for one pane. A knob that is
// empty falls back to a healthy value, so a test only states what it breaks.
type fixturePane struct {
	PaneID       string `json:"pane_id"`
	TerminalID   string `json:"terminal_id"`
	WorkspaceID  string `json:"workspace_id"`
	TabID        string `json:"tab_id"`
	Cwd          string `json:"cwd"`
	AgentStatus  string `json:"agent_status"`
	SessionKind  string `json:"session_kind"`
	SessionValue string `json:"session_value"`
	// NoSession makes `agent get` report no native session reference, the way
	// the integration does before it has published one.
	NoSession bool `json:"no_session"`
	// Missing makes every read about this pane answer pane_not_found.
	Missing bool `json:"missing"`
	// FocusError makes `agent focus` refuse with that Herdr code.
	FocusError string `json:"focus_error"`
	// FocusPaneID overrides the pane the focus result reports.
	FocusPaneID string `json:"focus_pane_id"`
	// FocusResultType overrides the focus result type, so a version that only
	// acknowledges the focus can be exercised.
	FocusResultType string `json:"focus_result_type"`
}

type fixturePlan struct {
	Panes map[string]fixturePane `json:"panes"`
}

type fixtureCall struct {
	Argv []string `json:"argv"`
}

func TestMain(m *testing.M) {
	if os.Getenv(fixtureEnv) == "1" {
		os.Exit(runFixture())
	}
	os.Exit(m.Run())
}

func runFixture() int {
	dir := os.Getenv(fixtureEnvDir)
	if dir == "" {
		return 20
	}
	plan := fixturePlan{}
	if data, err := os.ReadFile(filepath.Join(dir, fixturePlanName)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "fixture: plan: %v\n", err)
			return 21
		}
	} else if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &plan); err != nil {
			fmt.Fprintf(os.Stderr, "fixture: plan: %v\n", err)
			return 22
		}
	}
	argv := append([]string(nil), os.Args[1:]...)
	if err := appendCall(dir, argv); err != nil {
		return 23
	}
	if len(argv) < 3 {
		return fixtureUnexpected(argv, "argv is too short")
	}
	switch argv[0] + " " + argv[1] {
	case "pane get", "agent get", "agent focus":
		return fixtureIdentity(plan, argv[0]+" "+argv[1], argv[2])
	default:
		return fixtureUnexpected(argv, "unsupported command")
	}
}

// fixtureIdentity answers the three Herdr commands the board may use, from one
// pane description.
func fixtureIdentity(plan fixturePlan, command, target string) int {
	pane, ok := plan.Panes[target]
	if !ok || pane.Missing {
		return fixtureError("pane_not_found", "pane "+target+" not found")
	}
	workspace := firstNonEmpty(pane.WorkspaceID, "w1")
	info := map[string]any{
		"pane_id":      firstNonEmpty(pane.PaneID, target),
		"terminal_id":  firstNonEmpty(pane.TerminalID, "term_0000000000000001"),
		"workspace_id": workspace,
		"tab_id":       firstNonEmpty(pane.TabID, workspace+":t1"),
		"cwd":          pane.Cwd,
		"agent":        "omp",
		"agent_status": firstNonEmpty(pane.AgentStatus, "working"),
		"focused":      command == "agent focus",
	}
	if !pane.NoSession && (pane.SessionValue != "" || pane.SessionKind != "") {
		info["agent_session"] = map[string]any{
			"source": "herdr:omp",
			"agent":  "omp",
			"kind":   firstNonEmpty(pane.SessionKind, task.SessionKindPath),
			"value":  pane.SessionValue,
		}
	}
	switch command {
	case "pane get":
		return fixtureResult(map[string]any{"type": "pane_info", "pane": info})
	case "agent focus":
		if pane.FocusError != "" {
			return fixtureError(pane.FocusError, "the focus was refused")
		}
		if pane.FocusResultType == "ok" {
			return fixtureResult(map[string]any{"type": "ok"})
		}
		if pane.FocusPaneID != "" {
			info["pane_id"] = pane.FocusPaneID
		}
		return fixtureResult(map[string]any{"type": "agent_info", "agent": info})
	default:
		info["name"] = "orch_fixture"
		return fixtureResult(map[string]any{"type": "agent_info", "agent": info})
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func fixtureResult(result any) int {
	body, err := json.Marshal(map[string]any{"id": "cli:fixture", "result": result})
	if err != nil {
		return 24
	}
	if _, err := os.Stdout.Write(append(body, '\n')); err != nil {
		return 25
	}
	return 0
}

func fixtureError(code, message string) int {
	body, err := json.Marshal(map[string]any{
		"id":    "cli:fixture",
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return 26
	}
	if _, err := os.Stderr.Write(append(body, '\n')); err != nil {
		return 27
	}
	return 1
}

func fixtureUnexpected(argv []string, why string) int {
	fmt.Fprintf(os.Stderr, "fixture: %s: argv %q\n", why, argv)
	return 28
}

func appendCall(dir string, argv []string) error {
	line, err := json.Marshal(fixtureCall{Argv: argv})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, fixtureCalls), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.Write(append(line, '\n'))
	return err
}

// ---------------------------------------------------------------------------
// fixture harness
// ---------------------------------------------------------------------------

type boardFixture struct {
	t       *testing.T
	dir     string
	plan    fixturePlan
	store   *state.Store
	client  *herdr.Client
	project string
}

// newBoardFixture wires a real store and a stand-in Herdr CLI. The project key
// is the canonical root a bind records.
func newBoardFixture(t *testing.T, plan fixturePlan) *boardFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if plan.Panes == nil {
		plan.Panes = map[string]fixturePane{}
	}
	f := &boardFixture{t: t, dir: t.TempDir(), plan: plan, project: `C:\work\demo-repo`}
	t.Setenv(fixtureEnv, "1")
	t.Setenv(fixtureEnvDir, f.dir)

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store

	client, err := herdr.NewClient(exe)
	if err != nil {
		t.Fatalf("herdr.NewClient: %v", err)
	}
	f.client = client
	f.writePlan()
	return f
}

// seed records the bound project and one healthy pane per task, so a test only
// has to state the facts it cares about.
func (f *boardFixture) seed(t *testing.T, tasks ...task.Task) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.CreateProject(ctx, task.Project{
		Key:                f.project,
		RepoWorkspaceID:    "w1",
		CurrentWorkspaceID: "w1",
		LaunchPaneID:       "w1:p1",
		PublicPaneID:       "w1:p1",
		AgentName:          "orch_p_fixture",
		OMPSessionID:       "primary-session",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, tsk := range tasks {
		tsk.ProjectKey = f.project
		if err := f.store.CreateTask(ctx, tsk); err != nil {
			t.Fatalf("create task %s: %v", tsk.ID, err)
		}
		f.plan.Panes[tsk.PublicPaneID] = fixturePane{
			PaneID:       tsk.PublicPaneID,
			TerminalID:   tsk.TerminalID,
			WorkspaceID:  tsk.CurrentWorkspaceID,
			TabID:        tsk.CurrentWorkspaceID + ":t1",
			Cwd:          tsk.WorktreePath,
			AgentStatus:  "working",
			SessionKind:  task.SessionKindPath,
			SessionValue: tsk.OMPSessionID,
		}
	}
	f.writePlan()
}

func (f *boardFixture) writePlan() {
	f.t.Helper()
	data, err := json.Marshal(f.plan)
	if err != nil {
		f.t.Fatalf("encode plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, fixturePlanName), data, 0o600); err != nil {
		f.t.Fatalf("write plan: %v", err)
	}
}

func (f *boardFixture) updatePlan(mutate func(*fixturePlan)) {
	f.t.Helper()
	mutate(&f.plan)
	f.writePlan()
}

func (f *boardFixture) task(id string) task.Task {
	f.t.Helper()
	tsk, err := f.store.Task(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read task %s: %v", id, err)
	}
	return tsk
}

func (f *boardFixture) calls() [][]string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, fixtureCalls))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		f.t.Fatalf("read the call log: %v", err)
	}
	calls := [][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var call fixtureCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			f.t.Fatalf("decode %q: %v", line, err)
		}
		calls = append(calls, call.Argv)
	}
	return calls
}

func (f *boardFixture) model(t *testing.T, opts Options) *Model {
	t.Helper()
	opts.Theme = Theme{Profile: termenv.Ascii}
	opts.RefreshInterval = -1
	m, err := NewModel(context.Background(), f.store, f.client, f.project, opts)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	return m
}

// sampleTask is one task with healthy facts. A test states only what it breaks.
func sampleTask(id string, mutate func(*task.Task)) task.Task {
	tsk := task.Task{
		ID:                  id,
		Title:               "title " + id,
		Brief:               "brief " + id,
		BaseRevision:        strings.Repeat("a", 40),
		Branch:              "orch/" + id,
		WorktreePath:        `C:\herdr\worktrees\demo-repo\` + id,
		WorktreeWorkspaceID: "w-" + id,
		CurrentWorkspaceID:  "w-" + id,
		TerminalID:          "term_" + id,
		LaunchPaneID:        samplePane(id),
		PublicPaneID:        samplePane(id),
		OMPSessionID:        sampleSession(id),
		Phase:               task.PromptSubmitted,
		Activity:            "working",
		ActivitySeenAt:      1753000000000,
	}
	if mutate != nil {
		mutate(&tsk)
	}
	return tsk
}

func samplePane(id string) string    { return "w-" + id + ":p1" }
func sampleSession(id string) string { return `C:\herdr\sessions\` + id + ".jsonl" }

// ---------------------------------------------------------------------------
// navigation
// ---------------------------------------------------------------------------

func press(t *testing.T, m *Model, msg tea.Msg) tea.Cmd {
	t.Helper()
	updated, cmd := m.Update(msg)
	next, ok := updated.(*Model)
	if !ok {
		t.Fatalf("Update returned %T, want the board model", updated)
	}
	*m = *next
	return cmd
}

func keyPress(key string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

// TestBoardNavigationMovesSelectionWithoutMutatingTasks walks the board the way
// a reader does: up/down inside a column, tab between columns, enter for the
// details, q to close. Every one of those keys must leave the durable task
// facts, and Herdr, untouched.
func TestBoardNavigationMovesSelectionWithoutMutatingTasks(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	queued := sampleTask("orch_q1", func(x *task.Task) {
		x.Phase = task.Intent
		x.Activity = ""
		x.ActivitySeenAt = 0
	})
	running := sampleTask("orch_r2", nil)
	second := sampleTask("orch_r3", nil)
	blocked := sampleTask("orch_n4", func(x *task.Task) { x.Blocker = "which error contract?" })
	review := sampleTask("orch_v5", func(x *task.Task) {
		x.Report = &task.Report{Kind: "result", Summary: "done"}
	})
	f.seed(t, queued, running, second, blocked, review)

	m := f.model(t, Options{})
	if got := m.SelectedID(); got != queued.ID {
		t.Fatalf("initial selection = %q, want the first card of the board %q", got, queued.ID)
	}
	before := f.snapshot()

	down := func() {
		t.Helper()
		press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	up := func() {
		t.Helper()
		press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	}

	up()
	if got := m.SelectedID(); got != queued.ID {
		t.Errorf("up past the first card selected %q, want it to stay on %q", got, queued.ID)
	}
	down()
	if got := m.SelectedID(); got != running.ID {
		t.Errorf("down selected %q, want %q", got, running.ID)
	}
	down()
	if got := m.SelectedID(); got != second.ID {
		t.Errorf("down selected %q, want %q", got, second.ID)
	}
	down()
	if got := m.SelectedID(); got != blocked.ID {
		t.Errorf("down selected %q, want the next column's card %q", got, blocked.ID)
	}
	down()
	if got := m.SelectedID(); got != review.ID {
		t.Errorf("down selected %q, want %q", got, review.ID)
	}
	down()
	if got := m.SelectedID(); got != review.ID {
		t.Errorf("down past the last card selected %q, want it to stay on %q", got, review.ID)
	}
	up()
	if got := m.SelectedID(); got != blocked.ID {
		t.Errorf("up selected %q, want %q", got, blocked.ID)
	}

	// tab continues from the card the arrows left the reader on: the next
	// column after Needs input is Ready for review.
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != review.ID {
		t.Errorf("tab selected %q, want the ready-for-review card %q", got, review.ID)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != "" {
		t.Errorf("tab into an empty column selected %q, want no selection", got)
	}
	if !strings.Contains(m.View(), StageLabel(task.Done)) {
		t.Error("the empty column section is not rendered")
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != queued.ID {
		t.Errorf("tab wrapped to %q, want %q", got, queued.ID)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.SelectedID(); got != "" {
		t.Errorf("shift+tab selected %q, want the empty done column", got)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.SelectedID(); got != review.ID {
		t.Errorf("shift+tab selected %q, want %q", got, review.ID)
	}

	if m.expanded {
		t.Fatal("the details were open before enter was pressed")
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded {
		t.Fatal("enter did not open the details")
	}
	if !strings.Contains(m.View(), "Brief: ") {
		t.Errorf("the expanded view shows no brief:\n%s", m.View())
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.expanded {
		t.Fatal("enter did not close the details again")
	}

	cmd := press(t, m, keyPress("q"))
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q returned %T, want a quit", cmd())
	}

	if after := f.snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("navigation changed the durable task facts:\nbefore %+v\nafter  %+v", before, after)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("navigation called Herdr: %v", calls)
	}
}

// TestBoardRefreshesFromTheStoreOnTick covers the read-only refresh: a task
// that moved between columns while the board was open appears in its new column
// after a refresh, and only a read happened.
func TestBoardRefreshesFromTheStoreOnTick(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	blocked := sampleTask("orch_n4", func(x *task.Task) { x.Blocker = "which error contract?" })
	f.seed(t, blocked)
	m := f.model(t, Options{})
	if got := m.SelectedID(); got != blocked.ID {
		t.Fatalf("selection = %q, want %q", got, blocked.ID)
	}

	current := f.task(blocked.ID)
	if _, err := f.store.UpdateTask(context.Background(), blocked.ID, current.Revision, func(next *task.Task) error {
		next.Blocker = ""
		next.Report = &task.Report{Kind: "result", Summary: "done"}
		return nil
	}); err != nil {
		t.Fatalf("record the report: %v", err)
	}

	press(t, m, tickMsg(time.Now()))
	if got := m.SelectedID(); got != blocked.ID {
		t.Errorf("selection after the refresh = %q, want the same task id", got)
	}
	stages := map[task.Stage]int{}
	for _, card := range m.board.Cards() {
		stages[card.Stage]++
	}
	if stages[task.ReadyForReview] != 1 || stages[task.NeedsInput] != 0 {
		t.Errorf("the refresh did not move the card: %+v", stages)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("the refresh called Herdr: %v", calls)
	}
}

// ---------------------------------------------------------------------------
// focus: the only side-effecting key
// ---------------------------------------------------------------------------

// TestFocusVerifiedPaneIsTheOnlyHerdrWrite covers every way the board can
// refuse: a stale, missing, replaced or re-pointed pane, an unreadable session,
// and a Herdr refusal. Only a pane that is proven to still be the recorded
// worker is focused, with exactly one `agent focus` argv.
func TestFocusVerifiedPaneIsTheOnlyHerdrWrite(t *testing.T) {
	pane := samplePane("orch_w1")
	breakPane := func(mutate func(*fixturePane)) func(*fixturePlan) {
		return func(p *fixturePlan) {
			broken := p.Panes[pane]
			mutate(&broken)
			p.Panes[pane] = broken
		}
	}
	cases := []struct {
		name    string
		broken  func(p *fixturePlan)
		want    string // substring the refusal must name
		focused bool
		// attempted marks a case where Herdr was asked to focus and answered
		// something the board refused to accept: exactly one focus on the
		// recorded pane, which is still a refusal to claim success.
		attempted bool
	}{
		{name: "verified", focused: true},
		{
			name:    "verified, with a Herdr that only acknowledges the focus",
			broken:  breakPane(func(p *fixturePane) { p.FocusResultType = "ok" }),
			focused: true,
		},
		{
			name:   "a stale public pane id that resolves elsewhere",
			broken: breakPane(func(p *fixturePane) { p.PaneID = "wZ:p1" }),
			want:   "wZ:p1",
		},
		{
			name:   "a pane that no longer exists",
			broken: breakPane(func(p *fixturePane) { p.Missing = true }),
			want:   "pane_not_found",
		},
		{
			name:   "a pane on a different terminal",
			broken: breakPane(func(p *fixturePane) { p.TerminalID = "term_somebody_else" }),
			want:   "term_somebody_else",
		},
		{
			name:   "a pane in a different workspace",
			broken: breakPane(func(p *fixturePane) { p.WorkspaceID = "wZ"; p.TabID = "wZ:t1" }),
			want:   "wZ",
		},
		{
			name:   "a pane in a different checkout",
			broken: breakPane(func(p *fixturePane) { p.Cwd = `C:\somewhere\else` }),
			want:   `C:\somewhere\else`,
		},
		{
			name:   "a pane running another OMP session",
			broken: breakPane(func(p *fixturePane) { p.SessionValue = `C:\herdr\sessions\somebody-else.jsonl` }),
			want:   "somebody-else",
		},
		{
			name:   "a session reference this build cannot read",
			broken: breakPane(func(p *fixturePane) { p.SessionKind = "pid"; p.SessionValue = "4242" }),
			want:   "session",
		},
		{
			name:      "Herdr refuses the focus",
			broken:    breakPane(func(p *fixturePane) { p.FocusError = "pane_not_found" }),
			want:      "pane_not_found",
			attempted: true,
		},
		{
			name:      "Herdr focuses somebody else",
			broken:    breakPane(func(p *fixturePane) { p.FocusPaneID = "wZ:p1" }),
			want:      "wZ:p1",
			attempted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker := sampleTask("orch_w1", nil)
			f := newBoardFixture(t, fixturePlan{})
			f.seed(t, worker)
			if tc.broken != nil {
				f.updatePlan(tc.broken)
			}
			before := f.task(worker.ID)
			m := f.model(t, Options{})

			cmd := press(t, m, keyPress("f"))
			if cmd == nil {
				t.Fatal("f returned no command")
			}
			press(t, m, cmd())

			focusCalls := f.callsNamed("agent focus")
			if tc.focused {
				if m.noticeErr {
					t.Fatalf("focus failed: %s", m.notice)
				}
				if !strings.Contains(m.notice, worker.PublicPaneID) {
					t.Errorf("notice = %q, want it to name the focused pane %q", m.notice, worker.PublicPaneID)
				}
				if len(focusCalls) != 1 {
					t.Fatalf("agent focus calls = %v, want exactly one", focusCalls)
				}
				if got := focusCalls[0][2]; got != worker.PublicPaneID {
					t.Errorf("focused %q, want the recorded public pane id %q", got, worker.PublicPaneID)
				}
			} else {
				if !m.noticeErr {
					t.Fatalf("the board reported success on a pane it could not prove: %q", m.notice)
				}
				switch {
				case tc.attempted:
					if len(focusCalls) != 1 || focusCalls[0][2] != worker.PublicPaneID {
						t.Errorf("the refusal did not come from exactly one focus on the recorded pane: %v", focusCalls)
					}
				case len(focusCalls) != 0:
					t.Errorf("the board focused through a refusal: %v", focusCalls)
				}
				if tc.want != "" && !strings.Contains(m.notice, tc.want) {
					t.Errorf("refusal %q does not name %q", m.notice, tc.want)
				}
				if !strings.Contains(m.notice, worker.ID) {
					t.Errorf("refusal %q does not name the task", m.notice)
				}
			}
			// The outcome reaches the reader, not just the model.
			press(t, m, tea.WindowSizeMsg{Width: 200})
			if view := m.View(); !strings.Contains(view, m.notice) {
				t.Errorf("the board does not show %q:\n%s", m.notice, view)
			}
			if after := f.task(worker.ID); task.StageOf(after) != task.Running ||
				after.Revision != before.Revision || after.UpdatedAt != before.UpdatedAt {
				t.Errorf("focus changed the task facts: %+v", after)
			}
			for _, call := range f.calls() {
				switch call[0] + " " + call[1] {
				case "pane get", "agent get", "agent focus":
				default:
					t.Errorf("the board ran %v; only reads and a verified focus are allowed", call)
				}
			}
		})
	}
}

// TestFocusReReadsTheTaskBeforeItActs pins the re-read: a card rendered before
// the task moved public pane must focus the pane the store records now, not the
// one the board drew.
func TestFocusReReadsTheTaskBeforeItActs(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	worker := sampleTask("orch_w1", nil)
	f.seed(t, worker)
	m := f.model(t, Options{})
	if got := m.board.Cards()[0].Pane(); got != worker.PublicPaneID {
		t.Fatalf("the rendered card names pane %q, want the recorded %q", got, worker.PublicPaneID)
	}

	moved := `w-moved:p1`
	f.updatePlan(func(p *fixturePlan) {
		p.Panes[moved] = fixturePane{
			PaneID:       moved,
			TerminalID:   worker.TerminalID,
			WorkspaceID:  "w-moved",
			TabID:        "w-moved:t1",
			Cwd:          worker.WorktreePath,
			SessionKind:  task.SessionKindPath,
			SessionValue: worker.OMPSessionID,
		}
	})
	current := f.task(worker.ID)
	if _, err := f.store.UpdateTask(context.Background(), worker.ID, current.Revision, func(next *task.Task) error {
		next.PublicPaneID = moved
		next.CurrentWorkspaceID = "w-moved"
		return nil
	}); err != nil {
		t.Fatalf("move the task's public pane: %v", err)
	}

	cmd := press(t, m, keyPress("f"))
	if cmd == nil {
		t.Fatal("f returned no command")
	}
	press(t, m, cmd())
	if m.noticeErr {
		t.Fatalf("focus failed: %s", m.notice)
	}
	calls := f.callsNamed("agent focus")
	if len(calls) != 1 || calls[0][2] != moved {
		t.Fatalf("agent focus calls = %v, want one focus on the freshly recorded pane %q", calls, moved)
	}
}

// TestFocusWithoutASelectionRefusesBeforeHerdr keeps an unselected board from
// guessing a pane.
func TestFocusWithoutASelectionRefusesBeforeHerdr(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t)
	m := f.model(t, Options{})
	press(t, m, keyPress("f"))
	if !m.noticeErr {
		t.Fatalf("focus without a selection reported %q", m.notice)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("focus without a selection called Herdr: %v", calls)
	}
}

// ---------------------------------------------------------------------------
// program
// ---------------------------------------------------------------------------

// TestBoardRunRendersTheProjectAndQuits runs the real Bubble Tea program with a
// keyed input, so the pane route is exercised end to end.
func TestBoardRunRendersTheProjectAndQuits(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	worker := sampleTask("orch_w1", nil)
	f.seed(t, worker)

	var out bytes.Buffer
	if err := Run(context.Background(), f.store, f.client, f.project, Options{
		Input:           strings.NewReader("q"),
		Output:          &out,
		Theme:           Theme{Profile: termenv.Ascii},
		RefreshInterval: -1,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{"Queued", "Running", StageLabel(task.ReadyForReview), worker.ID} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the board pane rendered no %q:\n%s", want, rendered)
		}
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("reading the board called Herdr: %v", calls)
	}
}

// ---------------------------------------------------------------------------
// project resolution
// ---------------------------------------------------------------------------

// TestBoardProjectResolutionMapsThePaneContextBackToTheProject covers the pane
// route: the board is opened from a workspace or a checkout and must find the
// one bound project those identities belong to, or refuse.
func TestBoardProjectResolutionMapsThePaneContextBackToTheProject(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	worker := sampleTask("orch_w1", nil)
	f.seed(t, worker)
	ctx := context.Background()

	t.Run("by the bound workspace", func(t *testing.T) {
		project, err := ProjectFor(ctx, f.store, "w1", "")
		if err != nil {
			t.Fatalf("ProjectFor: %v", err)
		}
		if project.Key != f.project {
			t.Errorf("project = %q, want %q", project.Key, f.project)
		}
	})

	t.Run("by the canonical checkout", func(t *testing.T) {
		project, err := ProjectFor(ctx, f.store, "", f.project)
		if err != nil {
			t.Fatalf("ProjectFor: %v", err)
		}
		if project.Key != f.project {
			t.Errorf("project = %q, want %q", project.Key, f.project)
		}
	})

	t.Run("by a worker's worktree", func(t *testing.T) {
		project, err := ProjectFor(ctx, f.store, worker.CurrentWorkspaceID, worker.WorktreePath)
		if err != nil {
			t.Fatalf("ProjectFor: %v", err)
		}
		if project.Key != f.project {
			t.Errorf("project = %q, want the worker's project %q", project.Key, f.project)
		}
	})

	t.Run("an unknown workspace is refused", func(t *testing.T) {
		if _, err := ProjectFor(ctx, f.store, "w-nothing", `C:\somewhere\else`); err == nil {
			t.Fatal("ProjectFor bound an unknown pane to a project")
		}
	})

	t.Run("an empty context is refused", func(t *testing.T) {
		if _, err := ProjectFor(ctx, f.store, "", ""); err == nil {
			t.Fatal("ProjectFor guessed a project from an empty context")
		}
	})
}

// snapshot is every task's durable facts, so a test can prove a key sequence
// wrote nothing.
func (f *boardFixture) snapshot() map[string]task.Task {
	f.t.Helper()
	projects, err := f.store.Projects(context.Background())
	if err != nil {
		f.t.Fatalf("read the projects: %v", err)
	}
	out := map[string]task.Task{}
	for _, project := range projects {
		tasks, err := f.store.Tasks(context.Background(), project.Key)
		if err != nil {
			f.t.Fatalf("read the tasks of %s: %v", project.Key, err)
		}
		for _, tsk := range tasks {
			out[tsk.ID] = tsk
		}
	}
	return out
}

func (f *boardFixture) callsNamed(name string) [][]string {
	f.t.Helper()
	var named [][]string
	for _, call := range f.calls() {
		if len(call) >= 2 && call[0]+" "+call[1] == name {
			named = append(named, call)
		}
	}
	return named
}
