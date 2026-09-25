// Package task defines the durable facts of one dispatched unit of work and the
// projection from those facts onto a board column.
//
// Facts and activity are deliberately separate: Herdr activity only ever
// describes what a terminal is doing right now, so nothing here derives
// completion from it.
package task

// Stage is the board column a task occupies.
type Stage string

const (
	Queued         Stage = "queued"
	Running        Stage = "running"
	NeedsInput     Stage = "needs_input"
	ReadyForReview Stage = "ready_for_review"
	Done           Stage = "done"
)

// Phase records how far provisioning and dispatch of a task has progressed.
type Phase string

const (
	Intent          Phase = "intent"
	WorktreeCreated Phase = "worktree_created"
	AgentStarted    Phase = "agent_started"
	PromptSubmitted Phase = "prompt_submitted"
)

// Report is a worker's self-reported outcome. Only Kind "result" is final.
type Report struct {
	Kind, Summary, Branch string
	Evidence, Checks      []string
	Risk                  string
}

// Task is the durable record of one unit of work.
type Task struct {
	ID, ProjectKey, Title, Brief, BaseRevision, Branch  string
	Checks, Dependencies, ReservedPaths                 []string
	WorktreePath, WorktreeWorkspaceID                   string
	CurrentWorkspaceID, TerminalID                      string
	LaunchPaneID, PublicPaneID, AgentName, OMPSessionID string
	Phase                                               Phase
	Report                                              *Report
	Blocker, Attention, Disposition                     string
	Activity                                            string
	Revision, CreatedAt, UpdatedAt, ActivitySeenAt      int64
}

// Project is the durable record of one primary checkout the plugin dispatches from.
type Project struct {
	Key, RepoWorkspaceID, CurrentWorkspaceID, TerminalID string
	LaunchPaneID, PublicPaneID, AgentName, OMPSessionID  string
}

// Inbox is one durable note addressed to a project or task.
type Inbox struct {
	ID, ProjectKey, TaskID, Kind, Summary, Delivery string
}

// StageOf projects recorded facts onto a Stage in priority order.
//
// A recorded disposition closes a task; a blocker or attention request needs a
// human; only a final Report waits for review. Herdr activity and a partial
// report never complete anything.
func StageOf(x Task) Stage {
	switch {
	case x.Disposition != "":
		return Done
	case x.Blocker != "" || x.Attention != "":
		return NeedsInput
	case x.Report != nil && x.Report.Kind == "result":
		return ReadyForReview
	case x.Phase == WorktreeCreated || x.Phase == AgentStarted || x.Phase == PromptSubmitted:
		return Running
	default:
		return Queued
	}
}
