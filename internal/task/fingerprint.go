package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// fingerprintVersion names the shape hashed below. Changing what counts as the
// same work means a new version, so a fingerprint is never compared with one
// built from a different shape.
const fingerprintVersion = "dispatch/v1"

// Fingerprint identifies the work a dispatch asked for, so a repeated request
// for it can be recognised (issue #4). It covers exactly the dispatch
// parameters (title, brief, checks, dependencies, reserved paths, base
// revision) and the ticket the work is for, and nothing about the task's own
// identity or progress: a repeat gets a new id and branch, and the first task
// may have moved on by the time it arrives.
//
// Dependencies and reserved paths are sets (the request normalizer drops
// duplicates and their order means nothing), so they are sorted here; the
// checks are a list the worker reads in order, so they are not. Sorting works on
// copies: the task's own lists are never reordered.
//
// The value is the SHA-256 of one canonical JSON array, like Ticket.Digest, so
// no two different requests can produce the same hashed text by moving text
// across a field boundary.
func (t Task) Fingerprint() string {
	// Marshalling strings and lists of strings cannot fail.
	encoded, _ := json.Marshal([]any{
		fingerprintVersion,
		t.Title,
		t.Brief,
		listOf(t.Checks, false),
		listOf(t.Dependencies, true),
		listOf(t.ReservedPaths, true),
		t.BaseRevision,
		t.TicketID,
	})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// listOf returns a copy of items that is never nil, so a missing list and an
// empty one encode the same, optionally sorted.
func listOf(items []string, sorted bool) []string {
	out := append([]string{}, items...)
	if sorted {
		sort.Strings(out)
	}
	return out
}

// OpenHolderOf returns the open task of existing that holds the same work as
// the candidate (issue #4). A task is open until it records a disposition
// (ADR 0001), so a queued task counts and a decided one never does, whatever
// the outcome: a user who asks again for decided work is making a new request.
// Callers pass one project's tasks; the project is never compared here.
//
// Rows recorded before the duplicate gate existed can hold the same work more
// than once; the oldest is returned (ties broken by id) so the answer does not
// depend on row order.
func OpenHolderOf(existing []Task, candidate Task) (Task, bool) {
	want := candidate.Fingerprint()
	var holder Task
	found := false
	for _, other := range existing {
		if other.Disposition != "" || other.Fingerprint() != want {
			continue
		}
		if !found || other.CreatedAt < holder.CreatedAt || (other.CreatedAt == holder.CreatedAt && other.ID < holder.ID) {
			holder, found = other, true
		}
	}
	return holder, found
}
