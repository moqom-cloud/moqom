// Package storetest is the contract every Store implementation must satisfy.
//
// It lives here, in its own package, rather than as tests beside each
// implementation, because the interesting risk is not that one of them is broken:
// it is that they differ. Services are developed against the in-memory store and
// deployed against Postgres, so any behaviour the services rely on has to be
// asserted once and run twice. A test written next to an implementation cannot do
// that, and the difference it misses will be found in production.
//
// What is asserted here is deliberately the awkward half of the interface --
// idempotency under retry, contiguous event sequences, paging that neither
// repeats nor skips, unions across scope. The easy half fails loudly anyway.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// Factory makes an empty store, and a function to release it.
//
// The clock is supplied by the caller of Run so that time-dependent assertions
// do not depend on how fast the machine is.
type Factory func(t *testing.T, now func() time.Time) (store.Store, func())

// Run executes the whole contract.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	tests := []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"rooms are unique per tenant", testRoomUniqueness},
		{"a missing room is not found rather than empty", testMissingRoom},
		{"room paging neither repeats nor skips", testRoomPaging},
		{"participants filter backstage and departed", testParticipantFilters},
		{"a retried moderation action applies once", testIdempotency},
		{"idempotency is scoped to a tenant", testIdempotencyScope},
		{"event sequences are contiguous per room", testEventSequences},
		{"events resume from a cursor", testEventResumption},
		{"bans in force include the tenant-wide ones", testBanUnion},
		{"a ban on one subject replaces rather than accumulates", testBanReplacement},
		{"an expired ban is not in force", testBanExpiry},
		{"the two forced mutes coexist and lift separately", testMuteScopes},
		{"grant overrides survive a read", testGrantOverrides},
		{"the watchdog limit is enforced", testWatchdogLimit},
		{"the moderation log reads newest first and filters", testLogQuery},
		{"enforcement can be marked after the fact", testMarkEnforced},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.run(t, newStore) })
	}
}

// A fixed clock by default. Tests that need time to pass advance it themselves.
func fixedClock() (func() time.Time, func(time.Duration)) {
	current := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return current }, func(d time.Duration) { current = current.Add(d) }
}

func newRoom(tenant, id string) control.Room {
	return control.Room{
		Tenant:    tenant,
		Room:      id,
		Lifecycle: control.LifecycleLive,
		Policy:    control.Policy{}.WithDefaults(),
	}
}

func setup(t *testing.T, newStore Factory) (context.Context, store.Store, func(time.Duration)) {
	t.Helper()
	clock, advance := fixedClock()
	subject, release := newStore(t, clock)
	t.Cleanup(release)
	return context.Background(), subject, advance
}

func testRoomUniqueness(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	if _, err := subject.CreateRoom(ctx, newRoom("acme", "room-1")); err != nil {
		t.Fatalf("creating a room failed: %v", err)
	}
	_, err := subject.CreateRoom(ctx, newRoom("acme", "room-1"))
	if !errors.Is(err, control.ErrAlreadyExists) {
		t.Errorf("recreating a room gave %v, want ErrAlreadyExists", err)
	}
	// The same room ID under a different tenant is a different room. Tenants
	// name their rooms and two customers will both have a "lobby".
	if _, err := subject.CreateRoom(ctx, newRoom("other", "room-1")); err != nil {
		t.Errorf("another tenant could not use the same room name: %v", err)
	}
}

