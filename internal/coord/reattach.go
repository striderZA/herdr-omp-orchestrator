package coord

// Issue #18: re-establishing a lost primary is an explicit, user-invoked action.
//
// Bind never substitutes a new primary for a recorded one that is gone, and the
// startup pass only records that the primary is absent. Reattach is the one route
// that moves a project to a replacement pane, and it is built so that nothing
// about it is automatic or silent:
//
//   - PlanReattach reads Herdr and the store and writes nothing. Its plan names
//     the replacement pane (workspace, terminal, agent, native session) and the
//     recorded primary it would replace, and that plan is what the user confirms.
//   - Reattach re-derives the plan, refuses unless it is still the one that was
//     confirmed, records the binding in one compare-and-swap of the project row,
//     and only then completes the handshake the way bind does: one short notice
//     to the replacement session, then awaitAgentReady.
//
// The write comes before the notice because the extension in the replacement
// session can only resolve and announce itself once the project records its
// session. The binding is therefore provisional until awaitAgentReady confirms
// it, and the result says so: a failure after the write returns
// ErrReattachPending with the project as recorded and a primary attention that
// names the unconfirmed state, instead of reporting success.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

var (
	// ErrPrimaryLive reports a recorded primary that is verified live. Reattach
	// replaces a primary that is lost or unproven, never one that is answering.
	ErrPrimaryLive = errors.New("coord: the recorded primary is still live")
	// ErrAlreadyBound reports a replacement pane or session that another record
	// already owns: a worker, another project's primary, or an ambiguous claim.
	ErrAlreadyBound = errors.New("coord: the pane or its session already belongs to another binding")
	// ErrReplacementBusy reports a replacement session that is not idle, so the
	// notice that lets its extension announce itself cannot be sent safely.
	ErrReplacementBusy = errors.New("coord: the replacement session is not idle")
	// ErrReattachPending reports a reattach that recorded the replacement but could
	// not confirm it. The binding stays recorded, marked as unconfirmed on the
	// project; bind (or the same reattach again) completes the confirmation.
	ErrReattachPending = errors.New("coord: the primary was reattached but is not confirmed")
)

// reattachHint is appended to every refusal of Bind that means the recorded
// primary is not live, so the message names the action that exists for it.
const reattachHint = `start OMP in a pane at the repository root, then run the "reattach" plugin action (Reattach the primary OMP session) from that pane`

// reattachObservedLimit bounds the diagnostic kept about a displaced primary.
const reattachObservedLimit = 240

// ReattachPlan is what one reattach would record, as verified from Herdr and the
// store. It is what the user confirms: Reattach refuses unless the facts it
// re-derives are the ones in the plan it was given.
type ReattachPlan struct {
	// ProjectKey is the canonical repository root of the project being reattached.
	ProjectKey string
	// PaneID, WorkspaceID, TerminalID and AgentName are Herdr's own identities for
	// the replacement pane and the agent in it; Session is its native session.
	PaneID, WorkspaceID, TerminalID, AgentName string
	Session                                    task.SessionRef
	// Replaces is the primary the project records now. Its Reason is empty here;
	// Observed carries the finding.
	Replaces task.ReplacedPrimary
	// Observed is why the recorded primary is not proven live. It is the one field
	// that is not part of the confirmation: it is diagnostic text a probe words
	// from live state, and it is shown to the user, not compared.
	Observed string
	// Recorded reports that the project already records this pane and session, so
	// confirming only completes the handshake an earlier reattach left pending.
	Recorded bool
	// Notice is the state of the notice an earlier reattach sent to this recorded
	// replacement (task.NoticeSent or task.NoticeUncertain), or empty when none was
	// sent. It is part of the confirmation: with a notice on record, confirming
	// never sends another.
	Notice string
}

// noticeState words the recorded notice attempt for the person confirming.
func (p ReattachPlan) noticeState() string {
	if p.Notice == task.NoticeUncertain {
		return "its delivery is uncertain"
	}
	return "it was delivered"
}

