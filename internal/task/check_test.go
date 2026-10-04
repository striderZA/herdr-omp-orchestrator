package task

import (
	"encoding/json"
	"testing"
)

// A report stored before checks carried outcomes holds each check as a bare
// string. It has to decode as it always did, and if the task is rewritten for an
// unrelated reason its checks have to be encoded as the same JSON strings.
func TestLegacyStoredReportKeepsItsChecksVerbatim(t *testing.T) {
	const stored = `{"Kind":"result","Summary":"s","Branch":"orch/x","Evidence":["commit abc"],"Checks":["go test ./... ok","gofmt -l . (no output)"],"Risk":"none"}`

	var report Report
	if err := json.Unmarshal([]byte(stored), &report); err != nil {
		t.Fatalf("a report stored with free-text checks no longer decodes: %v", err)
	}
	if len(report.Checks) != 2 {
		t.Fatalf("decoded %d checks, want 2: %+v", len(report.Checks), report.Checks)
	}
	for i, want := range []string{"go test ./... ok", "gofmt -l . (no output)"} {
		got := report.Checks[i]
		if got.Name != want || got.Outcome != "" || got.Detail != "" {
			t.Errorf("legacy check %d = %+v, want only the name %q", i, got, want)
		}
		if got.HasOutcome() {
			t.Errorf("legacy check %d claims an outcome nobody recorded", i)
		}
		if got.Label() != want {
			t.Errorf("legacy check %d renders as %q, want it unchanged: %q", i, got.Label(), want)
		}
	}

	again, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode the decoded legacy report: %v", err)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again, &after); err != nil {
		t.Fatal(err)
	}
	if string(after["Checks"]) != string(before["Checks"]) {
		t.Errorf("rewriting a legacy report changed its checks:\n got  %s\n want %s", after["Checks"], before["Checks"])
	}
}

func TestCheckWithAnOutcomeRoundTripsAsAnObject(t *testing.T) {
	in := []Check{
		{Name: "go test ./...", Outcome: CheckPassed},
		{Name: "golangci-lint run", Outcome: CheckSkipped, Detail: "not installed on this machine"},
		{Name: "go vet ./...", Outcome: CheckFailed, Detail: "2 findings"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `[{"name":"go test ./...","outcome":"passed"},` +
		`{"name":"golangci-lint run","outcome":"skipped","detail":"not installed on this machine"},` +
		`{"name":"go vet ./...","outcome":"failed","detail":"2 findings"}]`
	if string(data) != want {
		t.Fatalf("encoded checks:\n got  %s\n want %s", data, want)
	}
	var out []Check
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("decoded %d checks, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("check %d = %+v, want %+v", i, out[i], in[i])
		}
		if !out[i].HasOutcome() {
			t.Errorf("check %d lost its outcome", i)
		}
	}
}

// The decoder is the wire reader too: a bare string is a check that states no
// outcome (the validator refuses it as an incomplete report), and a JSON type
// that is neither a string nor an object is a malformed body.
func TestCheckDecoderReadsWireShapes(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		want    Check
		wantErr bool
	}{
		{"bare string", `"go test ./..."`, Check{Name: "go test ./..."}, false},
		{"object", `{"name":"go test ./...","outcome":"passed","detail":"ok"}`, Check{Name: "go test ./...", Outcome: CheckPassed, Detail: "ok"}, false},
		{"object without an outcome", `{"name":"go test ./..."}`, Check{Name: "go test ./..."}, false},
		{"empty object", `{}`, Check{}, false},
		{"null", `null`, Check{}, false},
		{"number", `7`, Check{}, true},
		{"boolean", `true`, Check{}, true},
		{"array", `["go test"]`, Check{}, true},
		{"object with a non-string name", `{"name":7,"outcome":"passed"}`, Check{}, true},
		{"object with a non-string outcome", `{"name":"x","outcome":true}`, Check{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Check
			err := json.Unmarshal([]byte(tc.json), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decoded %s into %+v, want an error", tc.json, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode %s: %v", tc.json, err)
			}
			if got != tc.want {
				t.Errorf("decoded %s into %+v, want %+v", tc.json, got, tc.want)
			}
		})
	}
}

