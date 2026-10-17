package herdr

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCallObserverReceivesSuccessfulAndFailedCalls(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newFixture(t).stdout(`{"id":"1","result":{"type":"ack"}}`)
		var gotArgv []string
		var gotElapsed time.Duration
		var gotErr error
		f.client.SetCallObserver(func(argv []string, elapsed time.Duration, err error) {
			gotArgv = append([]string(nil), argv...)
			gotElapsed = elapsed
			gotErr = err
		})

		if _, err := f.client.Call(context.Background(), "pane", "get", "w1:p1"); err != nil {
			t.Fatalf("Call: %v", err)
		}
		if !reflect.DeepEqual(gotArgv, []string{"pane", "get", "w1:p1"}) {
			t.Fatalf("observer argv = %q", gotArgv)
		}
		if gotElapsed < 0 {
			t.Fatalf("observer elapsed = %s, want non-negative", gotElapsed)
		}
		if gotErr != nil {
			t.Fatalf("observer error = %v, want nil", gotErr)
		}
	})

	t.Run("failed", func(t *testing.T) {
		f := newFixture(t).hang()
		var gotErr error
		f.client.SetCallObserver(func(_ []string, _ time.Duration, err error) { gotErr = err })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err := f.client.Call(ctx, "agent", "get", "w1:p1")
		if err == nil {
			t.Fatal("Call succeeded, want timeout")
		}
		if gotErr == nil || !errors.Is(gotErr, err) {
			t.Fatalf("observer error = %v, want call error %v", gotErr, err)
		}
	})
}