// sameAs reports whether two plans describe the same reattach.
func (p ReattachPlan) sameAs(other ReattachPlan) bool {
	p.Observed, other.Observed = "", ""
	return p == other
}

// Describe renders the plan for the person who has to confirm it.
func (p ReattachPlan) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reattach the primary of %s\n\n", p.ProjectKey)
	fmt.Fprintf(&b, "  replacement  pane %s (workspace %s, terminal %s)\n", p.PaneID, p.WorkspaceID, p.TerminalID)
	if p.AgentName != "" {
		fmt.Fprintf(&b, "               agent %s\n", p.AgentName)
	}
	fmt.Fprintf(&b, "               session %s\n", p.Session.Value)
	switch {
	case p.Recorded:
		b.WriteString("  recorded     this pane and session are already recorded as the primary but were never confirmed\n")
	case strings.TrimSpace(p.Replaces.LaunchPaneID) == "":
		b.WriteString("  replaces     no primary pane is recorded\n")
	default:
		fmt.Fprintf(&b, "  replaces     pane %s (workspace %s)\n", p.Replaces.PublicPaneID, p.Replaces.WorkspaceID)
		if p.Replaces.OMPSessionID != "" {
			fmt.Fprintf(&b, "               session %s\n", p.Replaces.OMPSessionID)
		}
	}
	if p.Observed != "" {
		fmt.Fprintf(&b, "               %s\n", p.Observed)
	}
	b.WriteString("\n")
	switch {
	case p.Notice != "":
		fmt.Fprintf(&b, "A notice already went to this session (%s). Confirming does not send another: it waits for the session's extension to announce itself. If it still has not, send the session a message yourself.\n", p.noticeState())
	case p.Recorded:
		b.WriteString("Confirming completes the handshake: the replacement session is sent one short notice so its orchestrator extension can announce itself.\n")
	default:
		b.WriteString("Confirming records the replacement as the primary. Workers, tasks and the inbox are not touched, and the replaced session is recorded on the project.\n")
		b.WriteString("The replacement session is then sent one short notice so its orchestrator extension can announce itself and the orchestrator can confirm it.\n")
	}
	return b.String()
}

// reattachFacts is one verification: the plan plus the records it was derived from.
type reattachFacts struct {
	plan    ReattachPlan
	project task.Project
	view    agentView
}

// PaneProject names the project a pane belongs to: the canonical repository root
// of the directory the pane works in. It only names a candidate; PlanReattach
// decides whether that project can be reattached to the pane.
func (c *Coordinator) PaneProject(ctx context.Context, paneID string) (string, error) {
	pane, err := c.replacementPane(ctx, paneID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(pane.Cwd) == "" {
		return "", fmt.Errorf("%w: pane %q reports no working directory", ErrInvalidRequest, pane.PaneID)
	}
	return c.canonicalRoot(ctx, pane.Cwd)
}

// replacementPane reads the pane a reattach would adopt.
func (c *Coordinator) replacementPane(ctx context.Context, paneID string) (*herdr.PaneInfo, error) {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return nil, fmt.Errorf("%w: no replacement pane was named", ErrInvalidRequest)
	}
	pane, err := c.paneGet(ctx, paneID)
	switch {
	case isMissingTarget(err):
		return nil, fmt.Errorf("%w: pane %q does not exist", ErrInvalidRequest, paneID)
	case err != nil:
		return nil, err
	}
	return pane, nil
}

// PlanReattach verifies a replacement pane against a bound project and returns
// the plan a user can confirm. It never writes.
//
// It refuses, rather than guessing, when the evidence does not show a live OMP
// session of this repository that nothing else owns: the pane must exist and work
// at the project's root, must host the required agent kind in the workspace Herdr
// reports for the pane, must report a native session reference this build can
// read, and neither the pane nor that session may belong to a worker or another
// project. It also refuses while the recorded primary is verified live, and when
// Herdr cannot say whether it is.
func (c *Coordinator) PlanReattach(ctx context.Context, projectKey, paneID string) (ReattachPlan, error) {
	facts, err := c.verifyReattach(ctx, projectKey, paneID)
	if err != nil {
		return ReattachPlan{}, err
	}
	return facts.plan, nil
}

