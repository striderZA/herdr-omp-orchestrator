package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// Errors of a re-point. Callers compare them with errors.Is.
var (
	// ErrRepointRefused refuses a re-point of a dependent that has started, that
	// does not wait on the superseded task, or whose superseded task names no
	// successor.
	ErrRepointRefused = errors.New("coord: the dependent cannot be re-pointed")
	// ErrRepointChanged refuses a re-point whose successor or dependents changed
	// after the preview the user confirmed.
	ErrRepointChanged = errors.New("coord: the re-point changed after its preview")
	// ErrRepointCycle refuses a re-point that would make the dependencies cyclic.
	ErrRepointCycle = errors.New("coord: the re-point would create a dependency cycle")
)

// maxRepointDependents bounds one re-point. It is the same limit a dispatch puts on
// its dependency list.
const maxRepointDependents = maxDependencies

// RepointPreviewParams is the repoint_preview body: the superseded task and the
// queued dependents the user wants to wait on its successor instead.
type RepointPreviewParams struct {
	SupersededTaskID string   `json:"supersededTaskId"`
	DependentIDs     []string `json:"dependentIds"`
}

// RepointDependent is one named dependent with the revision the preview showed.
type RepointDependent struct {
	TaskID           string `json:"taskId"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

// RepointParams is the repoint body: the preview the user confirmed, with the
// successor it named and each dependent at the revision it showed.
type RepointParams struct {
	SupersededTaskID string             `json:"supersededTaskId"`
	SuccessorTaskID  string             `json:"successorTaskId"`
	Dependents       []RepointDependent `json:"dependents"`
}

// RepointCandidate is one named dependent as the preview shows it: a queued task
// that waits on the superseded task, at the revision the confirmation binds to.
type RepointCandidate struct {
	TaskID       string     `json:"taskId"`
	Title        string     `json:"title"`
	Stage        task.Stage `json:"stage"`
	Revision     int64      `json:"revision"`
	Dependencies []string   `json:"dependencies"`
}

// RepointPreview is the read-only plan a re-point is confirmed against. It shows
// the superseded task and the successor it names, whether that successor is
// accepting (only then are the re-pointed dependents released), and each dependent
// as it stands.
type RepointPreview struct {
	SupersededTaskID string             `json:"supersededTaskId"`
	SuccessorTaskID  string             `json:"successorTaskId"`
	SuccessorStage   task.Stage         `json:"successorStage"`
	SuccessorOutcome string             `json:"successorOutcome,omitempty"`
	Accepting        bool               `json:"accepting"`
	Dependents       []RepointCandidate `json:"dependents"`
}

// RepointResult is the answer to one recorded re-point.
type RepointResult struct {
	SupersededTaskID string   `json:"supersededTaskId"`
	SuccessorTaskID  string   `json:"successorTaskId"`
	TaskIDs          []string `json:"taskIds"`
}

// repointPlan is the validated plan that a preview shows and a write checks again.
type repointPlan struct {
	superseded task.Task
	successor  task.Task
	dependents []task.Task
}

// RepointPreview returns the read-only plan for re-pointing named queued dependents
// of a superseded task to its successor. It writes nothing, and it refuses what
// the write would refuse.
func (c *Coordinator) RepointPreview(ctx context.Context, primaryLaunchPaneID string, p RepointPreviewParams) (RepointPreview, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return RepointPreview{}, err
	}
	plan, err := c.planRepoint(ctx, project, p.SupersededTaskID, p.DependentIDs)
	if err != nil {
		return RepointPreview{}, err
	}
	return previewOf(plan), nil
}

func previewOf(plan repointPlan) RepointPreview {
	view := RepointPreview{
		SupersededTaskID: plan.superseded.ID,
		SuccessorTaskID:  plan.successor.ID,
		SuccessorStage:   task.StageOf(plan.successor),
		SuccessorOutcome: plan.successor.Outcome,
		Accepting:        task.StageOf(plan.successor) == task.Done && plan.successor.Accepted(),
	}
	for _, x := range plan.dependents {
		view.Dependents = append(view.Dependents, RepointCandidate{
			TaskID:       x.ID,
			Title:        x.Title,
			Stage:        task.StageOf(x),
			Revision:     x.Revision,
			Dependencies: slices.Clone(x.Dependencies),
		})
	}
	return view
}

// Repoint records a re-point the user confirmed against a preview. The superseded
// task, the successor it names and each dependent's revision must still match what
// was confirmed, and the plan is checked again inside the store's transaction, which
// writes every named dependent or none. The queue is then drained, so a dependent
// whose successor already accepted starts now. A superseded decision released
// nothing by itself; only this action and an accepting successor do.
func (c *Coordinator) Repoint(ctx context.Context, primaryLaunchPaneID string, p RepointParams) (RepointResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return RepointResult{}, err
	}
	ids := make([]string, 0, len(p.Dependents))
	for _, d := range p.Dependents {
		ids = append(ids, d.TaskID)
	}
	plan, err := c.planRepoint(ctx, project, p.SupersededTaskID, ids)
	if err != nil {
		return RepointResult{}, err
	}
	if strings.TrimSpace(p.SuccessorTaskID) != plan.successor.ID {
		return RepointResult{}, fmt.Errorf("%w: task %s names %s as its successor, not the %q you confirmed",
			ErrRepointChanged, plan.superseded.ID, plan.successor.ID, strings.TrimSpace(p.SuccessorTaskID))
	}
	targets := make([]state.RepointTarget, 0, len(plan.dependents))
	for i, dependent := range plan.dependents {
		if confirmed := p.Dependents[i].ExpectedRevision; confirmed != dependent.Revision {
			return RepointResult{}, fmt.Errorf("%w: task %s is at revision %d, not the %d you confirmed",
				ErrRepointChanged, dependent.ID, dependent.Revision, confirmed)
		}
		targets = append(targets, state.RepointTarget{TaskID: dependent.ID, ExpectedRevision: dependent.Revision})
	}

	updated, err := c.store.RepointDependents(ctx, plan.superseded.ID, plan.successor.ID, targets)
	switch {
	case errors.Is(err, state.ErrConflict):
		return RepointResult{}, fmt.Errorf("%w: %w", ErrRepointChanged, err)
	case errors.Is(err, state.ErrDependentUnavailable):
		return RepointResult{}, fmt.Errorf("%w: %w", ErrRepointRefused, err)
	case errors.Is(err, state.ErrDependencyCycle):
		return RepointResult{}, fmt.Errorf("%w: %w", ErrRepointCycle, err)
	case errors.Is(err, state.ErrSuccessorUnavailable):
		return RepointResult{}, fmt.Errorf("%w: %w", ErrSuccessorUnknown, err)
	case err != nil:
		return RepointResult{}, err
	}
	result := RepointResult{SupersededTaskID: plan.superseded.ID, SuccessorTaskID: plan.successor.ID}
	for _, x := range updated {
		result.TaskIDs = append(result.TaskIDs, x.ID)
	}
	if _, err := c.DrainQueued(ctx, project.Key); err != nil {
		return result, fmt.Errorf("the re-point was recorded, but the queued tasks of %s could not be drained: %w", project.Key, err)
	}
	return result, nil
}

// planRepoint validates a re-point against the records as they stand. The
// superseded task must belong to the project and name a successor; each dependent
// must be a queued task of the project that waits on the superseded task; and the
// project's dependencies must stay acyclic with the re-points applied. Dependents
// come back in the order they were named.
func (c *Coordinator) planRepoint(ctx context.Context, project task.Project, supersededID string, dependentIDs []string) (repointPlan, error) {
	superseded, err := c.ownedTask(ctx, project, supersededID)
	if err != nil {
		return repointPlan{}, err
	}
	if superseded.Outcome != task.OutcomeSuperseded || superseded.Successor == "" {
		return repointPlan{}, fmt.Errorf("%w: task %s names no successor, so there is nothing to re-point to", ErrRepointRefused, superseded.ID)
	}
	successor, err := c.successorTask(ctx, project.Key, superseded.Successor)
	if err != nil {
		return repointPlan{}, err
	}
	ids, err := normalizeDependentIDs(dependentIDs)
	if err != nil {
		return repointPlan{}, err
	}
	plan := repointPlan{superseded: superseded, successor: successor}
	rewritten := make(map[string][]string, len(ids))
	for _, id := range ids {
		dependent, err := c.ownedTask(ctx, project, id)
		if err != nil {
			return repointPlan{}, err
		}
		if stage := task.StageOf(dependent); stage != task.Queued {
			return repointPlan{}, fmt.Errorf("%w: task %s is %s, not queued, so its dependencies were checked at admission",
				ErrRepointRefused, dependent.ID, stage)
		}
		if !slices.Contains(dependent.Dependencies, superseded.ID) {
			return repointPlan{}, fmt.Errorf("%w: task %s does not wait on %s", ErrRepointRefused, dependent.ID, superseded.ID)
		}
		plan.dependents = append(plan.dependents, dependent)
		rewritten[dependent.ID] = task.ReplaceDependency(dependent.Dependencies, superseded.ID, successor.ID)
	}
	tasks, err := c.projectTasks(ctx, project.Key)
	if err != nil {
		return repointPlan{}, err
	}
	if cycle := task.DependencyCycle(tasks, rewritten); cycle != nil {
		return repointPlan{}, fmt.Errorf("%w: %s", ErrRepointCycle, strings.Join(append(slices.Clone(cycle), cycle[0]), " -> "))
	}
	return plan, nil
}

// normalizeDependentIDs trims and bounds the dependents a re-point names. A
// re-point names between one and maxRepointDependents distinct tasks.
func normalizeDependentIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: a re-point names at least one dependent", ErrInvalidRequest)
	}
	if len(ids) > maxRepointDependents {
		return nil, fmt.Errorf("%w: a re-point names more than %d dependents", ErrInvalidRequest, maxRepointDependents)
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, raw := range ids {
		id, err := normalizeSuccessor(raw)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, fmt.Errorf("%w: a dependent id is blank", ErrInvalidRequest)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: dependent %s is named twice", ErrInvalidRequest, id)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}
