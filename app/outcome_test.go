package app

import (
	"context"
	"testing"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// Recording an outcome (a mail send, a model's answer) onto a record that was
// deleted before the answer landed is a no-op, not an error: the answering
// listener must advance past it, not wedge the log retrying a gone record.
func TestRecordOutcomeSkipsDeletedRecord(t *testing.T) {
	a, _ := newTestApp(t)
	write := recordOutcome(a.Reg)
	ctx := context.Background()

	// A member exists; write an outcome onto a non-existent id of that class.
	ann := a.Reg.MustClass("Member").New(map[string]any{"email": "a@x.org", "pw": "long enough pw", "name": "Ann"})
	if err := a.Reg.MustClass("Member").Save(ctx, ann); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, "Member", ann.ID()+99999, map[string]any{"name": "ignored"}); err != nil {
		t.Errorf("writing onto a missing record should be a no-op, got %v", err)
	}

	// Deleting a record, then recording onto it, is also a no-op.
	if err := a.Reg.MustClass("Member").Delete(ctx, ann.ID()); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, "Member", ann.ID(), map[string]any{"name": "late answer"}); err != nil {
		t.Errorf("writing onto a deleted record should be a no-op, got %v", err)
	}
	// A real record still records.
	ben := a.Reg.MustClass("Member").New(map[string]any{"email": "b@x.org", "pw": "long enough pw", "name": "Ben"})
	if err := a.Reg.MustClass("Member").Save(ctx, ben); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, "Member", ben.ID(), map[string]any{"tz": "UTC"}); err != nil {
		t.Fatalf("recording onto a live record: %v", err)
	}
	got, _ := a.Reg.MustClass("Member").Load(ctx, ben.ID())
	if got.String("tz") != "UTC" {
		t.Errorf("the outcome was not written: tz=%q", got.String("tz"))
	}
	_ = runtime.ErrNotFound
}