func (c *Coordinator) verifyReattach(ctx context.Context, projectKey, paneID string) (reattachFacts, error) {
	key := strings.TrimSpace(projectKey)
	if key == "" {
		return reattachFacts{}, fmt.Errorf("%w: no project was named", ErrInvalidRequest)
	}
	project, err := c.store.Project(ctx, key)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return reattachFacts{}, fmt.Errorf("%w: %q is not a bound project; bind the repository first", ErrProjectUnknown, key)
	case err != nil:
		return reattachFacts{}, err
	}
	pane, err := c.replacementPane(ctx, paneID)
	if err != nil {
		return reattachFacts{}, err
	}
	if !samePath(pane.Cwd, project.Key) {
		return reattachFacts{}, fmt.Errorf("%w: pane %q works in %s, not the bound repository %s; a replacement primary works at the repository root",
			ErrDifferentRepository, pane.PaneID, pane.Cwd, project.Key)
	}
	view, err := c.agentGet(ctx, pane.PaneID)
	switch {
	case isMissingTarget(err):
		return reattachFacts{}, fmt.Errorf("%w: pane %q hosts no agent; start %s in it first", ErrAgentChanged, pane.PaneID, c.opts.AgentKind)
	case err != nil:
		return reattachFacts{}, err
	}
	switch {
	case view.Agent.PaneID != pane.PaneID:
		return reattachFacts{}, fmt.Errorf("%w: pane %q now resolves to %q", ErrAgentChanged, pane.PaneID, view.Agent.PaneID)
	case !strings.EqualFold(strings.TrimSpace(view.Agent.Agent), c.opts.AgentKind):
		return reattachFacts{}, fmt.Errorf("%w: pane %q runs %q, not %q", ErrAgentChanged, pane.PaneID, view.Agent.Agent, c.opts.AgentKind)
	case !samePath(view.Agent.Cwd, project.Key):
		return reattachFacts{}, fmt.Errorf("%w: the agent in pane %q works in %s, not the bound repository %s",
			ErrDifferentRepository, pane.PaneID, view.Agent.Cwd, project.Key)
	case view.Agent.WorkspaceID != pane.WorkspaceID:
		return reattachFacts{}, fmt.Errorf("%w: pane %q is in workspace %q but its agent reports %q",
			ErrAgentChanged, pane.PaneID, pane.WorkspaceID, view.Agent.WorkspaceID)
	case view.RefErr != nil:
		return reattachFacts{}, fmt.Errorf("%w: pane %q: %v", ErrSessionRefUnsupported, pane.PaneID, view.RefErr)
	case !view.HasRef:
		return reattachFacts{}, fmt.Errorf("%w: pane %q has not reported its OMP session yet; the integration reports it asynchronously, so retry shortly", ErrSessionUnavailable, pane.PaneID)
	}
	announcement, ok := task.AnnouncementOfRef(view.Ref)
	if !ok {
		return reattachFacts{}, fmt.Errorf("%w: pane %q reported an unusable session reference", ErrSessionRefUnsupported, pane.PaneID)
	}

	if err := c.requireUnclaimed(ctx, project.Key, pane.PaneID, announcement); err != nil {
		return reattachFacts{}, err
	}

	// A primary that is verified live is not lost, and one Herdr cannot be asked
	// about is not known to be lost: both refuse, and nothing is substituted.
	probe, err := c.probePrimary(ctx, project)
	if err != nil {
		return reattachFacts{}, fmt.Errorf("the recorded primary of %s could not be checked, so nothing was substituted for it: %w", project.Key, err)
	}
	if probe.Ours {
		return reattachFacts{}, fmt.Errorf("%w: pane %s is verified as the primary of %s; reattach replaces only a primary that is lost or unproven",
			ErrPrimaryLive, probe.Pane.PaneID, project.Key)
	}
	// The recorded session can only be compared with Herdr's reference in the same
	// form. When the replacement is a pane the project already records and Herdr
	// reports its session in the other form, nothing here can say whether it is the
	// same session: treating it as a new replacement would record the pane as
	// replacing itself and forget the notice already sent to it. Only a verified
	// handshake, which carries both forms, can show that it is a different session.
	if (project.LaunchPaneID == pane.PaneID || project.PublicPaneID == pane.PaneID) &&
		strings.TrimSpace(project.OMPSessionID) != "" && !comparableSession(project.OMPSessionID, view.Ref) {
		identity, err := c.identityOf(ctx, project.LaunchPaneID, project.OMPSessionID)
		if err != nil {
			return reattachFacts{}, fmt.Errorf("read the confirmed session of project %s: %w", project.Key, err)
		}
		if !identity.Proven || identity.proves(view.Ref) {
			return reattachFacts{}, fmt.Errorf("%w: pane %q reports its session as a %s, but project %s recorded %q, and without a verified handshake nothing here can tell whether that is the same session; nothing was changed - send a message in that session, then run the bind action to confirm it",
				ErrAgentChanged, pane.PaneID, view.Ref.Kind, project.Key, project.OMPSessionID)
		}
	}

	recorded := project.LaunchPaneID == pane.PaneID && project.PublicPaneID == pane.PaneID &&
		comparableSession(project.OMPSessionID, view.Ref) &&
		task.SameSessionReference(view.Ref.Kind, project.OMPSessionID, view.Ref.Value)
	// An attempt belongs to the pane and session it went to; for any other binding
	// it is stale and says nothing about this replacement. One on record for this
	// very pane must describe this very session in the same form, or the notice
	// policy cannot be applied and nothing is changed.
	notice := ""
	if attempt := project.ReattachNotice; recorded && attempt.PaneID == pane.PaneID && attempt.State != "" {
		if attempt.Kind != view.Ref.Kind || !task.SameSessionReference(view.Ref.Kind, attempt.Session, view.Ref.Value) {
			return reattachFacts{}, fmt.Errorf("%w: a notice attempt (%s) is on record for pane %q but for %s session %q, not the %s session %q it reports now; nothing was changed",
				ErrEvidenceChanged, attempt.State, pane.PaneID, attempt.Kind, attempt.Session, view.Ref.Kind, view.Ref.Value)
		}
		notice = attempt.State
	}

	plan := ReattachPlan{
		ProjectKey:  project.Key,
		PaneID:      pane.PaneID,
		WorkspaceID: view.Agent.WorkspaceID,
		TerminalID:  view.Agent.TerminalID,
		AgentName:   liveAgentName(view),
		Session:     view.Ref,
		Replaces: task.ReplacedPrimary{
			LaunchPaneID: project.LaunchPaneID,
			PublicPaneID: project.PublicPaneID,
			TerminalID:   project.TerminalID,
			WorkspaceID:  project.CurrentWorkspaceID,
			OMPSessionID: project.OMPSessionID,
		},
		Observed: truncateRunes(unverifiedPrimaryReason(probe), reattachObservedLimit),
		Recorded: recorded,
		Notice:   notice,
	}
	return reattachFacts{plan: plan, project: project, view: view}, nil
}

