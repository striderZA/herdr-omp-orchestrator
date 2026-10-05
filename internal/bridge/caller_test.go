package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// handleRaw runs one request with exactly the Ops given: unlike callWith it adds
// no stand-in VerifyCaller, so the seam's own behavior is what is observed.
func (f *bridgeFixture) handleRaw(t *testing.T, method, session, params string, ops Ops) bridgeResponse {
	t.Helper()
	payload, err := json.Marshal(wireRequest{Version: Protocol, Method: method, OMPSessionID: session, Params: json.RawMessage(params)})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	out, err := HandleWith(context.Background(), f.env, strings.NewReader(string(payload)), ops)
	if err != nil {
		t.Fatalf("Handle(%s): %v", method, err)
	}
	return decodeResponse(t, out)
}

// TestVerifyCallerIsTheSharedSeam pins where the live check runs: once per
// authorized call, with the verified principal, before the operation, and never
// for the handshake methods. A build that wires no check refuses.
func TestVerifyCallerIsTheSharedSeam(t *testing.T) {
	if Protocol != 1 {
		t.Fatalf("Protocol = %d, want 1: the live check adds no protocol change", Protocol)
	}
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.asPrimary()

	var order []string
	var seen []Principal
	op := func(context.Context, Principal, json.RawMessage) (any, error) {
		order = append(order, "op")
		return map[string]any{"ok": true}, nil
	}
	verify := func(_ context.Context, p Principal) error {
		order = append(order, "verify")
		seen = append(seen, p)
		return nil
	}

	t.Run("a build without the check refuses before the operation", func(t *testing.T) {
		order = nil
		resp := f.handleRaw(t, MethodTaskList, primarySession, `{}`, Ops{TaskList: op})
		wantError(t, resp, CodeNotImplemented)
		if len(order) != 0 {
			t.Fatalf("the operation ran without a live caller check: %v", order)
		}
	})

	t.Run("an unwired method is verified before it answers not_implemented", func(t *testing.T) {
		order = nil
		wantError(t, f.handleRaw(t, MethodTaskList, primarySession, `{}`, Ops{VerifyCaller: verify}), CodeNotImplemented)
		if strings.Join(order, ",") != "verify" {
			t.Fatalf("order = %v, want the live check before not_implemented", order)
		}
		refuse := func(context.Context, Principal) error {
			return &Error{Code: "agent_changed", Message: "the session changed"}
		}
		wantError(t, f.handleRaw(t, MethodTaskList, primarySession, `{}`, Ops{VerifyCaller: refuse}), "agent_changed")
	})

	t.Run("the check runs once, with the principal, before the operation", func(t *testing.T) {
		order, seen = nil, nil
		resp := f.handleRaw(t, MethodTaskList, primarySession, `{}`, Ops{TaskList: op, VerifyCaller: verify})
		if !resp.OK {
			t.Fatalf("task_list failed: %+v", resp.Error)
		}
		if strings.Join(order, ",") != "verify,op" {
			t.Fatalf("order = %v, want verify then op", order)
		}
		if len(seen) != 1 || seen[0].Role != state.RolePrimary || seen[0].LaunchPaneID != primaryPane || seen[0].ProjectKey != f.repoRoot {
			t.Fatalf("the check saw %+v, want the verified primary principal", seen)
		}
	})

	t.Run("a refusal is answered with its code and the operation never runs", func(t *testing.T) {
		order = nil
		refuse := func(context.Context, Principal) error {
			order = append(order, "verify")
			return &Error{Code: "agent_changed", Message: "the session changed"}
		}
		wantError(t, f.handleRaw(t, MethodTaskList, primarySession, `{}`, Ops{TaskList: op, VerifyCaller: refuse}), "agent_changed")
		if strings.Join(order, ",") != "verify" {
			t.Fatalf("order = %v, want only the refused check", order)
		}
		uncoded := func(context.Context, Principal) error { return errors.New("boom") }
		wantError(t, f.handleRaw(t, MethodTaskList, primarySession, `{}`, Ops{TaskList: op, VerifyCaller: uncoded}), CodeInternal)
	})

	t.Run("resolve and ready are the handshake and never run it", func(t *testing.T) {
		order = nil
		refuse := func(context.Context, Principal) error {
			order = append(order, "verify")
			return &Error{Code: "agent_changed", Message: "must not be asked"}
		}
		ops := Ops{VerifyCaller: refuse}
		if resp := f.handleRaw(t, "resolve", primarySession, `{}`, ops); !resp.OK {
			t.Fatalf("resolve failed: %+v", resp.Error)
		}
		if resp := f.handleRaw(t, "ready", primarySession, `{}`, ops); !resp.OK {
			t.Fatalf("ready failed: %+v", resp.Error)
		}
		if len(order) != 0 {
			t.Fatalf("the handshake ran the live check: %v", order)
		}
	})
}

