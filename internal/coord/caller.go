package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// VerifyCaller re-confirms, freshly from Herdr, that the bridge caller bound to
// launchPaneID is still the agent the orchestrator recorded for that binding.
//
// The bridge runs it on every authorized method call (previews and listings
// included), after the role and readiness checks and before the operation. The
// stored handshake proves who announced at some earlier moment; this proves the
// same session still occupies the recorded pane now. It reads the binding from
// the store, never from the caller, and writes nothing.
//
// The recorded pane (the public pane, or the launch pane when no public pane is
// recorded) must still resolve to itself, run the required agent kind, report a
// session reference the confirmed handshake proves is the recorded session, work
// in the recorded checkout and sit in the recorded workspace. A pane that is gone,
// an agent that contradicts any of that, or an `agent get` answer that is
// malformed or contradicts itself is ErrAgentChanged; a Herdr that cannot be
// asked at all is ErrHerdrUnavailable. Either way the call is refused.
func (c *Coordinator) VerifyCaller(ctx context.Context, role state.Role, launchPaneID string) error {
	var caller liveCaller
	switch role {
	case state.RolePrimary:
		project, err := c.primaryProject(ctx, launchPaneID)
		if err != nil {
			return err
		}
		pane, _ := firstPane(project.PublicPaneID, project.LaunchPaneID)
		caller = liveCaller{
			subject:   "the primary of project " + project.Key,
			launch:    project.LaunchPaneID,
			pane:      pane,
			session:   project.OMPSessionID,
			checkout:  project.Key,
			workspace: project.CurrentWorkspaceID,
		}
	case state.RoleWorker:
		st, err := c.callerTask(ctx, launchPaneID)
		if err != nil {
			return err
		}
		pane, _ := firstPane(st.PublicPaneID, st.LaunchPaneID)
		caller = liveCaller{
			subject:   "the worker of task " + st.ID,
			launch:    st.LaunchPaneID,
			pane:      pane,
			session:   st.OMPSessionID,
			checkout:  st.WorktreePath,
			workspace: st.CurrentWorkspaceID,
		}
	default:
		return fmt.Errorf("%w: role %q has no live caller to verify", ErrOwnership, role)
	}
	return c.confirmLiveCaller(ctx, caller)
}

// callerTask reads the task a worker launch pane is bound to. Unlike workerTask
// it does not judge whether the task's channel is still open: that is the
// operation's decision, and identity is the only question here.
func (c *Coordinator) callerTask(ctx context.Context, launchPaneID string) (task.Task, error) {
	pane := strings.TrimSpace(launchPaneID)
	if pane == "" {
		return task.Task{}, fmt.Errorf("%w: no worker launch pane was given", ErrOwnership)
	}
	st, err := c.store.ByLaunchPane(ctx, pane)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Task{}, fmt.Errorf("%w: launch pane %q is not a recorded worker", ErrOwnership, pane)
	case err != nil:
		return task.Task{}, fmt.Errorf("read the worker bound to pane %q: %w", pane, err)
	case strings.TrimSpace(st.LaunchPaneID) != pane:
		return task.Task{}, fmt.Errorf("%w: pane %q is not the recorded launch pane of task %s", ErrOwnership, pane, st.ID)
	}
	return st, nil
}

// liveCaller is the recorded identity of one bridge caller, as persisted facts
// describe it.
type liveCaller struct {
	subject   string
	launch    string
	pane      string
	session   string
	checkout  string
	workspace string
}

// confirmLiveCaller compares one recorded caller with Herdr's live `agent get`.
// The session comparison is the handshake's own: the confirmed announcement of
// the launch pane relates the recorded value to whichever form Herdr reports.
func (c *Coordinator) confirmLiveCaller(ctx context.Context, who liveCaller) error {
	if strings.TrimSpace(who.pane) == "" {
		return fmt.Errorf("%w: %s records no pane, so it cannot be verified", ErrAgentChanged, who.subject)
	}
	if strings.TrimSpace(who.session) == "" {
		return fmt.Errorf("%w: %s records no OMP session, so it cannot be verified", ErrAgentChanged, who.subject)
	}
	identity, err := c.identityOf(ctx, who.launch, who.session)
	if err != nil {
		return fmt.Errorf("read the confirmed session of %s: %w", who.subject, err)
	}
	if !identity.Proven {
		return fmt.Errorf("%w: launch pane %q has no verified OMP session, so %s cannot be trusted", ErrAgentChanged, who.launch, who.subject)
	}
	view, err := c.agentGet(ctx, who.pane)
	switch {
	case isMissingTarget(err):
		return fmt.Errorf("%w: pane %q of %s holds no agent any more", ErrAgentChanged, who.pane, who.subject)
	case errors.Is(err, herdr.ErrMalformedResponse), errors.Is(err, herdr.ErrIdentity):
		return fmt.Errorf("%w: Herdr's answer for pane %q of %s cannot be trusted: %v", ErrAgentChanged, who.pane, who.subject, err)
	case err != nil:
		return fmt.Errorf("%w: the live agent of %s could not be read from pane %q: %w", ErrHerdrUnavailable, who.subject, who.pane, err)
	}
	live := view.Agent
	switch {
	case live.PaneID != who.pane:
		return fmt.Errorf("%w: pane %q of %s now resolves to %q", ErrAgentChanged, who.pane, who.subject, live.PaneID)
	case !strings.EqualFold(strings.TrimSpace(live.Agent), c.opts.AgentKind):
		return fmt.Errorf("%w: pane %q of %s runs %q, not %q", ErrAgentChanged, who.pane, who.subject, live.Agent, c.opts.AgentKind)
	case view.RefErr != nil:
		return fmt.Errorf("%w: pane %q of %s reports a session reference this build cannot read: %v", ErrAgentChanged, who.pane, who.subject, view.RefErr)
	case !view.HasRef:
		return fmt.Errorf("%w: Herdr reports no session reference for pane %q, so %s cannot be verified", ErrAgentChanged, who.pane, who.subject)
	case !identity.proves(view.Ref):
		return fmt.Errorf("%w: pane %q reports %s session %q, which is not the session %s recorded (%s)",
			ErrAgentChanged, who.pane, view.Ref.Kind, view.Ref.Value, who.subject, identity.Announced.Describe())
	}
	// A binding that records no checkout or workspace cannot have its provenance
	// verified, so it is refused rather than compared against nothing.
	checkout := strings.TrimSpace(who.checkout)
	switch {
	case checkout == "":
		return fmt.Errorf("%w: %s records no checkout, so its provenance cannot be verified", ErrAgentChanged, who.subject)
	case !samePath(live.Cwd, checkout):
		return fmt.Errorf("%w: pane %q of %s works in %q, not the recorded checkout %s", ErrAgentChanged, who.pane, who.subject, live.Cwd, checkout)
	}
	workspace := strings.TrimSpace(who.workspace)
	switch {
	case workspace == "":
		return fmt.Errorf("%w: %s records no workspace, so its provenance cannot be verified", ErrAgentChanged, who.subject)
	case strings.TrimSpace(live.WorkspaceID) != workspace:
		return fmt.Errorf("%w: pane %q of %s is in workspace %q, not the recorded %q", ErrAgentChanged, who.pane, who.subject, live.WorkspaceID, workspace)
	}
	return nil
}
