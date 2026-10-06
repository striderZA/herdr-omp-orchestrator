package state

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestBindIntentClaimIsExclusiveAndUncertainIntentCanBeReclaimed(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	paneIDs := []string{"w1:p1", "w2:p1"}
	workspaceIDs := []string{"w1", "w2"}

	first, acquired, created, err := store.ClaimBindIntent(ctx, "repo-a", "owner-one", time.Minute, paneIDs, workspaceIDs)
	if err != nil || !acquired || !created {
		t.Fatalf("first ClaimBindIntent = (%+v, %v, %v, %v), want new owned intent", first, acquired, created, err)
	}
	if first.Phase != BindIntentPrepared || first.Owner != "owner-one" {
		t.Fatalf("first intent = %+v, want prepared and owned by owner-one", first)
	}

	if err := store.MarkBindIntentCreating(ctx, "repo-a", "owner-one", time.Minute); err != nil {
		t.Fatalf("MarkBindIntentCreating: %v", err)
	}
	second, acquired, created, err := store.ClaimBindIntent(ctx, "repo-a", "owner-two", time.Minute, nil, nil)
	if err != nil || acquired || created {
		t.Fatalf("concurrent ClaimBindIntent = (%+v, %v, %v, %v), want no second owner", second, acquired, created, err)
	}

	if err := store.MarkBindIntentUncertain(ctx, "repo-a", "owner-one"); err != nil {
		t.Fatalf("MarkBindIntentUncertain: %v", err)
	}
	recovered, acquired, created, err := store.ClaimBindIntent(ctx, "repo-a", "owner-two", time.Minute, nil, nil)
	if err != nil || !acquired || created {
		t.Fatalf("retry ClaimBindIntent = (%+v, %v, %v, %v), want to reclaim existing intent", recovered, acquired, created, err)
	}
	if recovered.Phase != BindIntentUncertain || recovered.Owner != "owner-two" {
		t.Fatalf("reclaimed intent = %+v, want uncertain and owned by owner-two", recovered)
	}
	if !reflect.DeepEqual(recovered.PaneIDs, paneIDs) || !reflect.DeepEqual(recovered.WorkspaceIDs, workspaceIDs) {
		t.Fatalf("reclaimed baseline = panes %v workspaces %v, want panes %v workspaces %v",
			recovered.PaneIDs, recovered.WorkspaceIDs, paneIDs, workspaceIDs)
	}
}