// TestLiveCallerCheckRefusesAPrimaryHerdrNoLongerVouchesFor drives the real
// coordinator: the primary's recorded pane is re-read with `agent get` on every
// call, a read (task_list) and a write (ticket_create) alike.
func TestLiveCallerCheckRefusesAPrimaryHerdrNoLongerVouchesFor(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *bridgeFixture, s *herdrStub)
		code  string
	}{
		{"the session was replaced", func(f *bridgeFixture, s *herdrStub) {
			a := f.primaryAgent()
			a.SessionValue = "omp-primary-2"
			s.setAgent(primaryPane, a)
		}, "agent_changed"},
		{"the pane holds no agent", func(f *bridgeFixture, s *herdrStub) { s.dropAgent(primaryPane) }, "agent_changed"},
		{"the pane resolves to another pane", func(f *bridgeFixture, s *herdrStub) {
			a := f.primaryAgent()
			a.PaneID = "w9:p9"
			s.setAgent(primaryPane, a)
		}, "agent_changed"},
		{"another agent kind runs there", func(f *bridgeFixture, s *herdrStub) {
			a := f.primaryAgent()
			a.Agent = "claude"
			s.setAgent(primaryPane, a)
		}, "agent_changed"},
		{"no session reference is reported", func(f *bridgeFixture, s *herdrStub) {
			a := f.primaryAgent()
			a.SessionValue = ""
			s.setAgent(primaryPane, a)
		}, "agent_changed"},
		{"the agent works in another checkout", func(f *bridgeFixture, s *herdrStub) {
			a := f.primaryAgent()
			a.Cwd = t.TempDir()
			s.setAgent(primaryPane, a)
		}, "agent_changed"},
		{"the pane sits in another workspace", func(f *bridgeFixture, s *herdrStub) {
			a := f.primaryAgent()
			a.WorkspaceID = "ws-elsewhere"
			s.setAgent(primaryPane, a)
		}, "agent_changed"},
		{"Herdr answers agent get with a malformed body", func(f *bridgeFixture, s *herdrStub) {
			s.update(func(script *stubScript) { script.AgentGetMalformed = true })
		}, "agent_changed"},
		{"Herdr cannot be reached", func(f *bridgeFixture, s *herdrStub) {
			s.update(func(script *stubScript) { script.AgentGetError = "server_unavailable" })
		}, CodeHerdrUnavailable},
		{"the project records no workspace", func(f *bridgeFixture, s *herdrStub) {
			if err := f.store.UpdateProject(context.Background(), f.repoRoot, func(p *task.Project) error {
				p.CurrentWorkspaceID = ""
				return nil
			}); err != nil {
				t.Fatalf("clear the recorded workspace: %v", err)
			}
		}, "agent_changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBridgeFixture(t)
			f.seedPrimary()
			f.asPrimary()
			stub := f.useHerdrStub(stubScript{RepoRoot: f.repoRoot})
			ops := CoordinatorOps(f.coordinator(t))

			// The matching live primary succeeds.
			if resp := f.callWith(MethodTaskList, primarySession, `{}`, ops); !resp.OK {
				t.Fatalf("task_list for the live primary failed: %+v", resp.Error)
			}

			tc.setup(f, stub)
			wantError(t, f.callWith(MethodTaskList, primarySession, `{}`, ops), tc.code)
			wantError(t, f.callWith(MethodTicketCreate, primarySession, `{"title":"refused"}`, ops), tc.code)
			tickets, err := f.store.Tickets(context.Background(), f.repoRoot)
			if err != nil {
				t.Fatalf("read tickets: %v", err)
			}
			if len(tickets) != 0 {
				t.Fatalf("a refused caller created %d tickets", len(tickets))
			}
		})
	}
}

