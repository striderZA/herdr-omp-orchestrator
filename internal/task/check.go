package task

import (
	"bytes"
	"encoding/json"
	"errors"
)

// The outcomes a worker states for each check in a final report. The vocabulary
// is closed: other code relies on the value, so it is compared exactly.
const (
	CheckPassed  = "passed"
	CheckFailed  = "failed"
	CheckSkipped = "skipped"
)

// CheckOutcomes lists every outcome a check can state, in display order.
func CheckOutcomes() []string {
	return []string{CheckPassed, CheckFailed, CheckSkipped}
}

// ValidCheckOutcome reports whether outcome is one a check can state. It is an
// exact comparison: callers trim what a human typed before they ask.
func ValidCheckOutcome(outcome string) bool {
	for _, o := range CheckOutcomes() {
		if o == outcome {
			return true
		}
	}
	return false
}

// Check is one check a worker ran and what came of it.
//
// A report recorded before outcomes existed holds each check as a bare string.
// Such a check decodes to a Check that has only a Name, renders as that text and
// encodes back to a JSON string, so a stored report is read and shown as it was
// recorded and stays a list of strings if its task is rewritten. Nobody recorded
// how those checks ended, so no outcome is invented for them. A new report is
// refused unless every check states an outcome (coord.validateReport), so a Check
// without one is either a legacy row or a request that has not been validated yet.
//
// The JSON spelling is lowercase because the OMP extension owns this shape: the
// same type is the wire body of orch_report and the persisted fact.
type Check struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// HasOutcome reports whether the check states how it ended. A legacy check does
// not.
func (c Check) HasOutcome() bool {
	return c.Outcome != ""
}

// Label is the one-line text a person reads: the name alone for a check that
// states nothing else (a legacy check, shown exactly as recorded), otherwise the
// name with its outcome and, when there is one, its detail.
func (c Check) Label() string {
	label := c.Name
	if c.Outcome != "" {
		label += ": " + c.Outcome
	}
	if c.Detail != "" {
		label += " (" + c.Detail + ")"
	}
	return label
}

// CheckLabels is the Label of each check, in order.
func CheckLabels(checks []Check) []string {
	labels := make([]string, 0, len(checks))
	for _, check := range checks {
		labels = append(labels, check.Label())
	}
	return labels
}

// MarshalJSON writes a check that states nothing but its name as the bare string
// it was stored as before outcomes existed, and any other check as an object.
func (c Check) MarshalJSON() ([]byte, error) {
	if c.Outcome == "" && c.Detail == "" {
		return json.Marshal(c.Name)
	}
	type plain Check // drops the methods, so this does not recurse
	return json.Marshal(plain(c))
}

// UnmarshalJSON reads either spelling of a check: the bare string of a legacy
// report (and of a request that left the outcome out) or the object. Any other
// JSON type is not a check. Unknown keys in the object are ignored, so a row
// written by a later version stays readable.
func (c *Check) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.Equal(trimmed, []byte("null")):
		// As encoding/json does for any other value, null resets the receiver, so
		// a reused Check cannot keep fields from an earlier decode.
		*c = Check{}
		return nil
	case len(trimmed) > 0 && trimmed[0] == '"':
		var name string
		if err := json.Unmarshal(trimmed, &name); err != nil {
			return err
		}
		*c = Check{Name: name}
		return nil
	case len(trimmed) > 0 && trimmed[0] == '{':
		type plain Check // drops the methods, so this does not recurse
		var decoded plain
		if err := json.Unmarshal(trimmed, &decoded); err != nil {
			return err
		}
		*c = Check(decoded)
		return nil
	}
	return errors.New("task: a check is a string or an object")
}
