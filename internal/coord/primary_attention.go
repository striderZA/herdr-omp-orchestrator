package coord

// Issue #17: a primary that is gone, or that nothing can prove, is a fact the
// board must be able to show. The founding rule is that a primary loss does not
// destroy workers - "the board marks its absence and retains inbox events" - and
// the board is a read-only projection of persisted facts, so the observation has
// to be persisted by whoever made it.
//
// The fact is task.Project.PrimaryAttention: a reason, empty while the primary is
// verified. Reconciliation and the paths that already probe the primary write it
// (a flush, a primary pane move, a reopen of the binding) and a verified live
// primary clears it. It is deliberately not a lifecycle state: writing it changes
// no task, no stage and no inbox note, and it never starts, closes or replaces
// anything. Re-establishing a lost primary stays the user's explicit action.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/task"
)

// primaryAttentionLimit bounds the stored reason. A refusal can carry a long
// diagnostic; the fact has to stay one readable sentence for a board line.
const primaryAttentionLimit = 400

// errPrimaryAttentionUnchanged is the mutation's way of saying the stored fact
// already reads as wanted, so the store writes nothing for it.
var errPrimaryAttentionUnchanged = errors.New("coord: the primary attention already reads as wanted")

// recordPrimaryAttention persists why the project's primary is absent or
// unproven.
//
// With replace, the reason is the latest evidence of a pass that searched for the
// primary (a startup reconcile, a reopen) and overwrites a different stored one.
// Without it the caller is an incidental observer - a flush that happened to
// probe - and never rewrites a reason already recorded: two observers describing
// the same absence in different words would otherwise trade writes forever.
// Recording the same reason twice writes nothing.
//
// The write is a compare-and-swap on the pane identity the observation was made
// against. A move keeps the launch pane and changes the public one, so both are
// checked: a newer binding or a newer move recorded in between owns the fact, and
// this stale observation - recording or clearing - is dropped.
func (c *Coordinator) recordPrimaryAttention(ctx context.Context, project task.Project, reason string, replace bool) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: a primary attention needs a reason", ErrInvalidRequest)
	}
	return c.writePrimaryAttention(ctx, project, truncateRunes(reason, primaryAttentionLimit), replace)
}

// clearPrimaryAttention retires the fact after a verified live observation of the
// primary. Clearing an already-clear fact writes nothing.
func (c *Coordinator) clearPrimaryAttention(ctx context.Context, project task.Project) error {
	return c.writePrimaryAttention(ctx, project, "", true)
}

func (c *Coordinator) writePrimaryAttention(ctx context.Context, project task.Project, reason string, replace bool) error {
	err := c.store.UpdateProject(ctx, project.Key, func(next *task.Project) error {
		switch {
		case next.LaunchPaneID != project.LaunchPaneID || next.PublicPaneID != project.PublicPaneID:
			return errSuperseded
		case next.PrimaryAttention == reason:
			return errPrimaryAttentionUnchanged
		case !replace && reason != "" && next.PrimaryAttention != "":
			return errPrimaryAttentionUnchanged
		}
		next.PrimaryAttention = reason
		return nil
	})
	if errors.Is(err, errSuperseded) || errors.Is(err, errPrimaryAttentionUnchanged) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record the primary attention of project %s: %w", project.Key, err)
	}
	return nil
}

// settlePrimaryAttention applies what one fresh probe of the recorded primary
// proved: a verified primary retires the fact, and any other answer records why
// the primary could not be verified (keeping a reason that is already there).
func (c *Coordinator) settlePrimaryAttention(ctx context.Context, project task.Project, probe paneProbe) error {
	if probe.Ours {
		return c.clearPrimaryAttention(ctx, project)
	}
	return c.recordPrimaryAttention(ctx, project, unverifiedPrimaryReason(probe), false)
}

// unverifiedPrimaryReason is the sentence for a probe of the recorded primary
// pane that did not prove it.
func unverifiedPrimaryReason(probe paneProbe) string {
	why := strings.TrimSpace(probe.Refusal)
	if why == "" {
		why = "nothing in Herdr's live state proves it"
	}
	return "the recorded primary cannot be verified: " + why
}

// snapshotPrimaryReason is the sentence for a reconcile pass whose snapshot
// search found no pane that proves the primary and none that refutes it.
//
// It speaks about the recorded primary pane, not about whichever other pane
// happens to work in the repository: a user's shell in the checkout is a
// candidate the search must examine, but it is not why the primary is missing.
func snapshotPrimaryReason(project task.Project, probes []paneProbe) string {
	recorded, found := firstPane(project.PublicPaneID, project.LaunchPaneID)
	if !found {
		return "the project records no primary pane"
	}
	for _, probe := range probes {
		if probe.Snapshot.PaneID == strings.TrimSpace(project.PublicPaneID) ||
			probe.Snapshot.PaneID == strings.TrimSpace(project.LaunchPaneID) {
			return unverifiedPrimaryReason(probe)
		}
	}
	return fmt.Sprintf("the recorded primary pane %s is absent from Herdr's snapshot", recorded)
}

// refuseReopen settles a bind that found the recorded primary definitely not
// live, and records the same finding for the board. Both are durable before the
// refusal is returned; nothing is started in the primary's place.
func (c *Coordinator) refuseReopen(ctx context.Context, existing task.Project, reason string, cause error) error {
	err := c.settleProject(ctx, existing.Key, true, cause)
	if recordErr := c.recordPrimaryAttention(ctx, existing, reason, true); recordErr != nil {
		return errors.Join(err, recordErr)
	}
	return err
}
