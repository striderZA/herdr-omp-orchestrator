package task

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
)

func fingerprintTask() Task {
	return Task{
		ID:            "orch_aaaaaaaaaaaa",
		ProjectKey:    "E:/repo",
		Title:         "Plan: parser",
		Brief:         "Investigate and report a plan.",
		Checks:        []string{"go test ./... ok", "go vet ./... ok"},
		Dependencies:  []string{"orch_d1", "orch_d2"},
		ReservedPaths: []string{"src/a.go", "src/b.go"},
		BaseRevision:  "0123456789abcdef0123456789abcdef01234567",
		TicketID:      "tkt_abc123",
	}
}

func TestFingerprintIsEqualForTheSameWork(t *testing.T) {
	a := fingerprintTask()
	b := fingerprintTask()
	// Identity and progress are not part of "the same work": a repeated dispatch
	// gets a new id and branch, and the first task may have moved on.
	b.ID = "orch_bbbbbbbbbbbb"
	b.Branch = "orch/bbbbbbbbbbbb"
	b.Phase = PromptSubmitted
	b.Blocker = "brief submission pending: x y"
	b.Attention = "something"
	b.Report = &Report{Kind: "progress", Summary: "half way"}
	b.Revision, b.CreatedAt, b.UpdatedAt = 9, 1, 2
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("identity and progress fields must not change the fingerprint")
	}
}

func TestFingerprintChangesWithEveryDispatchParameter(t *testing.T) {
	base := fingerprintTask().Fingerprint()
	for name, mutate := range map[string]func(*Task){
		"title":         func(x *Task) { x.Title = "Plan: lexer" },
		"brief":         func(x *Task) { x.Brief = "A different brief." },
		"checks":        func(x *Task) { x.Checks = []string{"go test ./... ok"} },
		"checks order":  func(x *Task) { x.Checks = []string{"go vet ./... ok", "go test ./... ok"} },
		"dependencies":  func(x *Task) { x.Dependencies = []string{"orch_d1"} },
		"reserved":      func(x *Task) { x.ReservedPaths = []string{"src/a.go"} },
		"base revision": func(x *Task) { x.BaseRevision = "fedcba9876543210fedcba9876543210fedcba98" },
		"ticket":        func(x *Task) { x.TicketID = "tkt_other" },
		"no ticket":     func(x *Task) { x.TicketID = "" },
	} {
		changed := fingerprintTask()
		mutate(&changed)
		if changed.Fingerprint() == base {
			t.Errorf("changing the %s must change the fingerprint", name)
		}
	}
}

func TestFingerprintTreatsDependenciesAndReservedPathsAsSets(t *testing.T) {
	// The request normalizer drops duplicates and keeps the first order, so the
	// order of these two lists carries no meaning; the order of the checks does.
	reordered := fingerprintTask()
	reordered.Dependencies = []string{"orch_d2", "orch_d1"}
	reordered.ReservedPaths = []string{"src/b.go", "src/a.go"}
	if reordered.Fingerprint() != fingerprintTask().Fingerprint() {
		t.Fatal("the order of dependencies and reserved paths must not change the fingerprint")
	}
}

func TestFingerprintDoesNotReorderTheTaskItIsComputedFrom(t *testing.T) {
	x := fingerprintTask()
	x.Dependencies = []string{"orch_d2", "orch_d1"}
	x.ReservedPaths = []string{"src/b.go", "src/a.go"}
	_ = x.Fingerprint()
	if x.Dependencies[0] != "orch_d2" || x.ReservedPaths[0] != "src/b.go" {
		t.Fatalf("Fingerprint sorted the task's own lists: %v %v", x.Dependencies, x.ReservedPaths)
	}
}

func TestFingerprintReadsNilAndEmptyListsAsTheSame(t *testing.T) {
	a := fingerprintTask()
	a.Dependencies, a.ReservedPaths = nil, nil
	b := fingerprintTask()
	b.Dependencies, b.ReservedPaths = []string{}, []string{}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("a missing list and an empty list are the same request")
	}
}