// TestLiveCallerCheckRefusesAWorkerHerdrNoLongerVouchesFor is the worker side:
// a report is refused, and records nothing, unless Herdr still reports the
// recorded session in the recorded pane, checkout and workspace.
func TestLiveCallerCheckRefusesAWorkerHerdrNoLongerVouchesFor(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *bridgeFixture, s *herdrStub)
		code  string
	}{
		{"the session was replaced", func(f *bridgeFixture, s *herdrStub) {
			s.setAgent(workerPane, workerAgent(workerPane, "omp-worker-2", f.worktree))
		}, "agent_changed"},
		{"the pane holds no agent", func(f *bridgeFixture, s *herdrStub) { s.dropAgent(workerPane) }, "agent_changed"},
		{"the agent left its checkout", func(f *bridgeFixture, s *herdrStub) {
			s.setAgent(workerPane, workerAgent(workerPane, workerSession, t.TempDir()))
		}, "agent_changed"},
		{"the pane moved to another workspace unrecorded", func(f *bridgeFixture, s *herdrStub) {
			a := workerAgent(workerPane, workerSession, f.worktree)
			a.WorkspaceID = "ws-moved"
			s.setAgent(workerPane, a)
		}, "agent_changed"},
		{"Herdr cannot be reached", func(f *bridgeFixture, s *herdrStub) {
			s.update(func(script *stubScript) { script.AgentGetError = "server_unavailable" })
		}, CodeHerdrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBridgeFixture(t)
			f.seedPrimary()
			f.seedWorker("t1", workerPane, workerPane, workerSession)
			f.asWorker()
			stub := f.useHerdrStub(stubScript{
				RepoRoot: f.repoRoot,
				Agents:   map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
			})
			ops := CoordinatorOps(f.coordinator(t))

			if resp := f.callWith("report", workerSession, `{"kind":"progress","summary":"first"}`, ops); !resp.OK {
				t.Fatalf("progress from the live worker failed: %+v", resp.Error)
			}
			tc.setup(f, stub)
			wantError(t, f.callWith("report", workerSession, `{"kind":"progress","summary":"second"}`, ops), tc.code)
			if got := notesOf(t, f, "progress"); len(got) != 1 {
				t.Fatalf("progress notes = %d, want only the live worker's", len(got))
			}
		})
	}
}

// A moved worker is verified at its recorded public pane, not the launch pane.
func TestLiveCallerCheckFollowsARecordedMove(t *testing.T) {
	const moved = "w7:p4"
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.movePublicPane(t, workerPane, moved)
	st := f.task(t, "t1")
	live := workerAgent(moved, workerSession, f.worktree)
	live.WorkspaceID = st.CurrentWorkspaceID
	f.useHerdrStub(stubScript{RepoRoot: f.repoRoot, Agents: map[string]stubAgent{moved: live}})
	ops := CoordinatorOps(f.coordinator(t))
	if resp := f.callWith("report", workerSession, `{"kind":"progress","summary":"after the move"}`, ops); !resp.OK {
		t.Fatalf("progress from the moved worker failed: %+v", resp.Error)
	}
	if got := len(notesOf(t, f, "progress")); got != 1 {
		t.Fatalf("progress notes = %d, want 1", got)
	}
	// The same worker is refused once Herdr no longer reports it at the recorded
	// public pane, even though its launch pane is unchanged.
	stub := &herdrStub{t: t, path: os.Getenv(fixtureScriptFile)}
	stub.dropAgent(moved)
	stub.setAgent(workerPane, live)
	wantError(t, f.callWith("report", workerSession, `{"kind":"progress","summary":"stale"}`, ops), "agent_changed")
}