// requireUnclaimed refuses a replacement pane or session that a worker or another
// project owns. A pane or session the same project already records is not a
// claim: that is a reattach being completed.
func (c *Coordinator) requireUnclaimed(ctx context.Context, projectKey, paneID string, announced task.Announcement) error {
	binding, err := c.store.ByPane(ctx, paneID)
	switch {
	case err == nil:
		if err := ownedBy(binding, projectKey, "pane "+paneID); err != nil {
			return err
		}
	case errors.Is(err, state.ErrAmbiguous):
		return fmt.Errorf("%w: pane %q is claimed by more than one record", ErrAlreadyBound, paneID)
	case !errors.Is(err, state.ErrNotFound):
		return err
	}
	binding, err = c.store.ByOMPSession(ctx, announced)
	switch {
	case err == nil:
		return ownedBy(binding, projectKey, "session "+announced.Describe())
	case errors.Is(err, state.ErrAmbiguous):
		return fmt.Errorf("%w: session %s is claimed by more than one record", ErrAlreadyBound, announced.Describe())
	case errors.Is(err, state.ErrNotFound):
		return nil
	default:
		return err
	}
}

// ownedBy accepts a binding that is the project's own primary and refuses any
// other, naming what holds the pane or session.
func ownedBy(binding state.Binding, projectKey, what string) error {
	if binding.Role == state.RolePrimary && binding.Project.Key == projectKey {
		return nil
	}
	if binding.Role == state.RolePrimary {
		return fmt.Errorf("%w: %s is the primary of %s", ErrAlreadyBound, what, binding.Project.Key)
	}
	return fmt.Errorf("%w: %s belongs to task %s", ErrAlreadyBound, what, binding.Task.ID)
}

