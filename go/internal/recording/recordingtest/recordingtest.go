// Package recordingtest is the behaviour every recording.Store must have.
package recordingtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/recording"
)

var epoch = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// Run runs the suite against a fresh store from each call to open.
func Run(t *testing.T, open func(t *testing.T) recording.Store) {
	t.Run("OneActiveRecordingPerRoom", func(t *testing.T) { oneActivePerRoom(t, open(t)) })
	t.Run("ClaimsHonourTheLease", func(t *testing.T) { claimsHonourTheLease(t, open(t)) })
	t.Run("ListingPagesNewestFirst", func(t *testing.T) { listingPagesNewestFirst(t, open(t)) })
	t.Run("ReadsAreTenantScoped", func(t *testing.T) { readsAreTenantScoped(t, open(t)) })
}

func create(t *testing.T, store recording.Store, id, room string, at time.Time) recording.Recording {
	t.Helper()
	created, err := store.Create(context.Background(), recording.Recording{
		ID: id, Tenant: "acme", Room: room, State: recording.StatePending, CreatedAt: at,
		Artifacts: []recording.Artifact{{Name: "seed", Bytes: 1}},
	})
	if err != nil {
		t.Fatalf("creating %s: %v", id, err)
	}
	return created
}

func oneActivePerRoom(t *testing.T, store recording.Store) {
	ctx := context.Background()
	create(t, store, "r1", "room", epoch)
	_, err := store.Create(ctx, recording.Recording{ID: "r2", Tenant: "acme", Room: "room", State: recording.StatePending, CreatedAt: epoch})
	if !errors.Is(err, recording.ErrActive) {
		t.Fatalf("a second active recording gave %v, want ErrActive", err)
	}
	if _, err := store.Update(ctx, "r1", func(r *recording.Recording) error {
		r.State = recording.StateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	create(t, store, "r2", "room", epoch.Add(time.Second))
	active, err := store.Active(ctx, "acme", "room")
	if err != nil || len(active) != 1 || active[0].ID != "r2" {
		t.Errorf("active recordings are %v (%v), want r2 alone", active, err)
	}

	sentinel := errors.New("refused")
	if _, err := store.Update(ctx, "r2", func(r *recording.Recording) error {
		r.Error = "should not be kept"
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Errorf("a refused update gave %v", err)
	}
	if got, _ := store.Get(ctx, "acme", "r2"); got.Error != "" {
		t.Error("a refused update was written")
	}
}

func claimsHonourTheLease(t *testing.T, store recording.Store) {
	ctx := context.Background()
	create(t, store, "old", "a", epoch)
	create(t, store, "new", "b", epoch.Add(time.Minute))

	first, ok, err := store.Claim(ctx, "w1", epoch, time.Minute)
	if err != nil || !ok || first.ID != "old" {
		t.Fatalf("first claim gave %q %v %v, want the oldest", first.ID, ok, err)
	}
	if first.State != recording.StateRecording || first.Attempt != 1 || !first.StartedAt.Equal(epoch) {
		t.Errorf("a claimed recording is %s attempt %d started %v", first.State, first.Attempt, first.StartedAt)
	}
	second, _, _ := store.Claim(ctx, "w2", epoch, time.Minute)
	if second.ID != "new" {
		t.Fatalf("second claim gave %q, want the other recording", second.ID)
	}
	if _, ok, _ := store.Claim(ctx, "w3", epoch.Add(30*time.Second), time.Minute); ok {
		t.Fatal("a recording under lease was claimed")
	}

	// Progress from the first attempt is kept when the lease lapses.
	if _, err := store.Update(ctx, "old", func(r *recording.Recording) error {
		r.Current = recording.Progress{Objects: 5, BytesStored: 50}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	again, ok, _ := store.Claim(ctx, "w3", epoch.Add(2*time.Minute), time.Minute)
	if !ok || again.ID != "old" || again.Attempt != 2 || again.Worker != "w3" {
		t.Fatalf("after the lease lapsed the claim gave %q attempt %d to %q", again.ID, again.Attempt, again.Worker)
	}
	if again.Stored.BytesStored != 50 || again.Current != (recording.Progress{}) || !again.StartedAt.Equal(epoch) {
		t.Errorf("a reclaimed recording kept %+v stored, %+v current, started %v", again.Stored, again.Current, again.StartedAt)
	}
	if len(again.Artifacts) != 1 {
		t.Errorf("a reclaimed recording has %d artifacts, want the one it was created with", len(again.Artifacts))
	}

	if _, err := store.Update(ctx, "old", func(r *recording.Recording) error {
		r.State = recording.StateCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := store.Claim(ctx, "w4", epoch.Add(time.Hour), time.Minute); ok && got.ID == "old" {
		t.Error("a completed recording was claimed")
	}
}

func listingPagesNewestFirst(t *testing.T, store recording.Store) {
	ctx := context.Background()
	for i := range 5 {
		create(t, store, fmt.Sprintf("r%d", i), fmt.Sprintf("room-%d", i), epoch.Add(time.Duration(i)*time.Second))
	}
	var seen []string
	token := ""
	for range 10 {
		page, err := store.List(ctx, recording.Query{Tenant: "acme", PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Recordings {
			seen = append(seen, r.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if got := strings.Join(seen, ","); got != "r4,r3,r2,r1,r0" {
		t.Errorf("listed %s", got)
	}
	one, err := store.List(ctx, recording.Query{Tenant: "acme", Room: "room-2"})
	if err != nil || len(one.Recordings) != 1 || one.Recordings[0].ID != "r2" {
		t.Errorf("listing one room gave %v (%v)", one.Recordings, err)
	}
	if _, err := store.List(ctx, recording.Query{Tenant: "acme", PageToken: "!!"}); err == nil {
		t.Error("a malformed page token was accepted")
	}
}

func readsAreTenantScoped(t *testing.T, store recording.Store) {
	ctx := context.Background()
	create(t, store, "r1", "room", epoch)
	if _, err := store.Get(ctx, "globex", "r1"); err == nil {
		t.Error("another tenant read our recording")
	}
	page, err := store.List(ctx, recording.Query{Tenant: "globex"})
	if err != nil || len(page.Recordings) != 0 {
		t.Errorf("another tenant's listing shows %d of ours (%v)", len(page.Recordings), err)
	}
	active, err := store.Active(ctx, "globex", "room")
	if err != nil || len(active) != 0 {
		t.Errorf("another tenant's same-named room shows %d of ours (%v)", len(active), err)
	}
}