func TestFingerprintIsTheHashOfAVersionedJSONArray(t *testing.T) {
	sum := sha256.Sum256([]byte(
		`["dispatch/v1","Plan: parser","Investigate and report a plan.",` +
			`["go test ./... ok","go vet ./... ok"],["orch_d1","orch_d2"],["src/a.go","src/b.go"],` +
			`"0123456789abcdef0123456789abcdef01234567","tkt_abc123"]`))
	want := hex.EncodeToString(sum[:])
	if got := fingerprintTask().Fingerprint(); got != want {
		t.Fatalf("Fingerprint = %s, want the SHA-256 of the canonical JSON array %s", got, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fingerprintTask().Fingerprint()) {
		t.Fatal("the fingerprint is not lowercase hex SHA-256")
	}
}

func TestFingerprintHasNoFieldBoundaryCollisions(t *testing.T) {
	// Raw concatenation would make both of these "ab".
	a := Task{Title: "a", Brief: "b"}
	b := Task{Title: "ab", Brief: ""}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("a title and brief that concatenate to the same text must not collide")
	}
	// A value that merely looks like the next field.
	c := Task{Title: `x","y`, Brief: "z"}
	d := Task{Title: "x", Brief: `y","z`}
	if c.Fingerprint() == d.Fingerprint() {
		t.Fatal("a value containing the delimiter must not collide with a split value")
	}
}

func workTask(id, title string) Task {
	x := fingerprintTask()
	x.ID, x.Title = id, title
	return x
}

func TestOpenHolderOfFindsTheOpenTaskHoldingTheSameWork(t *testing.T) {
	candidate := workTask("new", "Plan: parser")
	existing := []Task{workTask("a", "Plan: lexer"), workTask("b", "Plan: parser")}
	holder, ok := OpenHolderOf(existing, candidate)
	if !ok || holder.ID != "b" {
		t.Fatalf("OpenHolderOf = (%q, %v), want task b", holder.ID, ok)
	}
	if _, ok := OpenHolderOf(existing[:1], candidate); ok {
		t.Fatal("work no open task holds must report no holder")
	}
	if _, ok := OpenHolderOf(nil, candidate); ok {
		t.Fatal("no tasks at all must report no holder")
	}
}

func TestOpenHolderOfIgnoresADecidedTask(t *testing.T) {
	// Every outcome closes a task (ADR 0001), so even a rejected one is finished
	// work: a user who asks for it again is making a new request.
	for _, outcome := range Outcomes() {
		decided := workTask("a", "Plan: parser")
		decided.Outcome, decided.Disposition = outcome, outcome
		if _, ok := OpenHolderOf([]Task{decided}, workTask("new", "Plan: parser")); ok {
			t.Errorf("a task decided as %q still holds its work", outcome)
		}
	}
	// A legacy row has a disposition and no outcome; it is closed too.
	legacy := workTask("a", "Plan: parser")
	legacy.Disposition = "accepted"
	if _, ok := OpenHolderOf([]Task{legacy}, workTask("new", "Plan: parser")); ok {
		t.Error("a legacy closed row still holds its work")
	}
}

func TestOpenHolderOfNamesTheOldestRegardlessOfOrder(t *testing.T) {
	older, newer := workTask("older", "Plan: parser"), workTask("newer", "Plan: parser")
	older.CreatedAt, newer.CreatedAt = 100, 200
	for name, existing := range map[string][]Task{
		"oldest first": {older, newer},
		"newest first": {newer, older},
	} {
		if holder, ok := OpenHolderOf(existing, workTask("new", "Plan: parser")); !ok || holder.ID != "older" {
			t.Errorf("%s: holder = %q, want the oldest open task", name, holder.ID)
		}
	}
	// Equal timestamps fall back to the id, so the answer is still one value.
	a, b := workTask("a", "Plan: parser"), workTask("b", "Plan: parser")
	if holder, _ := OpenHolderOf([]Task{b, a}, workTask("new", "Plan: parser")); holder.ID != "a" {
		t.Errorf("tie holder = %q, want a", holder.ID)
	}
}