// Reattach moves a project to the replacement pane the user confirmed, and
// confirms the replacement the way bind confirms a new primary.
//
// confirmed is the plan PlanReattach returned and the user accepted. The facts are
// derived again and must still describe that plan; anything that changed since -
// a restarted session, a different pane, a primary that came back - is
// ErrEvidenceChanged and nothing is written.
//
// The returned project is what the store holds. When the error is nil the
// replacement is recorded and its extension announced the session Herdr reports.
// When it wraps ErrReattachPending the replacement is recorded but not confirmed:
// the project's primary attention says so, the inbox and workers are untouched,
// and bind completes the confirmation. Every other error means nothing was
// recorded.
func (c *Coordinator) Reattach(ctx context.Context, projectKey, paneID string, confirmed ReattachPlan) (task.Project, error) {
	if confirmed == (ReattachPlan{}) {
		return task.Project{}, fmt.Errorf("%w: a reattach needs the plan the user confirmed", ErrInvalidRequest)
	}
	facts, err := c.verifyReattach(ctx, projectKey, paneID)
	if err != nil {
		return task.Project{}, err
	}
	if !facts.plan.sameAs(confirmed) {
		return task.Project{}, fmt.Errorf("%w: the replacement or the recorded primary changed after the reattach was confirmed; review it again", ErrEvidenceChanged)
	}
	idle := strings.EqualFold(strings.TrimSpace(facts.view.Agent.AgentStatus), "idle")
	if !facts.plan.Recorded && !idle {
		// Refused before anything is written: the notice that lets the extension
		// announce itself is a turn, and a busy session cannot take it safely.
		return task.Project{}, fmt.Errorf("%w: pane %s reports %q; wait for it to be idle, then confirm again",
			ErrReplacementBusy, facts.plan.PaneID, facts.view.Agent.AgentStatus)
	}

	project := facts.project
	if !facts.plan.Recorded {
		project, err = c.recordReattach(ctx, facts)
		if err != nil {
			return task.Project{}, err
		}
	}
	return c.confirmReattach(ctx, project, facts, idle)
}