func testMissingRoom(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	_, err := subject.GetRoom(ctx, "acme", "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// Paging over a table being written to must not skip or repeat. An offset does
// both, and for a moderation log that means an auditor who cannot tell.
func testRoomPaging(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	const total = 25
	for i := range total {
		if _, err := subject.CreateRoom(ctx, newRoom("acme", fmt.Sprintf("room-%02d", i))); err != nil {
			t.Fatalf("creating room %d: %v", i, err)
		}
	}

	seen := map[string]int{}
	token := ""
	pages := 0
	for {
		page, err := subject.ListRooms(ctx, store.RoomQuery{
			Tenant: "acme",
			Paging: store.Paging{PageSize: 7, PageToken: token},
		})
		if err != nil {
			t.Fatalf("listing rooms: %v", err)
		}
		for _, room := range page.Rooms {
			seen[room.Room]++
		}
		pages++
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct rooms across %d pages, want %d", len(seen), pages, total)
	}
	for room, count := range seen {
		if count != 1 {
			t.Errorf("%s appeared %d times", room, count)
		}
	}
}

func testParticipantFilters(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)
	if _, err := subject.CreateRoom(ctx, newRoom("acme", "room-1")); err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	onstage := control.Participant{Identity: control.Identity{Username: "alice", Device: "d1"}, Presence: control.PresenceConnected}
	backstage := control.Participant{Identity: control.Identity{Username: "bob", Device: "d1"}, Presence: control.PresenceConnected, Backstage: true}
	departed := control.Participant{Identity: control.Identity{Username: "carol", Device: "d1"}, Presence: control.PresenceLeft}

	for _, participant := range []control.Participant{onstage, backstage, departed} {
		if err := subject.SaveParticipant(ctx, "acme", "room-1", participant); err != nil {
			t.Fatalf("saving %s: %v", participant.Identity, err)
		}
	}

	page, err := subject.ListParticipants(ctx, store.ParticipantQuery{Tenant: "acme", Room: "room-1"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Participants) != 1 || page.Participants[0].Identity.Username != "alice" {
		t.Errorf("default listing returned %d participants, want only alice", len(page.Participants))
	}

	page, err = subject.ListParticipants(ctx, store.ParticipantQuery{
		Tenant: "acme", Room: "room-1", IncludeBackstage: true, IncludeDeparted: true,
	})
	if err != nil {
		t.Fatalf("listing everything: %v", err)
	}
	if len(page.Participants) != 3 {
		t.Errorf("inclusive listing returned %d, want 3", len(page.Participants))
	}

	// A participant who left keeps their record, so "who was here when this
	// happened" still has an answer after they have gone.
	if _, err := subject.GetParticipant(ctx, "acme", "room-1", departed.Identity); err != nil {
		t.Errorf("a departed participant's record was gone: %v", err)
	}
}

// The action most likely to be retried is the one whose second application does
// the most damage. A queue redelivering "ban for 24 hours" must not produce 48.
func testIdempotency(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	entry := control.ModerationEntry{
		Tenant:         "acme",
		Room:           "room-1",
		Verb:           control.VerbBan,
		Target:         control.Identity{Username: "alice"},
		IdempotencyKey: "queue-delivery-1",
	}

	first, err := subject.AppendModerationEntry(ctx, entry)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	second, err := subject.AppendModerationEntry(ctx, entry)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("a retry produced a second entry: %s then %s", first.ID, second.ID)
	}

	page, err := subject.ListModerationLog(ctx, store.LogQuery{Tenant: "acme"})
	if err != nil {
		t.Fatalf("listing the log: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Errorf("the log holds %d entries after one retried action, want 1", len(page.Entries))
	}

	found, err := subject.FindModerationEntry(ctx, "acme", "queue-delivery-1")
	if err != nil {
		t.Fatalf("finding by key: %v", err)
	}
	if found.ID != first.ID {
		t.Errorf("lookup by key returned %s, want %s", found.ID, first.ID)
	}
}

// Keys are supplied by callers, so two tenants will eventually pick the same
// one. If the scope were global, one customer's retry key would suppress
// another customer's action.
func testIdempotencyScope(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	shared := "action-1"
	first, err := subject.AppendModerationEntry(ctx, control.ModerationEntry{
		Tenant: "acme", Room: "r", Verb: control.VerbKick, IdempotencyKey: shared,
	})
	if err != nil {
		t.Fatalf("first tenant: %v", err)
	}
	second, err := subject.AppendModerationEntry(ctx, control.ModerationEntry{
		Tenant: "other", Room: "r", Verb: control.VerbKick, IdempotencyKey: shared,
	})
	if err != nil {
		t.Fatalf("second tenant: %v", err)
	}
	if first.ID == second.ID {
		t.Error("one tenant's idempotency key suppressed another tenant's action")
	}
}

// Contiguity is a promise to consumers: a gap means events were lost, which is
// worth alerting on. That only works if the store never leaves a gap itself, and
// never restarts numbering.
func testEventSequences(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	for i := range 5 {
		event, err := subject.AppendEvent(ctx, store.Event{
			Tenant: "acme", Room: "room-1", Kind: store.EventPresenceChanged,
			Body: []byte{byte(i)},
		})
		if err != nil {
			t.Fatalf("appending event %d: %v", i, err)
		}
		if event.Sequence != uint64(i+1) {
			t.Errorf("event %d got sequence %d, want %d", i, event.Sequence, i+1)
		}
	}

	// Sequences are per room, so a busy room does not advance a quiet one's
	// numbering and a consumer of one is unaffected by the other.
	other, err := subject.AppendEvent(ctx, store.Event{Tenant: "acme", Room: "room-2", Kind: store.EventPresenceChanged})
	if err != nil {
		t.Fatalf("appending to a second room: %v", err)
	}
	if other.Sequence != 1 {
		t.Errorf("a second room started at %d, want 1", other.Sequence)
	}
}

func testEventResumption(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	for i := range 5 {
		if _, err := subject.AppendEvent(ctx, store.Event{
			Tenant: "acme", Room: "room-1",
			Kind: store.EventPresenceChanged, Body: []byte{byte(i)},
		}); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}

	events, err := subject.ListEvents(ctx, store.EventQuery{Tenant: "acme", Room: "room-1", AfterSequence: 2})
	if err != nil {
		t.Fatalf("listing after a cursor: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("resuming after 2 returned %d events, want 3", len(events))
	}
	if events[0].Sequence != 3 {
		t.Errorf("first resumed event is %d, want 3", events[0].Sequence)
	}

	filtered, err := subject.ListEvents(ctx, store.EventQuery{
		Tenant: "acme", Room: "room-1", Kinds: []store.EventKind{store.EventModerationApplied},
	})
	if err != nil {
		t.Fatalf("filtering by kind: %v", err)
	}
	if len(filtered) != 0 {
		t.Errorf("filtering by an absent kind returned %d events", len(filtered))
	}
}

// A relay is handed the bans that apply, not the rule for working them out. If
// the union were the relay's job it would be a second implementation of it, in
// another language, on the enforcement path.
func testBanUnion(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	if _, err := subject.PutBan(ctx, control.Ban{
		Tenant: "acme", Room: "room-1", Kind: control.BanByUsername, Username: "alice",
	}); err != nil {
		t.Fatalf("room ban: %v", err)
	}
	if _, err := subject.PutBan(ctx, control.Ban{
		Tenant: "acme", Kind: control.BanByUsername, Username: "bob",
	}); err != nil {
		t.Fatalf("tenant ban: %v", err)
	}
	if _, err := subject.PutBan(ctx, control.Ban{
		Tenant: "acme", Room: "room-2", Kind: control.BanByUsername, Username: "carol",
	}); err != nil {
		t.Fatalf("another room's ban: %v", err)
	}

	bans, err := subject.ListBans(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}

	names := map[string]bool{}
	for _, ban := range bans {
		names[ban.Username] = true
	}
	if !names["alice"] {
		t.Error("the room's own ban was missing")
	}
	if !names["bob"] {
		t.Error("a tenant-wide ban did not apply to the room")
	}
	if names["carol"] {
		t.Error("another room's ban leaked into this one")
	}
}

func testBanReplacement(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	subjectBan := control.Ban{Tenant: "acme", Room: "room-1", Kind: control.BanByUsername, Username: "alice", Reason: "first"}
	if _, err := subject.PutBan(ctx, subjectBan); err != nil {
		t.Fatalf("first ban: %v", err)
	}
	subjectBan.Reason = "second"
	if _, err := subject.PutBan(ctx, subjectBan); err != nil {
		t.Fatalf("second ban: %v", err)
	}

	bans, err := subject.ListBans(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(bans) != 1 {
		t.Fatalf("banning the same subject twice left %d bans, want 1", len(bans))
	}
	if bans[0].Reason != "second" {
		t.Errorf("the later ban did not win: reason is %q", bans[0].Reason)
	}

	// Lifting has to actually lift. With two rows for one subject, an unban
	// leaves one in force and reads to a moderator as a broken button.
	if err := subject.DeleteBan(ctx, "acme", "room-1", control.BanByUsername, subjectBan); err != nil {
		t.Fatalf("unbanning: %v", err)
	}
	bans, err = subject.ListBans(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing after unban: %v", err)
	}
	if len(bans) != 0 {
		t.Errorf("%d bans remain after unbanning", len(bans))
	}
}

func testBanExpiry(t *testing.T, newStore Factory) {
	ctx, subject, advance := setup(t, newStore)

	if _, err := subject.PutBan(ctx, control.Ban{
		Tenant: "acme", Room: "room-1", Kind: control.BanByUsername, Username: "alice",
		ExpiresAt: time.Date(2026, 7, 27, 12, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("timed ban: %v", err)
	}

	bans, err := subject.ListBans(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(bans) != 1 {
		t.Fatalf("a live timed ban was not in force")
	}

	advance(time.Hour)
	bans, err = subject.ListBans(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing after expiry: %v", err)
	}
	if len(bans) != 0 {
		t.Errorf("an expired ban is still in force: %+v", bans)
	}
}

// One participant can be silenced for everybody and hidden from the audience at
// the same time. Merging the two would make lifting one lift both.
func testMuteScopes(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)
	alice := control.Identity{Username: "alice", Device: "d1"}

	if _, err := subject.PutMute(ctx, store.Mute{
		Tenant: "acme", Room: "room-1", Identity: alice, Media: control.MediaAudio,
	}); err != nil {
		t.Fatalf("everyone mute: %v", err)
	}
	if _, err := subject.PutMute(ctx, store.Mute{
		Tenant: "acme", Room: "room-1", Identity: alice, Media: control.MediaScreenShare, AudienceOnly: true,
	}); err != nil {
		t.Fatalf("audience mute: %v", err)
	}

	mutes, err := subject.ListMutes(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(mutes) != 2 {
		t.Fatalf("one participant under both mutes produced %d rows, want 2", len(mutes))
	}

	if err := subject.DeleteMute(ctx, "acme", "room-1", alice, true); err != nil {
		t.Fatalf("lifting the audience mute: %v", err)
	}
	mutes, err = subject.ListMutes(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing after lift: %v", err)
	}
	if len(mutes) != 1 {
		t.Fatalf("lifting one mute left %d, want 1", len(mutes))
	}
	if mutes[0].AudienceOnly {
		t.Error("the wrong mute was lifted")
	}
}

func testGrantOverrides(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)
	alice := control.Identity{Username: "alice", Device: "d1"}

	demoted := control.Grants{Publish: control.PublishNone, Moderation: 0}
	if err := subject.PutGrantOverride(ctx, "acme", "room-1", alice, demoted); err != nil {
		t.Fatalf("writing an override: %v", err)
	}

	overrides, err := subject.ListGrantOverrides(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("reading overrides: %v", err)
	}
	got, ok := overrides[alice]
	if !ok {
		t.Fatal("the override was not returned")
	}
	if got.Publish != control.PublishNone || got.Moderation != 0 {
		t.Errorf("override came back as %+v", got)
	}

	// Overwriting rather than accumulating: the override is the whole answer, so
	// a promotion after a demotion is one row, not two.
	promoted := control.Grants{Publish: control.PublishCamera, Moderation: control.CapChatGroup}
	if err := subject.PutGrantOverride(ctx, "acme", "room-1", alice, promoted); err != nil {
		t.Fatalf("overwriting: %v", err)
	}
	overrides, err = subject.ListGrantOverrides(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("reading again: %v", err)
	}
	if overrides[alice].Moderation != control.CapChatGroup {
		t.Errorf("the later override did not win: %+v", overrides[alice])
	}
}

func testWatchdogLimit(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	room := newRoom("acme", "room-1")
	room.Policy.WatchdogLimit = 2
	if _, err := subject.CreateRoom(ctx, room); err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	for i := range 2 {
		if _, err := subject.AttachWatchdog(ctx, "acme", "room-1", control.Watchdog{
			Kind: control.WatchdogRecording, Health: control.WatchdogAttached,
			Descriptor: fmt.Sprintf("recorder %d", i),
		}); err != nil {
			t.Fatalf("attaching watchdog %d: %v", i, err)
		}
	}

	_, err := subject.AttachWatchdog(ctx, "acme", "room-1", control.Watchdog{Kind: control.WatchdogSafety})
	if !errors.Is(err, control.ErrInvalid) {
		t.Errorf("exceeding the watchdog limit gave %v, want ErrInvalid", err)
	}

	watchdogs, err := subject.ListWatchdogs(ctx, "acme", "room-1")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(watchdogs) != 2 {
		t.Errorf("room has %d watchdogs, want 2", len(watchdogs))
	}

	detached, err := subject.DetachWatchdog(ctx, "acme", "room-1", watchdogs[0].ID)
	if err != nil {
		t.Fatalf("detaching: %v", err)
	}
	if detached.ID != watchdogs[0].ID {
		t.Errorf("detached %s, asked for %s", detached.ID, watchdogs[0].ID)
	}
	if _, err := subject.DetachWatchdog(ctx, "acme", "room-1", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("detaching an absent watchdog gave %v, want ErrNotFound", err)
	}
}

func testLogQuery(t *testing.T, newStore Factory) {
	ctx, subject, advance := setup(t, newStore)

	alice := control.Identity{Username: "alice", Device: "d1"}
	bob := control.Identity{Username: "bob", Device: "d1"}

	for _, entry := range []control.ModerationEntry{
		{Tenant: "acme", Room: "room-1", Verb: control.VerbKick, Target: alice},
		{Tenant: "acme", Room: "room-1", Verb: control.VerbBan, Target: bob},
		{Tenant: "acme", Room: "room-2", Verb: control.VerbKick, Target: alice},
	} {
		if _, err := subject.AppendModerationEntry(ctx, entry); err != nil {
			t.Fatalf("appending: %v", err)
		}
		advance(time.Second)
	}

	// Newest first: an audit view opens on what just happened.
	page, err := subject.ListModerationLog(ctx, store.LogQuery{Tenant: "acme"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("tenant log has %d entries, want 3", len(page.Entries))
	}
	for i := 1; i < len(page.Entries); i++ {
		if page.Entries[i-1].At.Before(page.Entries[i].At) {
			t.Errorf("the log is not newest first: %v then %v", page.Entries[i-1].At, page.Entries[i].At)
		}
	}

	page, err = subject.ListModerationLog(ctx, store.LogQuery{Tenant: "acme", Room: "room-1"})
	if err != nil {
		t.Fatalf("listing one room: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Errorf("room log has %d entries, want 2", len(page.Entries))
	}

	page, err = subject.ListModerationLog(ctx, store.LogQuery{Tenant: "acme", Target: alice})
	if err != nil {
		t.Fatalf("listing by target: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Errorf("target filter returned %d entries, want 2", len(page.Entries))
	}

	page, err = subject.ListModerationLog(ctx, store.LogQuery{
		Tenant: "acme", Verbs: []control.ModerationVerb{control.VerbBan},
	})
	if err != nil {
		t.Fatalf("listing by verb: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Verb != control.VerbBan {
		t.Errorf("verb filter returned %d entries", len(page.Entries))
	}
}

// A moderator told an action succeeded, whose target keeps talking, stops
// trusting the tool. So enforcement is recorded separately from the decision and
// can be corrected when a relay confirms late.
func testMarkEnforced(t *testing.T, newStore Factory) {
	ctx, subject, _ := setup(t, newStore)

	entry, err := subject.AppendModerationEntry(ctx, control.ModerationEntry{
		Tenant: "acme", Room: "room-1", Verb: control.VerbKick, Enforced: false,
	})
	if err != nil {
		t.Fatalf("appending: %v", err)
	}

	if err := subject.MarkEnforced(ctx, "acme", entry.ID, true); err != nil {
		t.Fatalf("marking enforced: %v", err)
	}
	page, err := subject.ListModerationLog(ctx, store.LogQuery{Tenant: "acme"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page.Entries) != 1 || !page.Entries[0].Enforced {
		t.Error("the entry was not marked enforced")
	}

	if err := subject.MarkEnforced(ctx, "acme", "nope", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("marking an absent entry gave %v, want ErrNotFound", err)
	}
}
