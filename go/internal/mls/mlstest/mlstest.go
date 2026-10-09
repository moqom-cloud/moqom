// Package mlstest is the contract every mls.Store must meet, run against the
// memory store in `go test` and against Postgres where one is available.
package mlstest

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/mls"
)

// Run runs the contract. open returns a fresh, empty store per case.
func Run(t *testing.T, open func(t *testing.T) mls.Store) {
	ctx := context.Background()
	room := mls.Room{Tenant: "acme", Room: "room-42"}
	other := mls.Room{Tenant: "acme", Room: "elsewhere"}
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	t.Run("key packages are used once and the last resort forever", func(t *testing.T) {
		store := open(t)
		if err := store.PutKeyPackages(ctx, room, "bob/b", [][]byte{{1}, {2}}, []byte{9}); err != nil {
			t.Fatal(err)
		}
		for _, want := range [][]byte{{1}, {2}, {9}, {9}} {
			got, err := store.ClaimKeyPackage(ctx, room, "bob/b")
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("claimed %v, %v; want %v", got, err, want)
			}
		}
		if _, err := store.ClaimKeyPackage(ctx, other, "bob/b"); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("another room's packages: %v", err)
		}
		// Replacing discards what was there.
		if err := store.PutKeyPackages(ctx, room, "bob/b", [][]byte{{3}}, []byte{8}); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.ClaimKeyPackage(ctx, room, "bob/b"); !bytes.Equal(got, []byte{3}) {
			t.Fatalf("after replace, claimed %v", got)
		}
	})

	t.Run("a one-time package is never handed to two claimants", func(t *testing.T) {
		store := open(t)
		var packages [][]byte
		for i := range 20 {
			packages = append(packages, []byte{byte(i + 1)})
		}
		if err := store.PutKeyPackages(ctx, room, "bob/b", packages, []byte{0xff}); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		seen := map[byte]int{}
		var group sync.WaitGroup
		for range 20 {
			group.Add(1)
			go func() {
				defer group.Done()
				got, err := store.ClaimKeyPackage(ctx, room, "bob/b")
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				seen[got[0]]++
				mu.Unlock()
			}()
		}
		group.Wait()
		for value, count := range seen {
			if value != 0xff && count > 1 {
				t.Fatalf("package %d handed out %d times", value, count)
			}
		}
	})

	t.Run("one group per room, and commits are ordered by epoch", func(t *testing.T) {
		store := open(t)
		if _, err := store.Group(ctx, room); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("empty room has a group: %v", err)
		}
		if _, err := store.CreateGroup(ctx, room, []byte("g1"), mls.SuiteMLDSA, "alice/a", at); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateGroup(ctx, room, []byte("g2"), mls.SuiteMLDSA, "bob/b", at); !errors.Is(err, mls.ErrConflict) {
			t.Fatalf("second founder: %v", err)
		}

		seq, err := store.AppendCommit(ctx, room, mls.Commit{
			GroupID: []byte("g1"), Epoch: 0, Sender: "alice/a", Data: []byte("c0"),
			Welcome: []byte("w0"), Joiners: []string{"bob/b"},
		}, at)
		if err != nil || seq != 1 {
			t.Fatalf("first commit: %d, %v", seq, err)
		}
		// Two members racing for epoch 1: the first wins and the second is stale.
		if _, err := store.AppendCommit(ctx, room, mls.Commit{
			GroupID: []byte("g1"), Epoch: 1, Sender: "bob/b", Data: []byte("c1"),
		}, at); err != nil {
			t.Fatal(err)
		}
		for _, stale := range []mls.Commit{
			{GroupID: []byte("g1"), Epoch: 1, Sender: "alice/a", Data: []byte("late")},
			{GroupID: []byte("g1"), Epoch: 0, Sender: "alice/a", Data: []byte("old")},
			{GroupID: []byte("g9"), Epoch: 2, Sender: "alice/a", Data: []byte("wrong group")},
		} {
			if _, err := store.AppendCommit(ctx, room, stale, at); !errors.Is(err, mls.ErrStale) {
				t.Fatalf("%s accepted: %v", stale.Data, err)
			}
		}
		group, err := store.Group(ctx, room)
		if err != nil || group.Epoch != 2 {
			t.Fatalf("epoch %d, %v", group.Epoch, err)
		}

		presence, _ := store.Presence(ctx, room)
		members := map[string]bool{}
		for _, entry := range presence {
			members[entry.Device] = entry.Member
		}
		if !members["alice/a"] || !members["bob/b"] {
			t.Fatalf("membership after the add: %v", members)
		}
	})

	t.Run("a group keeps the cipher suite it was founded in", func(t *testing.T) {
		store := open(t)
		// A store holds whatever suite it is given: rows from before the
		// AES-128 suites were removed say 0x0001, and the service drops them.
		legacy := mls.CipherSuite(0x0001)
		if _, err := store.CreateGroup(ctx, room, []byte("g"), legacy, "alice/a", at); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendCommit(ctx, room, mls.Commit{GroupID: []byte("g"), Sender: "alice/a", Data: []byte("c")}, at); err != nil {
			t.Fatal(err)
		}
		group, err := store.Group(ctx, room)
		if err != nil || group.CipherSuite != legacy || group.Epoch != 1 {
			t.Fatalf("after a commit: %+v, %v", group, err)
		}
		// Refounded after a drop: the new group's suite, not the old one's.
		if err := store.DropGroup(ctx, room, []byte("g")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateGroup(ctx, room, []byte("h"), mls.SuiteMLDSA, "bob/b", at); err != nil {
			t.Fatal(err)
		}
		group, err = store.Group(ctx, room)
		if err != nil || group.CipherSuite != mls.SuiteMLDSA {
			t.Fatalf("refounded: %+v, %v", group, err)
		}
	})

	t.Run("a Welcome is only for the devices it names", func(t *testing.T) {
		store := open(t)
		_, _ = store.CreateGroup(ctx, room, []byte("g"), mls.SuiteMLDSA, "alice/a", at)
		_, err := store.AppendCommit(ctx, room, mls.Commit{
			GroupID: []byte("g"), Epoch: 0, Sender: "alice/a", Data: []byte("c"),
			Welcome: []byte("w"), Joiners: []string{"bob/b"},
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		bob, cursor, err := store.Messages(ctx, room, 0, "bob/b", 100)
		if err != nil || len(bob) != 2 || bob[1].Kind != mls.KindWelcome || cursor != 2 {
			t.Fatalf("bob sees %+v, cursor %d, %v", bob, cursor, err)
		}
		carol, cursor, _ := store.Messages(ctx, room, 0, "carol/c", 100)
		if len(carol) != 1 || carol[0].Kind != mls.KindCommit || cursor != 2 {
			t.Fatalf("carol sees %+v, cursor %d", carol, cursor)
		}
		after, cursor, _ := store.Messages(ctx, room, 1, "bob/b", 100)
		if len(after) != 1 || after[0].Seq != 2 || cursor != 2 {
			t.Fatalf("after 1: %+v", after)
		}
		limited, cursor, _ := store.Messages(ctx, room, 0, "bob/b", 1)
		if len(limited) != 1 || cursor != 1 {
			t.Fatalf("limited to one: %+v cursor %d", limited, cursor)
		}
	})

	t.Run("a removed and re-added device ends up a member", func(t *testing.T) {
		store := open(t)
		_, _ = store.CreateGroup(ctx, room, []byte("g"), mls.SuiteMLDSA, "alice/a", at)
		_ = store.Touch(ctx, room, "bob/b", at)
		_, err := store.AppendCommit(ctx, room, mls.Commit{
			GroupID: []byte("g"), Epoch: 0, Sender: "alice/a", Data: []byte("c"),
			Welcome: []byte("w"), Joiners: []string{"bob/b"}, Removed: []string{"bob/b"},
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		presence, _ := store.Presence(ctx, room)
		for _, entry := range presence {
			if entry.Device == "bob/b" && !entry.Member {
				t.Fatal("bob was left out")
			}
		}
	})

	t.Run("dropping a group keeps sequence numbers rising and clears membership", func(t *testing.T) {
		store := open(t)
		_, _ = store.CreateGroup(ctx, room, []byte("g"), mls.SuiteMLDSA, "alice/a", at)
		_, _ = store.AppendCommit(ctx, room, mls.Commit{GroupID: []byte("g"), Sender: "alice/a", Data: []byte("c")}, at)
		if err := store.DropGroup(ctx, room, []byte("not this one")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Group(ctx, room); err != nil {
			t.Fatal("a drop for another group id dropped this one")
		}
		if err := store.DropGroup(ctx, room, []byte("g")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Group(ctx, room); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("group survived: %v", err)
		}
		presence, _ := store.Presence(ctx, room)
		for _, entry := range presence {
			if entry.Member {
				t.Fatalf("%s still a member", entry.Device)
			}
		}
		_, _ = store.CreateGroup(ctx, room, []byte("h"), mls.SuiteMLDSA, "bob/b", at)
		seq, err := store.AppendCommit(ctx, room, mls.Commit{GroupID: []byte("h"), Sender: "bob/b", Data: []byte("d")}, at)
		if err != nil || seq <= 1 {
			t.Fatalf("sequence after a drop: %d, %v", seq, err)
		}
		messages, _, _ := store.Messages(ctx, room, 0, "bob/b", 100)
		if len(messages) != 1 || !bytes.Equal(messages[0].Data, []byte("d")) {
			t.Fatalf("the old group's log survived: %+v", messages)
		}
	})

	t.Run("old messages are pruned and the cursor still rises", func(t *testing.T) {
		store := open(t)
		_, _ = store.CreateGroup(ctx, room, []byte("g"), mls.SuiteMLDSA, "alice/a", at)
		_, _ = store.AppendCommit(ctx, room, mls.Commit{GroupID: []byte("g"), Sender: "alice/a", Data: []byte("old")}, at)
		later := at.Add(mls.MessageRetention + time.Second)
		seq, err := store.AppendCommit(ctx, room, mls.Commit{
			GroupID: []byte("g"), Epoch: 1, Sender: "alice/a", Data: []byte("new"),
		}, later)
		if err != nil {
			t.Fatal(err)
		}
		messages, cursor, _ := store.Messages(ctx, room, 0, "alice/a", 100)
		if len(messages) != 1 || !bytes.Equal(messages[0].Data, []byte("new")) || cursor != seq {
			t.Fatalf("after pruning: %+v, cursor %d", messages, cursor)
		}
	})

	t.Run("presence: touch, membership, leave", func(t *testing.T) {
		store := open(t)
		_ = store.PutKeyPackages(ctx, room, "bob/b", nil, []byte{1})
		_ = store.Touch(ctx, room, "bob/b", at)
		_ = store.SetMember(ctx, room, "bob/b", true)
		presence, _ := store.Presence(ctx, room)
		if len(presence) != 1 || !presence[0].Member || !presence[0].HasKeyPackages ||
			!presence[0].LastSeen.Equal(at) {
			t.Fatalf("presence %+v", presence)
		}
		_ = store.SetMember(ctx, room, "bob/b", false)
		_ = store.Leave(ctx, room, "bob/b")
		presence, _ = store.Presence(ctx, room)
		if len(presence) != 0 {
			t.Fatalf("left, still present: %+v", presence)
		}
		if _, err := store.ClaimKeyPackage(ctx, room, "bob/b"); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("a departed device's packages: %v", err)
		}
	})

	t.Run("audience: keys, envelopes, counts", func(t *testing.T) {
		store := open(t)
		first := mls.EpochRef{GroupID: []byte("group-one"), Epoch: 3}
		second := mls.EpochRef{GroupID: []byte("group-one"), Epoch: 4}
		key := bytes.Repeat([]byte{1}, mls.AudienceKeyBytes)
		_ = store.PutAudienceKey(ctx, room, "viewer/a", key, at)
		_ = store.PutAudienceKey(ctx, room, "viewer/b", key, at.Add(-time.Hour))
		if _, err := store.Envelope(ctx, room, "viewer/a"); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("an envelope before any was sealed: %v", err)
		}
		present, waiting, _ := store.AudienceCount(ctx, room, first, at.Add(-time.Minute))
		if present != 1 || waiting != 1 {
			t.Fatalf("present %d waiting %d, want 1 and 1: the stale viewer is not counted", present, waiting)
		}

		err := store.PutEnvelopes(ctx, room, []mls.Envelope{
			{Device: "viewer/a", EpochRef: first, Data: []byte("sealed")},
			{Device: "stranger/x", EpochRef: first, Data: []byte("dropped")},
		})
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := store.Envelope(ctx, room, "viewer/a")
		if err != nil || !envelope.EpochRef.Equal(first) || string(envelope.Data) != "sealed" {
			t.Fatalf("envelope %+v, %v", envelope, err)
		}
		if _, err := store.Envelope(ctx, room, "stranger/x"); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("an envelope for a device that is not a viewer was kept: %v", err)
		}
		if _, waiting, _ := store.AudienceCount(ctx, room, first, at.Add(-time.Minute)); waiting != 0 {
			t.Fatalf("waiting %d once sealed", waiting)
		}
		if _, waiting, _ := store.AudienceCount(ctx, room, second, at.Add(-time.Minute)); waiting != 1 {
			t.Fatalf("waiting %d once the epoch moved on", waiting)
		}
		seats, _ := store.Audience(ctx, room)
		if len(seats) != 2 || seats[0].Device != "viewer/a" || seats[0].Sealed == nil || !seats[0].Sealed.Equal(first) ||
			!bytes.Equal(seats[0].Key, key) || seats[1].Sealed != nil {
			t.Fatalf("seats %+v", seats)
		}

		// The same key again keeps its envelope; a new one cannot open it.
		_ = store.PutAudienceKey(ctx, room, "viewer/a", key, at.Add(time.Second))
		if _, err := store.Envelope(ctx, room, "viewer/a"); err != nil {
			t.Fatalf("a renewed key lost its envelope: %v", err)
		}
		_ = store.PutAudienceKey(ctx, room, "viewer/a", bytes.Repeat([]byte{2}, mls.AudienceKeyBytes), at.Add(2*time.Second))
		if _, err := store.Envelope(ctx, room, "viewer/a"); !errors.Is(err, mls.ErrNotFound) {
			t.Fatalf("a replaced key kept an envelope it cannot open: %v", err)
		}

		_ = store.LeaveAudience(ctx, room, "viewer/a")
		if seats, _ := store.Audience(ctx, room); len(seats) != 1 {
			t.Fatalf("left, still seated: %+v", seats)
		}
		if seats, _ := store.Audience(ctx, other); len(seats) != 0 {
			t.Fatalf("another room's audience: %+v", seats)
		}
	})
}