// recordReattach is the one write: a compare-and-swap of the project row against
// the primary the plan replaces. The displaced identities are recorded in the same
// write that records their replacement.
func (c *Coordinator) recordReattach(ctx context.Context, facts reattachFacts) (task.Project, error) {
	plan, old := facts.plan, facts.project
	now := time.Now().UnixMilli()
	var stored task.Project
	err := c.store.UpdateProject(ctx, old.Key, func(next *task.Project) error {
		if next.LaunchPaneID != old.LaunchPaneID || next.PublicPaneID != old.PublicPaneID ||
			next.OMPSessionID != old.OMPSessionID || next.TerminalID != old.TerminalID {
			return errSuperseded
		}
		next.Replaced = appendReplaced(next.Replaced, task.ReplacedPrimary{
			LaunchPaneID: next.LaunchPaneID,
			PublicPaneID: next.PublicPaneID,
			TerminalID:   next.TerminalID,
			WorkspaceID:  next.CurrentWorkspaceID,
			OMPSessionID: next.OMPSessionID,
			Reason:       plan.Observed,
			At:           now,
		})
		next.LaunchPaneID = plan.PaneID
		next.PublicPaneID = plan.PaneID
		next.TerminalID = plan.TerminalID
		next.RepoWorkspaceID = plan.WorkspaceID
		next.CurrentWorkspaceID = plan.WorkspaceID
		next.OMPSessionID = plan.Session.Value
		next.AgentName = plan.AgentName
		next.PrimaryAttention = truncateRunes(fmt.Sprintf(
			"reattached to pane %s (session %s): the replacement has not confirmed its handshake yet",
			plan.PaneID, plan.Session.Value), primaryAttentionLimit)
		// A new binding starts with no notice attempt: the attempt belongs to the
		// pane and session it was made for.
		next.ReattachNotice = task.NoticeAttempt{}
		stored = *next
		return nil
	})
	switch {
	case errors.Is(err, errSuperseded):
		return task.Project{}, fmt.Errorf("%w: the primary of %s changed while the reattach was being confirmed", ErrEvidenceChanged, old.Key)
	case errors.Is(err, state.ErrPaneTaken):
		return task.Project{}, fmt.Errorf("%w: record the reattached primary of %s: %w", ErrAlreadyBound, old.Key, err)
	case err != nil:
		return task.Project{}, fmt.Errorf("record the reattached primary of %s: %w", old.Key, err)
	}
	return stored, nil
}

// appendReplaced adds one displaced primary and keeps the most recent
// MaxReplacedPrimaries, oldest first.
func appendReplaced(existing []task.ReplacedPrimary, replaced task.ReplacedPrimary) []task.ReplacedPrimary {
	next := append(append([]task.ReplacedPrimary(nil), existing...), replaced)
	if extra := len(next) - task.MaxReplacedPrimaries; extra > 0 {
		next = next[extra:]
	}
	return next
}

// confirmReattach completes the handshake for a recorded replacement. A session
// that already announced itself needs no notice. Otherwise the replacement gets
// one short notice - the turn in which its extension resolves the new binding and
// announces - and the coordinator waits for the announcement, exactly as bind does.
//
// The notice is an input injection into the user's session, so its attempt is
// persisted (task.Project.ReattachNotice) before it is sent and the same rule the
// inbox follows applies across retries, not only within one call: a notice that
// Herdr acknowledged or whose outcome is unknown is never sent again. A later
// reattach only waits for the handshake; if it still has not come, the user sends
// the session a message and bind confirms it. Only a refusal that proves nothing
// was written withdraws the attempt, so that a retry may send one.
func (c *Coordinator) confirmReattach(ctx context.Context, project task.Project, facts reattachFacts, idle bool) (task.Project, error) {
	plan := facts.plan
	if err := c.confirmReference(ctx, plan.PaneID, facts.view.Ref); err == nil {
		return c.finishReattach(ctx, project, facts.view)
	} else if !handshakePending(err) {
		return c.reattachPending(ctx, project, fmt.Errorf("the readiness handshake could not be checked, so no notice was sent: %w", err))
	}
	if plan.Notice == task.NoticeSent || plan.Notice == task.NoticeUncertain {
		view, err := c.awaitReattachHandshake(ctx, project, plan)
		if err != nil {
			return c.reattachPending(ctx, project, fmt.Errorf("a notice already went to pane %s (%s), so none was sent again: %w",
				plan.PaneID, plan.Notice, err))
		}
		return c.finishReattach(ctx, project, view)
	}
	if !idle {
		return c.reattachPending(ctx, project, fmt.Errorf("%w: pane %s reports %q, so no notice was sent",
			ErrReplacementBusy, plan.PaneID, facts.view.Agent.AgentStatus))
	}

	// Intent before the side effect: from here on the notice may exist, so a crash
	// or an unreadable answer leaves an attempt that forbids another one.
	attempt := task.NoticeAttempt{PaneID: plan.PaneID, Session: plan.Session.Value, Kind: plan.Session.Kind, State: task.NoticeUncertain, At: time.Now().UnixMilli()}
	if err := c.recordNotice(ctx, project, attempt); err != nil {
		return c.reattachPending(ctx, project, fmt.Errorf("no notice was sent: %w", err))
	}
	callCtx, cancel := c.callContext(ctx)
	_, err := c.herdr.AgentPrompt(callCtx, plan.PaneID, reattachNotice(plan))
	cancel()
	var noticeErr error
	switch {
	case err == nil:
		// A failed upgrade leaves the attempt uncertain, the stricter reading: it can
		// only withhold a later notice, never allow one.
		attempt.State = task.NoticeSent
		noticeErr = c.recordNotice(ctx, project, attempt)
	case errors.Is(err, herdr.ErrAmbiguous):
		// The notice may already be in the session: the attempt stays uncertain and
		// the wait below still runs.
	case isNonDelivery(err):
		// Herdr refused before writing anything, so no notice exists and a later
		// reattach may send one. If the withdrawal cannot be recorded the attempt
		// stays uncertain, which only withholds a retry.
		cause := fmt.Errorf("the replacement refused the notice before any input was written: %w", err)
		if withdrawErr := c.recordNotice(ctx, project, task.NoticeAttempt{}); withdrawErr != nil {
			cause = errors.Join(cause, withdrawErr)
		}
		return c.reattachPending(ctx, project, cause)
	default:
		return c.reattachPending(ctx, project, fmt.Errorf("the notice to the replacement failed without proof that nothing was written, so it is recorded as uncertain and will not be resent; inspect `agent read`: %w", err))
	}

	view, err := c.awaitReattachHandshake(ctx, project, plan)
	if err != nil {
		return c.reattachPending(ctx, project, errors.Join(err, noticeErr))
	}
	return c.finishReattach(ctx, project, view)
}

