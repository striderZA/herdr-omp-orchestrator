package coord

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

var (
	ErrForgetRefused      = errors.New("coord: the project cannot be forgotten yet")
	ErrForgetPlanChanged  = errors.New("coord: the project changed since the plan was shown")
	ErrForgetNotConfirmed = errors.New("coord: a forget needs the plan the user confirmed")
)

type ForgetPlan struct {
	ProjectKey                                  string
	FolderName                                  string
	Tasks, Decided                              int
	Tickets                                     int
	NotesDelivered, NotesPending, NotesBlocking int
	Blockers                                    []string
	Stays                                       []string
	Fingerprint                                 string
}

type ForgetResult struct {
	Deleted state.ForgetCounts
	Stays   []string
}

// PlanForget returns a read-only snapshot of the project facts that would be
// removed by Forget and the external resources that will remain.

func (c *Coordinator) PlanForget(ctx context.Context, projectKey string) (ForgetPlan, error) {
	key := strings.TrimSpace(projectKey)
	if key == "" {
		return ForgetPlan{}, fmt.Errorf("%w: project key is required", ErrInvalidRequest)
	}
	facts, err := c.store.ForgetFacts(ctx, key)
	if errors.Is(err, state.ErrNotFound) {
		return ForgetPlan{}, fmt.Errorf("%w: %s", ErrProjectUnknown, key)
	}
	if err != nil {
		return ForgetPlan{}, err
	}
	fingerprint, err := task.ForgetFingerprint(facts)
	if err != nil {
		return ForgetPlan{}, fmt.Errorf("fingerprint forget facts: %w", err)
	}
	plan := ForgetPlan{
		ProjectKey:  facts.Project.Key,
		FolderName:  filepath.Base(filepath.Clean(facts.Project.Key)),
		Tickets:     len(facts.Tickets),
		Blockers:    task.ForgetBlockers(facts),
		Fingerprint: fingerprint,
	}
	for _, item := range facts.Tasks {
		plan.Tasks++
		if item.Disposition != "" {
			plan.Decided++
		}
	}
	for _, note := range facts.Notes {
		switch note.State() {
		case task.DeliveryDelivered:
			plan.NotesDelivered++
		case task.DeliveryPending:
			plan.NotesPending++
		case task.DeliverySending, task.DeliveryUncertain:
			plan.NotesBlocking++
		}
	}
	if len(plan.Blockers) != 0 && facts.Project.PrimaryAttention != "" {
		plan.Blockers = append(plan.Blockers, "the primary is not verified: reattach it first to record outcomes or deliver notes")
	}
	plan.Stays = forgetStays(facts)
	return plan, nil
}

// Forget deletes the project's durable facts only when the supplied snapshot
// fingerprint still matches and all refusal conditions are clear.

func (c *Coordinator) Forget(ctx context.Context, projectKey string, confirmed ForgetPlan) (ForgetResult, error) {
	if strings.TrimSpace(confirmed.Fingerprint) == "" {
		return ForgetResult{}, ErrForgetNotConfirmed
	}
	key := strings.TrimSpace(projectKey)
	if key == "" {
		return ForgetResult{}, fmt.Errorf("%w: project key is required", ErrInvalidRequest)
	}
	facts, err := c.store.ForgetFacts(ctx, key)
	if errors.Is(err, state.ErrNotFound) {
		return ForgetResult{}, fmt.Errorf("%w: %s", ErrProjectUnknown, key)
	}
	if err != nil {
		return ForgetResult{}, err
	}
	fingerprint, err := task.ForgetFingerprint(facts)
	if err != nil {
		return ForgetResult{}, fmt.Errorf("fingerprint forget facts: %w", err)
	}
	if fingerprint != confirmed.Fingerprint {
		return ForgetResult{}, fmt.Errorf("%w: project %q", ErrForgetPlanChanged, key)
	}
	if blockers := task.ForgetBlockers(facts); len(blockers) != 0 {
		if facts.Project.PrimaryAttention != "" {
			blockers = append(blockers, "the primary is not verified: reattach it first to record outcomes or deliver notes")
		}
		return ForgetResult{}, fmt.Errorf("%w: %s", ErrForgetRefused, strings.Join(blockers, "; "))
	}
	counts, err := c.store.ForgetProject(ctx, key, confirmed.Fingerprint)
	if errors.Is(err, state.ErrConflict) {
		return ForgetResult{}, fmt.Errorf("%w: project %q", ErrForgetPlanChanged, key)
	}
	if errors.Is(err, state.ErrForgetBlocked) {
		return ForgetResult{}, fmt.Errorf("%w: %v", ErrForgetRefused, err)
	}
	if errors.Is(err, state.ErrNotFound) {
		return ForgetResult{}, fmt.Errorf("%w: %s", ErrProjectUnknown, key)
	}
	if err != nil {
		return ForgetResult{}, err
	}
	return ForgetResult{Deleted: counts, Stays: forgetStays(facts)}, nil
}

func forgetStays(facts task.ForgetFacts) []string {
	stays := []string{
		"repository: " + facts.Project.Key,
		"branches: Git branches are unchanged",
		"worktree paths: Git worktrees are unchanged",
		"Herdr workspaces, panes, and agent sessions are unchanged",
	}
	if facts.Project.CurrentWorkspaceID != "" {
		stays = append(stays, "workspace: "+facts.Project.CurrentWorkspaceID)
	}
	if facts.Project.RepoWorkspaceID != "" && facts.Project.RepoWorkspaceID != facts.Project.CurrentWorkspaceID {
		stays = append(stays, "repository workspace: "+facts.Project.RepoWorkspaceID)
	}
	if facts.Project.TerminalID != "" {
		stays = append(stays, "terminal: "+facts.Project.TerminalID)
	}
	panes := make([]string, 0, len(facts.Tasks)+1)
	if facts.Project.PublicPaneID != "" {
		panes = append(panes, facts.Project.PublicPaneID)
	}
	if facts.Project.LaunchPaneID != "" {
		panes = append(panes, facts.Project.LaunchPaneID)
	}
	for _, item := range facts.Tasks {
		if item.Branch != "" {
			stays = append(stays, "branch: "+item.Branch)
		}
		if item.WorktreePath != "" {
			stays = append(stays, "worktree: "+item.WorktreePath)
		}
		if item.PublicPaneID != "" {
			panes = append(panes, item.PublicPaneID)
		}
		if item.LaunchPaneID != "" {
			panes = append(panes, item.LaunchPaneID)
		}
	}
	seen := make(map[string]struct{}, len(panes))
	for _, pane := range panes {
		if _, ok := seen[pane]; ok {
			continue
		}
		seen[pane] = struct{}{}
		stays = append(stays, "pane: "+pane)
	}
	return stays
}