func TestCheckOutcomeVocabulary(t *testing.T) {
	for _, outcome := range []string{"passed", "failed", "skipped"} {
		if !ValidCheckOutcome(outcome) {
			t.Errorf("%q is not a valid check outcome", outcome)
		}
	}
	// The vocabulary is closed and exact: no synonyms, no case folding, no padding.
	for _, outcome := range []string{"", "ok", "pass", "PASSED", "Passed", " passed", "passed ", "unknown", "error"} {
		if ValidCheckOutcome(outcome) {
			t.Errorf("%q must not be a valid check outcome", outcome)
		}
	}
	if got := CheckOutcomes(); len(got) != 3 || got[0] != CheckPassed || got[1] != CheckFailed || got[2] != CheckSkipped {
		t.Errorf("CheckOutcomes() = %v, want [passed failed skipped]", got)
	}
}

func TestCheckLabel(t *testing.T) {
	cases := []struct {
		check Check
		want  string
	}{
		{Check{Name: "go test ./... ok"}, "go test ./... ok"},
		{Check{Name: "go test ./...", Outcome: CheckPassed}, "go test ./...: passed"},
		{Check{Name: "go vet ./...", Outcome: CheckFailed, Detail: "2 findings"}, "go vet ./...: failed (2 findings)"},
		{Check{Name: "lint", Outcome: CheckSkipped, Detail: "not installed"}, "lint: skipped (not installed)"},
	}
	for _, tc := range cases {
		if got := tc.check.Label(); got != tc.want {
			t.Errorf("Label(%+v) = %q, want %q", tc.check, got, tc.want)
		}
	}
}

// Null clears the receiver, as it does for any other value encoding/json decodes
// into. A reused Check must not keep fields from an earlier decode.
func TestCheckDecoderNullResetsAReusedReceiver(t *testing.T) {
	check := Check{Name: "go test ./...", Outcome: CheckPassed, Detail: "ok"}
	if err := json.Unmarshal([]byte(`null`), &check); err != nil {
		t.Fatalf("decode null: %v", err)
	}
	if check != (Check{}) {
		t.Errorf("null left %+v in a reused Check, want the zero value", check)
	}

	// The same holds inside a list that reuses its elements.
	checks := []Check{{Name: "stale", Outcome: CheckFailed, Detail: "stale detail"}}
	if err := json.Unmarshal([]byte(`[null]`), &checks); err != nil {
		t.Fatalf("decode [null]: %v", err)
	}
	if len(checks) != 1 || checks[0] != (Check{}) {
		t.Errorf("[null] decoded into %+v, want one zero Check", checks)
	}
}

// Whatever a Check holds must come back from its own encoding unchanged, so a task
// that is read and written again never changes a report: a name that needs
// escaping, a legacy check (name only), a detail without an outcome, and the zero
// value.
func TestCheckRoundTripsThroughItsOwnEncoding(t *testing.T) {
	for _, in := range []Check{
		{},
		{Name: "go test ./..."},
		{Name: `quote " backslash \ tab ` + "\t" + ` angle <b> amp & unicode é 日本`},
		{Name: "x", Detail: "detail without an outcome"},
		{Name: "x", Outcome: CheckPassed},
		{Name: "x", Outcome: CheckFailed, Detail: `<details> & "quoted"`},
	} {
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("encode %+v: %v", in, err)
		}
		var out Check
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		if out != in {
			t.Errorf("%+v came back from %s as %+v", in, data, out)
		}
		again, err := json.Marshal(out)
		if err != nil || string(again) != string(data) {
			t.Errorf("re-encoding %+v gave %s (err %v), want the same %s", out, again, err, data)
		}
	}
}