// awaitReattachHandshake waits for the replacement's extension to announce the
// session Herdr reports, exactly as bind waits for a new primary.
func (c *Coordinator) awaitReattachHandshake(ctx context.Context, project task.Project, plan ReattachPlan) (agentView, error) {
	return c.awaitAgentReady(ctx, plan.PaneID, plan.PaneID, project.Key, []string{plan.WorkspaceID}, func(view agentView) error {
		return c.store.UpdateProject(ctx, project.Key, func(next *task.Project) error {
			if next.LaunchPaneID != plan.PaneID {
				return fmt.Errorf("%w: the primary binding changed to pane %q while the reattach was confirmed", ErrPrimaryNotLive, next.LaunchPaneID)
			}
			next.TerminalID = view.Agent.TerminalID
			next.CurrentWorkspaceID = view.Agent.WorkspaceID
			return nil
		})
	})
}

// recordNotice is a compare-and-swap of the notice attempt against the binding it
// was made for. The zero attempt withdraws it.
func (c *Coordinator) recordNotice(ctx context.Context, project task.Project, attempt task.NoticeAttempt) error {
	err := c.store.UpdateProject(ctx, project.Key, func(next *task.Project) error {
		if next.LaunchPaneID != project.LaunchPaneID || next.PublicPaneID != project.PublicPaneID ||
			next.OMPSessionID != project.OMPSessionID {
			return errSuperseded
		}
		next.ReattachNotice = attempt
		return nil
	})
	switch {
	case errors.Is(err, errSuperseded):
		return fmt.Errorf("%w: the primary of %s changed while the notice attempt was being recorded", ErrEvidenceChanged, project.Key)
	case err != nil:
		return fmt.Errorf("record the notice attempt for %s: %w", project.Key, err)
	}
	return nil
}

