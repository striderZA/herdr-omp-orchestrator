package task

// DependencyCycle reports one cycle in the dependency graph of tasks, or nil when
// the graph is acyclic. A task's edges are its Dependencies, unless overrides
// names it: an override is the dependency list the graph would have after a
// re-point, so a cycle that a re-point would create is found before anything is
// written. An edge to an id the list does not hold is not part of the graph.
//
// The cycle is returned as ids where each task waits on the next, and the last
// waits on the first. A task that waits on itself is a cycle of one.
func DependencyCycle(tasks []Task, overrides map[string][]string) []string {
	present := make(map[string]bool, len(tasks))
	for _, x := range tasks {
		present[x.ID] = true
	}
	edges := make(map[string][]string, len(tasks))
	for _, x := range tasks {
		deps := x.Dependencies
		if next, ok := overrides[x.ID]; ok {
			deps = next
		}
		for _, dep := range deps {
			if present[dep] {
				edges[x.ID] = append(edges[x.ID], dep)
			}
		}
	}

	const (
		unvisited = iota
		onPath
		done
	)
	state := make(map[string]int, len(tasks))
	var path []string
	var visit func(id string) []string
	visit = func(id string) []string {
		switch state[id] {
		case done:
			return nil
		case onPath:
			// id is already on the current path, so the path from it onward closes a loop.
			for i, step := range path {
				if step == id {
					return append([]string(nil), path[i:]...)
				}
			}
			return []string{id}
		}
		state[id] = onPath
		path = append(path, id)
		for _, dep := range edges[id] {
			if cycle := visit(dep); cycle != nil {
				return cycle
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return nil
	}
	for _, x := range tasks {
		if state[x.ID] != unvisited {
			continue
		}
		if cycle := visit(x.ID); cycle != nil {
			return cycle
		}
	}
	return nil
}

// ReplaceDependency returns deps with every entry for from replaced by to, in
// place, and with duplicates removed: a dependent that already waits on to keeps
// one entry for it. It is the one rule a re-point applies to a dependency list,
// shared by the store that writes it and the coordinator that previews it.
func ReplaceDependency(deps []string, from, to string) []string {
	out := make([]string, 0, len(deps))
	seen := make(map[string]bool, len(deps))
	for _, dep := range deps {
		if dep == from {
			dep = to
		}
		if seen[dep] {
			continue
		}
		seen[dep] = true
		out = append(out, dep)
	}
	return out
}
