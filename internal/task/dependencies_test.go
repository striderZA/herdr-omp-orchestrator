package task

import (
	"slices"
	"testing"
)

func TestDependencyCycleFindsNoCycleInAChain(t *testing.T) {
	tasks := []Task{
		{ID: "a"},
		{ID: "b", Dependencies: []string{"a"}},
		{ID: "c", Dependencies: []string{"b", "a"}},
	}
	if cycle := DependencyCycle(tasks, nil); cycle != nil {
		t.Fatalf("an acyclic graph reported cycle %v", cycle)
	}
}

func TestDependencyCycleFindsASelfLoop(t *testing.T) {
	tasks := []Task{{ID: "a", Dependencies: []string{"a"}}}
	if cycle := DependencyCycle(tasks, nil); !slices.Equal(cycle, []string{"a"}) {
		t.Fatalf("a task that waits on itself = %v, want [a]", cycle)
	}
}

func TestDependencyCycleSeesACycleThatOnlyAnOverrideCreates(t *testing.T) {
	// Recorded: b waits on a. The override re-points a to wait on b, which closes
	// a -> b -> a. Nothing recorded is cyclic, so only the overrides can refuse it.
	tasks := []Task{
		{ID: "a", Dependencies: []string{"superseded"}},
		{ID: "b", Dependencies: []string{"a"}},
	}
	if cycle := DependencyCycle(tasks, nil); cycle != nil {
		t.Fatalf("the recorded graph is cyclic: %v", cycle)
	}
	cycle := DependencyCycle(tasks, map[string][]string{"a": {"b"}})
	if len(cycle) != 2 || !slices.Contains(cycle, "a") || !slices.Contains(cycle, "b") {
		t.Fatalf("the override cycle = %v, want the a and b loop", cycle)
	}
}

func TestDependencyCycleIgnoresDependenciesOutsideTheList(t *testing.T) {
	// A dependency on an id this list does not hold is refused elsewhere; here it
	// is simply not an edge of this graph.
	tasks := []Task{{ID: "a", Dependencies: []string{"gone"}}, {ID: "b", Dependencies: []string{"a"}}}
	if cycle := DependencyCycle(tasks, map[string][]string{"gone": {"a"}}); cycle != nil {
		t.Fatalf("an unknown dependency made a cycle: %v", cycle)
	}
}

func TestReplaceDependencyReplacesInPlaceAndKeepsOneEntry(t *testing.T) {
	for _, c := range []struct {
		name       string
		deps, want []string
	}{
		{"in place", []string{"s", "x"}, []string{"t", "x"}},
		{"already waits on the successor", []string{"s", "t"}, []string{"t"}},
		{"no entry for the superseded task", []string{"x"}, []string{"x"}},
	} {
		if got := ReplaceDependency(c.deps, "s", "t"); !slices.Equal(got, c.want) {
			t.Errorf("%s: ReplaceDependency(%v) = %v, want %v", c.name, c.deps, got, c.want)
		}
	}
}