// finishReattach accepts a confirmed handshake only if it proves the session the
// project now records, and then retires the unconfirmed marker and the notice
// attempt in one write: a confirmed replacement has nothing pending.
func (c *Coordinator) finishReattach(ctx context.Context, project task.Project, view agentView) (task.Project, error) {
	identity, err := c.identityOf(ctx, project.LaunchPaneID, project.OMPSessionID)
	if err != nil {
		return c.reattachPending(ctx, project, err)
	}
	if !identity.proves(view.Ref) {
		return c.reattachPending(ctx, project, fmt.Errorf("%w: pane %q reports %s session %q, which the announced handshake does not prove for the recorded %q",
			ErrAgentChanged, project.LaunchPaneID, view.Ref.Kind, view.Ref.Value, project.OMPSessionID))
	}
	err = c.store.UpdateProject(ctx, project.Key, func(next *task.Project) error {
		if next.LaunchPaneID != project.LaunchPaneID || next.PublicPaneID != project.PublicPaneID {
			// A newer binding owns the row; this confirmation describes an older one.
			return errSuperseded
		}
		next.PrimaryAttention = ""
		next.ReattachNotice = task.NoticeAttempt{}
		return nil
	})
	if err != nil && !errors.Is(err, errSuperseded) {
		return project, fmt.Errorf("retire the unconfirmed marker of %s: %w", project.Key, err)
	}
	if stored, readErr := c.store.Project(ctx, project.Key); readErr == nil {
		return stored, nil
	}
	project.PrimaryAttention, project.ReattachNotice = "", task.NoticeAttempt{}
	return project, nil
}

// reattachPending leaves the recorded replacement as it is, says on the project
// why it is not confirmed, and returns that as ErrReattachPending: a provisional
// write is never reported as success.
func (c *Coordinator) reattachPending(ctx context.Context, project task.Project, cause error) (task.Project, error) {
	reason := fmt.Sprintf("reattached to pane %s but not confirmed: %v", project.LaunchPaneID, cause)
	err := fmt.Errorf("%w: pane %s is recorded as the primary of %s but is not confirmed (%w); send a message in that session, then run the bind action to confirm it",
		ErrReattachPending, project.LaunchPaneID, project.Key, cause)
	if recordErr := c.recordPrimaryAttention(ctx, project, reason, true); recordErr != nil {
		err = errors.Join(err, recordErr)
	}
	if stored, readErr := c.store.Project(ctx, project.Key); readErr == nil {
		project = stored
	}
	return project, err
}

// reattachNotice is the one turn a reattach sends the replacement session. It
// carries no instruction beyond saying what happened; its purpose is the turn, in
// which the extension resolves the new binding and announces the session.
func reattachNotice(plan ReattachPlan) string {
	text := fmt.Sprintf("orchestrator: this session is now the primary of %s", plan.ProjectKey)
	if replaced := strings.TrimSpace(plan.Replaces.OMPSessionID); replaced != "" && !plan.Recorded {
		text += ", replacing session " + replaced
	}
	text += ". Nothing needs doing; this note only lets the orchestrator confirm this session."
	return truncateRunes(text, 600)
}

// sessionKindOf is the form a recorded session identity is held in: a session
// file path (it has path separators) or an id.
func sessionKindOf(identity string) string {
	if strings.ContainsAny(strings.TrimSpace(identity), `/\`) {
		return task.SessionKindPath
	}
	return task.SessionKindID
}

// comparableSession reports whether a recorded session identity can be compared
// with Herdr's reference as it is: both must be in the same form. A path and an
// id name the same session only through a verified handshake, which carries both.
func comparableSession(recorded string, ref task.SessionRef) bool {
	return sessionKindOf(recorded) == ref.Kind
}

// handshakePending reports whether a failed confirmation only means the extension
// has not announced the session Herdr reports: there is no announcement for the
// pane yet (state.ErrNotFound), or the one on record is an earlier session's
// (state.ErrMismatch), which is what a pane that was restarted still holds.
//
// Every other failure fails closed. That includes ErrAgentChanged, which
// confirmReference returns after the confirmation was recorded when the announced
// session still is not the one Herdr reports - a contradiction, not an absence -
// and any failure of the readiness store itself. Neither says the extension has
// not announced, so nothing may be injected into the session on top of them.
func handshakePending(err error) bool {
	return errors.Is(err, state.ErrNotFound) || errors.Is(err, state.ErrMismatch)
}
