package recording

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestParseLifecycle(t *testing.T) {
	got, err := ParseLifecycle("nearline=30, coldline=90,archive=180,delete=365")
	if err != nil || got != (Lifecycle{Nearline: 30, Coldline: 90, Archive: 180, Delete: 365}) {
		t.Fatalf("%+v %v", got, err)
	}
	if empty, err := ParseLifecycle(""); err != nil || empty != (Lifecycle{}) {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	for _, bad := range []string{"nearline", "nearline=-1", "glacier=3", "delete=x"} {
		if _, err := ParseLifecycle(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

type biller struct {
	charges map[string]StorageCharge
	fail    error
}

func (b *biller) BillStorage(_ context.Context, charges []StorageCharge) error {
	if b.fail != nil {
		return b.fail
	}
	for _, c := range charges {
		if _, seen := b.charges[c.ID]; !seen {
			b.charges[c.ID] = c
		}
	}
	return nil
}

func (b *biller) byteSeconds(class string) int64 {
	var total int64
	for _, c := range b.charges {
		if c.Class == class {
			total += c.Bytes * c.Seconds
		}
	}
	return total
}

// Dev's bucket: Nearline after a day, deleted after a week.
func TestStorageIsBilledByDayAndClassUntilTheLifecycleDeletesIt(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	written := time.Date(2027, 1, 10, 18, 0, 0, 0, time.UTC)
	now := written
	_, _ = store.Create(ctx, Recording{ID: "rec", Tenant: "acme", Room: "standup", State: StateCompleted,
		CreatedAt: written, EndedAt: written, Artifacts: []Artifact{
			{Name: "a", Bytes: 600, Attempt: 1, StoredAt: written},
			{Name: "b", Bytes: 400, Attempt: 1, StoredAt: written},
		}})
	b := &biller{charges: map[string]StorageCharge{}}
	job := &StorageBilling{Store: store, Biller: b, Lifecycle: Lifecycle{Nearline: 1, Delete: 7}, Now: func() time.Time { return now }}

	// Before the first midnight nothing is closed.
	if n, err := job.Run(ctx); err != nil || n != 0 || len(b.charges) != 0 {
		t.Fatalf("billed %d (%v) before a day closed: %v", n, err, b.charges)
	}

	// Two days on: the six hours to the first midnight and the whole of the
	// next day, which turned Nearline at 18:00.
	now = time.Date(2027, 1, 12, 3, 0, 0, 0, time.UTC)
	if n, err := job.Run(ctx); err != nil || n != 1 {
		t.Fatalf("billed %d (%v)", n, err)
	}
	want := map[string]int64{
		"rec:1:2027-01-10:standard": 6 * 3600,
		"rec:1:2027-01-11:standard": 18 * 3600,
		"rec:1:2027-01-11:nearline": 6 * 3600,
	}
	if len(b.charges) != len(want) {
		t.Fatalf("charges %v, want %v", b.charges, want)
	}
	for id, seconds := range want {
		if c := b.charges[id]; c.Seconds != seconds || c.Bytes != 1000 || c.Tenant != "acme" || c.Room != "standup" {
			t.Errorf("%s: %+v, want %d seconds of 1000 bytes", id, c, seconds)
		}
	}
	// A second pass the same day bills nothing more.
	if n, _ := job.Run(ctx); n != 0 {
		t.Fatalf("a second pass billed %d recordings", n)
	}

	// A month later: billed to the deletion seven days after writing, and done.
	now = time.Date(2027, 2, 12, 0, 0, 0, 0, time.UTC)
	if n, err := job.Run(ctx); err != nil || n != 1 {
		t.Fatalf("billed %d (%v)", n, err)
	}
	if got := b.byteSeconds(ClassStandard); got != 1000*24*3600 {
		t.Errorf("standard byte-seconds %d, want the first day", got)
	}
	if got := b.byteSeconds(ClassNearline); got != 1000*6*24*3600 {
		t.Errorf("nearline byte-seconds %d, want the six days after it", got)
	}
	r, _ := store.Get(ctx, "acme", "rec")
	if !r.StorageDone {
		t.Error("storage deleted by the lifecycle is still owed")
	}
	if due, _ := store.StorageDue(ctx, now.Add(30*24*time.Hour), 10); len(due) != 0 {
		t.Errorf("done storage is still due: %v", due)
	}
}

func TestALaterAttemptIsBilledFromWhenItWasWritten(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	first := time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	_, _ = store.Create(ctx, Recording{ID: "rec", Tenant: "acme", State: StateRecording, CreatedAt: first,
		Artifacts: []Artifact{{Name: "a", Bytes: 10, Attempt: 1, StoredAt: first}}})
	b := &biller{charges: map[string]StorageCharge{}}
	now := first.Add(36 * time.Hour)
	job := &StorageBilling{Store: store, Biller: b, Now: func() time.Time { return now }}
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	second := now
	_, _ = store.Update(ctx, "rec", func(r *Recording) error {
		r.Artifacts = append(r.Artifacts, Artifact{Name: "b", Bytes: 5, Attempt: 2, StoredAt: second})
		return nil
	})
	now = first.Add(72 * time.Hour)
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Kept for ever in Standard: three whole days of the first, twelve hours
	// and a day of the second.
	if got, want := b.byteSeconds(ClassStandard), int64(10*3*24*3600+5*36*3600); got != want {
		t.Fatalf("byte-seconds %d, want %d: %v", got, want, b.charges)
	}
	if r, _ := store.Get(ctx, "acme", "rec"); r.StorageDone {
		t.Error("storage with no deletion rule was marked done")
	}
}

func TestAFailedChargeLeavesTheWatermark(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	at := time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	_, _ = store.Create(ctx, Recording{ID: "rec", Tenant: "acme", State: StateCompleted, CreatedAt: at,
		Artifacts: []Artifact{{Name: "a", Bytes: 10, Attempt: 1, StoredAt: at}}})
	b := &biller{charges: map[string]StorageCharge{}, fail: errors.New("billing is down")}
	job := &StorageBilling{Store: store, Biller: b, Now: func() time.Time { return at.Add(48 * time.Hour) }}
	if _, err := job.Run(ctx); err == nil {
		t.Fatal("a failed charge was not reported")
	}
	if r, _ := store.Get(ctx, "acme", "rec"); !r.StorageBilledThrough.IsZero() {
		t.Fatalf("watermark moved to %s on a failed charge", r.StorageBilledThrough)
	}
	b.fail = nil
	if n, err := job.Run(ctx); err != nil || n != 1 || len(b.charges) != 2 {
		t.Fatalf("the retry billed %d (%v): %s", n, err, fmt.Sprint(b.charges))
	}
}
