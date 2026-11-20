package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A settle that fails (a rolling upgrade: the other nodes refuse the new view
// index's rows) is tried again until it succeeds, not left until the next start.
func TestSettleUntilDoneRetries(t *testing.T) {
	tries := 0
	try := func() error {
		tries++
		if tries < 3 {
			return errors.New("cluster: only 1 of 3 live nodes took the change")
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		settleUntilDone(context.Background(), try, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, 4*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("settleUntilDone did not finish")
	}
	if tries != 3 {
		t.Fatalf("tried %d times, want 3", tries)
	}
}

// It stops when the app does.
func TestSettleUntilDoneStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		settleUntilDone(ctx, func() error { return errors.New("no") }, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("settleUntilDone kept going after the app stopped")
	}
}
