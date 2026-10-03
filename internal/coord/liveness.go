package coord

import (
	"strings"

	"herdr-omp-orchestrator/internal/task"
)

// The two attentions a liveness finding records on a task whose brief was
// submitted. Like questionAttentionPrefix, each prefix is the marker that makes
// the attention distinguishable from every other reason a task needs a human, and
// it is what lets a later verified observation retire exactly what a liveness
// finding raised and nothing else: a worker question, a blocker and a dispatch
// failure all belong to someone else and are never cleared here.
//
// Neither is evidence that the worker failed or finished. They say the plugin
// cannot currently see the worker, which is a reason for a human to look, not a
// verdict - so their text says so, and nothing is started or sent in response.
const (
	absentAttentionPrefix  = "worker not running: "
	unknownAttentionPrefix = "worker state unknown: "
)

// absentAttention is the attention a worker whose pane Herdr no longer lists
// leaves on its task.
const absentAttention = absentAttentionPrefix + "its pane is absent from Herdr's snapshot. " +
	"That is not a report that it failed or finished: inspect it before deciding; nothing was started in its place and the checkout was left as it is"

// unknownAttention is the attention a worker whose Herdr activity is "unknown"
// leaves on its task.
const unknownAttention = unknownAttentionPrefix + "Herdr reports its activity as unknown. " +
	"That is not a report that it failed or finished: inspect it before deciding; this clears once a verified status shows it live again"

// isLivenessAttention reports whether an attention was raised by a liveness
// finding, which is the only attention a verified live observation or the
// worker's own final report may clear.
func isLivenessAttention(attention string) bool {
	attention = strings.TrimSpace(attention)
	return strings.HasPrefix(attention, absentAttentionPrefix) || strings.HasPrefix(attention, unknownAttentionPrefix)
}

// raiseLiveness records a liveness attention on a task that has a worker to
// lose, and reports whether it changed anything.
//
// It never touches a task that is closed or reviewed, one that has not yet
// submitted its brief (the dispatch step machine owns provisioning), or one that
// already needs a human for another reason: a blocker or a worker question is
// never replaced. A liveness attention of the other kind is replaced, because
// the newer finding is the one that is true now. It is deliberately idempotent
// so it can run inside a compare-and-swap retry.
func raiseLiveness(x *task.Task, attention string) bool {
	switch {
	case x.Phase != task.PromptSubmitted,
		strings.TrimSpace(x.Disposition) != "",
		isReviewed(*x),
		strings.TrimSpace(x.Blocker) != "":
		return false
	case x.Attention == attention:
		return false
	case strings.TrimSpace(x.Attention) != "" && !isLivenessAttention(x.Attention):
		return false
	}
	x.Attention = attention
	return true
}

// clearLiveness retires a liveness attention and reports whether there was one.
func clearLiveness(x *task.Task) bool {
	if !isLivenessAttention(x.Attention) {
		return false
	}
	x.Attention = ""
	return true
}

// settleLiveness applies one verified observation of a live worker to the
// attention a liveness finding owns: an "unknown" activity raises it, and any
// other recognized activity shows the worker live again and clears it. An
// activity Herdr did not name says nothing either way.
func settleLiveness(x *task.Task, activity string) bool {
	switch {
	case activity == "unknown":
		return raiseLiveness(x, unknownAttention)
	case herdrAgentStatuses[activity]:
		return clearLiveness(x)
	default:
		return false
	}
}

// livenessEffect says in words what a status observation did to a task's
// attention, for the note that announces it. Without one, the observation was
// activity only.
func livenessEffect(before, after task.Task) string {
	switch {
	case before.Attention == after.Attention:
		return ""
	case after.Attention != "":
		return "Herdr cannot tell what the worker is doing, so the task now needs input; that is not a report that it failed or finished"
	default:
		return "the worker is observable again, so the needs-input its unknown or absent state raised is cleared"
	}
}
