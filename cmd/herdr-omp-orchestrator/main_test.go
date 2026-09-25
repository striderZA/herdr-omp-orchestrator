package main

// Task 6: the hook routes are the only surface Herdr and the OMP extension call
// directly, so their refusals are exercised without a coordinator, a state
// directory or a Herdr binary: every one of these paths returns before anything
// is opened.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
)

// eventAnswer is one JSON envelope an event route wrote.
type eventAnswer struct {
	OK    bool `json:"ok"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func runEventRoute(t *testing.T, env map[string]string) (int, eventAnswer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := runEvent(env, &stdout, &stderr)
	var answer eventAnswer
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
			t.Fatalf("decode the answer %q: %v", stdout.String(), err)
		}
	}
	return status, answer
}

// TestEventRouteChecksThePayloadBoundBeforeTrimming pins that the bound applies
// to the bytes Herdr delivered: a payload made mostly of legal JSON whitespace
// with a tiny valid object inside is refused whole, while a whitespace-only
// payload is still refused as missing and one exactly at the bound passes the
// gate.
func TestEventRouteChecksThePayloadBoundBeforeTrimming(t *testing.T) {
	inner := `{"type":"pane_agent_status_changed","pane_id":"w1:p1","workspace_id":"w1","agent_status":"idle"}`

	t.Run("an oversized payload is refused whole", func(t *testing.T) {
		status, answer := runEventRoute(t, map[string]string{
			envPluginEvent:     coord.EventAgentStatus,
			envPluginEventJSON: inner + strings.Repeat(" ", coord.MaxEventPayloadBytes),
		})
		if status == 0 {
			t.Fatalf("status = 0, want nonzero")
		}
		if answer.Error == nil || answer.Error.Code != "event_payload_too_large" {
			t.Fatalf("answer = %+v, want event_payload_too_large", answer.Error)
		}
		if !strings.Contains(answer.Error.Message, "larger than") {
			t.Fatalf("message = %q, want the size refusal", answer.Error.Message)
		}
	})

	t.Run("a whitespace-only payload is still missing", func(t *testing.T) {
		status, answer := runEventRoute(t, map[string]string{
			envPluginEvent:     coord.EventAgentStatus,
			envPluginEventJSON: strings.Repeat(" \t\r\n", 16),
		})
		if status == 0 {
			t.Fatalf("status = 0, want nonzero")
		}
		if answer.Error == nil || answer.Error.Code != "event_payload_unavailable" {
			t.Fatalf("answer = %+v, want event_payload_unavailable", answer.Error)
		}
	})

	t.Run("a payload at the bound passes the size gate", func(t *testing.T) {
		atBound := inner + strings.Repeat(" ", coord.MaxEventPayloadBytes-len(inner))
		status, answer := runEventRoute(t, map[string]string{
			envPluginEvent:     coord.EventAgentStatus,
			envPluginEventJSON: atBound,
		})
		if status == 0 {
			t.Fatalf("status = 0, want the missing state directory to fail the route")
		}
		if answer.Error == nil || answer.Error.Code != "bad_request" {
			t.Fatalf("answer = %+v, want the state-dir refusal (the size gate must not fire)", answer.Error)
		}
		if !strings.Contains(answer.Error.Message, envStateDir) {
			t.Fatalf("message = %q, want it to name the missing state directory", answer.Error.Message)
		}
	})

	t.Run("a missing event name is refused first", func(t *testing.T) {
		status, answer := runEventRoute(t, map[string]string{envPluginEventJSON: inner})
		if status == 0 {
			t.Fatalf("status = 0, want nonzero")
		}
		if answer.Error == nil || answer.Error.Code != "event_unavailable" {
			t.Fatalf("answer = %+v, want event_unavailable", answer.Error)
		}
	})
}
