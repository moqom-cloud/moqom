// Package memory is a Store that keeps everything in a map.
//
// It exists so the services can be tested without a database, and it is a real
// implementation rather than a stub: it holds the same invariants, including the
// awkward ones -- idempotency, contiguous per-room event sequences, stable
// ordering under paging -- because a fake that is easier to satisfy than the real
// thing tests the wrong system.
//
// It is also what a single-node deployment or a laptop can run against, which is
// worth something on its own. What it is not is durable, and nothing here
// pretends otherwise.
package memory

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// Store is an in-memory implementation of store.Store.
type Store struct {
	// One lock for everything.
	//
	// Finer locking would be faster and would also let a caller observe a room
	// whose ban list has been updated and whose moderation entry has not.
	// Uniform coarse locking is what makes the multi-write operations atomic
	// without a transaction concept, which is the whole reason this can stand in
	// for a database.
	mu sync.Mutex

	rooms        map[roomKey]control.Room
	participants map[roomKey]map[control.Identity]control.Participant
	bans         map[roomKey][]control.Ban
	mutes        map[roomKey][]store.Mute
	overrides    map[roomKey]map[control.Identity]control.Grants
	watchdogs    map[roomKey][]control.Watchdog
	log          []control.ModerationEntry
	byKey        map[idempotencyKey]int
	events       []store.Event
	sequences    map[roomKey]uint64

	now func() time.Time
	ids func() string
}

type roomKey struct {
	tenant string
	room   string
}

type idempotencyKey struct {
	tenant string
	key    string
}

// New returns an empty store.
//
// The clock and the ID source are injected because the tests assert on both:
// ordering that only holds when two writes land in different milliseconds is
// ordering that fails on a fast machine.
func New(now func() time.Time, ids func() string) *Store {
	if now == nil {
		now = time.Now
	}
	if ids == nil {
		ids = defaultIDs()
	}
	return &Store{
		rooms:        map[roomKey]control.Room{},
		participants: map[roomKey]map[control.Identity]control.Participant{},
		bans:         map[roomKey][]control.Ban{},
		mutes:        map[roomKey][]store.Mute{},
		overrides:    map[roomKey]map[control.Identity]control.Grants{},
		watchdogs:    map[roomKey][]control.Watchdog{},
		byKey:        map[idempotencyKey]int{},
		sequences:    map[roomKey]uint64{},
		now:          now,
		ids:          ids,
	}
}

func defaultIDs() func() string {
	var mu sync.Mutex
	var counter uint64
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("mem-%d", counter)
	}
}

func (s *Store) CreateRoom(_ context.Context, room control.Room) (control.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{room.Tenant, room.Room}
	if _, exists := s.rooms[key]; exists {
		return control.Room{}, fmt.Errorf("%w: room %s/%s", control.ErrAlreadyExists, room.Tenant, room.Room)
	}
	if room.CreatedAt.IsZero() {
		room.CreatedAt = s.now()
	}
	room.Policy = room.Policy.WithDefaults()
	s.rooms[key] = room
	return room, nil
}

func (s *Store) GetRoom(_ context.Context, tenant, room string) (control.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getRoomLocked(tenant, room)
}

func (s *Store) getRoomLocked(tenant, room string) (control.Room, error) {
	found, ok := s.rooms[roomKey{tenant, room}]
	if !ok {
		return control.Room{}, fmt.Errorf("%w: room %s/%s", store.ErrNotFound, tenant, room)
	}
	return found, nil
}

func (s *Store) SaveRoom(_ context.Context, room control.Room) (control.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{room.Tenant, room.Room}
	if _, ok := s.rooms[key]; !ok {
		return control.Room{}, fmt.Errorf("%w: room %s/%s", store.ErrNotFound, room.Tenant, room.Room)
	}
	s.rooms[key] = room
	return room, nil
}

func (s *Store) ListRooms(_ context.Context, query store.RoomQuery) (store.RoomPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wanted := map[control.Lifecycle]bool{}
	for _, lifecycle := range query.Lifecycle {
		wanted[lifecycle] = true
	}

	var matched []control.Room
	for key, room := range s.rooms {
		if key.tenant != query.Tenant {
			continue
		}
		if len(wanted) > 0 && !wanted[room.Lifecycle] {
			continue
		}
		matched = append(matched, room)
	}
	// By ID rather than by creation time: paging needs a total order that does
	// not change as rooms are created, and two rooms created in the same
	// millisecond have no order by time at all.
	sort.Slice(matched, func(i, j int) bool { return matched[i].Room < matched[j].Room })

	after, err := decodeToken(query.PageToken)
	if err != nil {
		return store.RoomPage{}, err
	}
	if after != "" {
		matched = matched[upperBound(len(matched), func(i int) bool { return matched[i].Room > after }):]
	}

	limit := int(query.Clamp())
	page := store.RoomPage{}
	if len(matched) > limit {
		page.NextPageToken = encodeToken(matched[limit-1].Room)
		matched = matched[:limit]
	}
	page.Rooms = matched
	return page, nil
}

func (s *Store) SaveParticipant(_ context.Context, tenant, room string, participant control.Participant) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{tenant, room}
	if _, ok := s.rooms[key]; !ok {
		return fmt.Errorf("%w: room %s/%s", store.ErrNotFound, tenant, room)
	}
	if s.participants[key] == nil {
		s.participants[key] = map[control.Identity]control.Participant{}
	}
	if participant.JoinedAt.IsZero() {
		participant.JoinedAt = s.now()
	}
	participant.LastSeenAt = s.now()
	s.participants[key][participant.Identity] = participant
	return nil
}

func (s *Store) GetParticipant(_ context.Context, tenant, room string, id control.Identity) (control.Participant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.participants[roomKey{tenant, room}][id]
	if !ok {
		return control.Participant{}, fmt.Errorf("%w: participant %s in %s/%s", store.ErrNotFound, id, tenant, room)
	}
	return found, nil
}

func (s *Store) ListParticipants(_ context.Context, query store.ParticipantQuery) (store.ParticipantPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var matched []control.Participant
	for _, participant := range s.participants[roomKey{query.Tenant, query.Room}] {
		if participant.Backstage && !query.IncludeBackstage {
			continue
		}
		if participant.Presence == control.PresenceLeft && !query.IncludeDeparted {
			continue
		}
		matched = append(matched, participant)
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Identity.String() < matched[j].Identity.String()
	})

	after, err := decodeToken(query.PageToken)
	if err != nil {
		return store.ParticipantPage{}, err
	}
	if after != "" {
		matched = matched[upperBound(len(matched), func(i int) bool {
			return matched[i].Identity.String() > after
		}):]
	}

	limit := int(query.Clamp())
	page := store.ParticipantPage{}
	if len(matched) > limit {
		page.NextPageToken = encodeToken(matched[limit-1].Identity.String())
		matched = matched[:limit]
	}
	page.Participants = matched
	return page, nil
}

func (s *Store) DeleteParticipant(_ context.Context, tenant, room string, id control.Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	participants := s.participants[roomKey{tenant, room}]
	if _, ok := participants[id]; !ok {
		return fmt.Errorf("%w: participant %s", store.ErrNotFound, id)
	}
	delete(participants, id)
	return nil
}

func (s *Store) PutBan(_ context.Context, ban control.Ban) (control.Ban, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ban.ID == "" {
		ban.ID = s.ids()
	}
	if ban.CreatedAt.IsZero() {
		ban.CreatedAt = s.now()
	}
	key := roomKey{ban.Tenant, ban.Room}
	existing := s.bans[key]
	for i, candidate := range existing {
		if sameSubject(candidate, ban) {
			// Replace rather than append. Two bans on one subject would make
			// lifting one leave the other in force, which reads to a moderator
			// as an unban that did not work.
			ban.ID = candidate.ID
			ban.CreatedAt = candidate.CreatedAt
			existing[i] = ban
			s.bans[key] = existing
			return ban, nil
		}
	}
	s.bans[key] = append(existing, ban)
	return ban, nil
}

func sameSubject(a, b control.Ban) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case control.BanByUsername:
		return a.Username == b.Username
	case control.BanByDevice:
		return a.Username == b.Username && a.Device == b.Device
	case control.BanByAddress:
		return a.IPPrefix == b.IPPrefix
	default:
		return false
	}
}

func (s *Store) DeleteBan(_ context.Context, tenant, room string, kind control.BanKind, subject control.Ban) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	subject.Kind = kind
	key := roomKey{tenant, room}
	for i, candidate := range s.bans[key] {
		if sameSubject(candidate, subject) {
			s.bans[key] = append(s.bans[key][:i], s.bans[key][i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: no such ban", store.ErrNotFound)
}

func (s *Store) ListBans(_ context.Context, tenant, room string) ([]control.Ban, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var live []control.Ban
	// Room bans and the tenant-wide ones that also apply. The union is computed
	// here so a relay is handed the answer rather than the rule.
	for _, key := range []roomKey{{tenant, room}, {tenant, ""}} {
		for _, ban := range s.bans[key] {
			if ban.ExpiresAt.IsZero() || now.Before(ban.ExpiresAt) {
				live = append(live, ban)
			}
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	return live, nil
}

// One row per kind, so that a second mute cannot lift the first and each kind
// keeps the expiry it was given.
func (s *Store) PutMute(_ context.Context, mute store.Mute) (store.Mute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if mute.CreatedAt.IsZero() {
		mute.CreatedAt = s.now()
	}
	key := roomKey{mute.Tenant, mute.Room}
	for _, kind := range mute.Media.Each() {
		row := mute
		row.Media = kind
		if i := s.findMute(key, mute.Identity, kind, mute.AudienceOnly); i >= 0 {
			s.mutes[key][i] = row
			continue
		}
		s.mutes[key] = append(s.mutes[key], row)
	}
	return mute, nil
}

// ErrNotFound only when nothing at all was lifted. A moderator lifting audio
// and video from somebody who was only ever muted on audio has got what they
// asked for.
func (s *Store) DeleteMute(
	_ context.Context, tenant, room string, id control.Identity,
	media control.MediaKind, audienceOnly bool,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{tenant, room}
	kept := make([]store.Mute, 0, len(s.mutes[key]))
	lifted := false
	for _, candidate := range s.mutes[key] {
		if candidate.Identity == id && candidate.AudienceOnly == audienceOnly &&
			candidate.Media&media != 0 {
			lifted = true
			continue
		}
		kept = append(kept, candidate)
	}
	s.mutes[key] = kept
	if !lifted {
		return fmt.Errorf("%w: no such mute", store.ErrNotFound)
	}
	return nil
}

func (s *Store) findMute(
	key roomKey, id control.Identity, kind control.MediaKind, audienceOnly bool,
) int {
	for i, candidate := range s.mutes[key] {
		if candidate.Identity == id && candidate.AudienceOnly == audienceOnly &&
			candidate.Media == kind {
			return i
		}
	}
	return -1
}

func (s *Store) ListMutes(_ context.Context, tenant, room string) ([]store.Mute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var live []store.Mute
	for _, mute := range s.mutes[roomKey{tenant, room}] {
		if !mute.Expired(now) {
			live = append(live, mute)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].Identity != live[j].Identity {
			return live[i].Identity.String() < live[j].Identity.String()
		}
		if live[i].AudienceOnly != live[j].AudienceOnly {
			return !live[i].AudienceOnly && live[j].AudienceOnly
		}
		return live[i].Media < live[j].Media
	})
	return live, nil
}

func (s *Store) PutGrantOverride(_ context.Context, tenant, room string, id control.Identity, grants control.Grants) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{tenant, room}
	if s.overrides[key] == nil {
		s.overrides[key] = map[control.Identity]control.Grants{}
	}
	s.overrides[key][id] = grants
	return nil
}

func (s *Store) ListGrantOverrides(_ context.Context, tenant, room string) (map[control.Identity]control.Grants, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := map[control.Identity]control.Grants{}
	for id, grants := range s.overrides[roomKey{tenant, room}] {
		copied[id] = grants
	}
	return copied, nil
}

func (s *Store) AttachWatchdog(_ context.Context, tenant, room string, watchdog control.Watchdog) (control.Watchdog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.rooms[roomKey{tenant, room}]
	if !ok {
		return control.Watchdog{}, fmt.Errorf("%w: room %s/%s", store.ErrNotFound, tenant, room)
	}
	key := roomKey{tenant, room}
	limit := int(found.Policy.WithDefaults().WatchdogLimit)
	if len(s.watchdogs[key]) >= limit {
		return control.Watchdog{}, fmt.Errorf("%w: room already has %d watchdogs, limit %d", control.ErrInvalid, len(s.watchdogs[key]), limit)
	}
	if watchdog.ID == "" {
		watchdog.ID = s.ids()
	}
	if watchdog.AttachedAt.IsZero() {
		watchdog.AttachedAt = s.now()
	}
	s.watchdogs[key] = append(s.watchdogs[key], watchdog)
	return watchdog, nil
}

func (s *Store) DetachWatchdog(_ context.Context, tenant, room, id string) (control.Watchdog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{tenant, room}
	for i, watchdog := range s.watchdogs[key] {
		if watchdog.ID == id {
			s.watchdogs[key] = append(s.watchdogs[key][:i], s.watchdogs[key][i+1:]...)
			return watchdog, nil
		}
	}
	return control.Watchdog{}, fmt.Errorf("%w: watchdog %s", store.ErrNotFound, id)
}

func (s *Store) ListWatchdogs(_ context.Context, tenant, room string) ([]control.Watchdog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	watchdogs := make([]control.Watchdog, len(s.watchdogs[roomKey{tenant, room}]))
	copy(watchdogs, s.watchdogs[roomKey{tenant, room}])
	sort.Slice(watchdogs, func(i, j int) bool { return watchdogs[i].ID < watchdogs[j].ID })
	return watchdogs, nil
}

func (s *Store) AppendModerationEntry(_ context.Context, entry control.ModerationEntry) (control.ModerationEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The retry check and the append are one critical section. Checking first and
	// appending after would let two deliveries of the same queued action both
	// find nothing and both write, which for "ban for 24 hours" means 48.
	if entry.IdempotencyKey != "" {
		if index, seen := s.byKey[idempotencyKey{entry.Tenant, entry.IdempotencyKey}]; seen {
			return s.log[index], nil
		}
	}
	if entry.ID == "" {
		entry.ID = s.ids()
	}
	if entry.At.IsZero() {
		entry.At = s.now()
	}
	s.log = append(s.log, entry)
	if entry.IdempotencyKey != "" {
		s.byKey[idempotencyKey{entry.Tenant, entry.IdempotencyKey}] = len(s.log) - 1
	}
	return entry, nil
}

func (s *Store) FindModerationEntry(_ context.Context, tenant, key string) (control.ModerationEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, ok := s.byKey[idempotencyKey{tenant, key}]
	if !ok {
		return control.ModerationEntry{}, fmt.Errorf("%w: no entry for key %q", store.ErrNotFound, key)
	}
	return s.log[index], nil
}

func (s *Store) MarkEnforced(_ context.Context, tenant, id string, enforced bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.log {
		if s.log[i].Tenant == tenant && s.log[i].ID == id {
			s.log[i].Enforced = enforced
			return nil
		}
	}
	return fmt.Errorf("%w: moderation entry %s", store.ErrNotFound, id)
}

func (s *Store) ListModerationLog(_ context.Context, query store.LogQuery) (store.LogPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	verbs := map[control.ModerationVerb]bool{}
	for _, verb := range query.Verbs {
		verbs[verb] = true
	}

	var matched []control.ModerationEntry
	for _, entry := range s.log {
		if entry.Tenant != query.Tenant {
			continue
		}
		if query.Room != "" && entry.Room != query.Room {
			continue
		}
		if !query.Since.IsZero() && entry.At.Before(query.Since) {
			continue
		}
		if !query.Until.IsZero() && !entry.At.Before(query.Until) {
			continue
		}
		if !query.Target.IsZero() && !matchesTarget(entry, query.Target) {
			continue
		}
		if len(verbs) > 0 && !verbs[entry.Verb] {
			continue
		}
		matched = append(matched, entry)
	}
	// Newest first: an audit interface opens on what just happened, and an
	// appeals queue works backwards from the most recent decision.
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].At.Equal(matched[j].At) {
			return matched[i].At.After(matched[j].At)
		}
		return matched[i].ID > matched[j].ID
	})

	after, err := decodeToken(query.PageToken)
	if err != nil {
		return store.LogPage{}, err
	}
	if after != "" {
		for i, entry := range matched {
			if entry.ID == after {
				matched = matched[i+1:]
				break
			}
		}
	}

	limit := int(query.Clamp())
	page := store.LogPage{}
	if len(matched) > limit {
		page.NextPageToken = encodeToken(matched[limit-1].ID)
		matched = matched[:limit]
	}
	page.Entries = matched
	return page, nil
}

func matchesTarget(entry control.ModerationEntry, target control.Identity) bool {
	if entry.Target.Username != target.Username {
		return false
	}
	// A query naming no device asks about the person, and matches entries against
	// any of their devices as well as entries against all of them.
	return target.Device == "" || entry.Target.Device == "" || entry.Target.Device == target.Device
}

func (s *Store) AppendEvent(_ context.Context, event store.Event) (store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := roomKey{event.Tenant, event.Room}
	s.sequences[key]++
	event.Sequence = s.sequences[key]
	if event.At.IsZero() {
		event.At = s.now()
	}
	s.events = append(s.events, event)
	return event, nil
}

func (s *Store) ListEvents(_ context.Context, query store.EventQuery) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kinds := map[store.EventKind]bool{}
	for _, kind := range query.Kinds {
		kinds[kind] = true
	}

	var matched []store.Event
	for _, event := range s.events {
		if event.Tenant != query.Tenant {
			continue
		}
		if query.Room != "" && event.Room != query.Room {
			continue
		}
		if event.Sequence <= query.AfterSequence {
			continue
		}
		if len(kinds) > 0 && !kinds[event.Kind] {
			continue
		}
		matched = append(matched, event)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Room != matched[j].Room {
			return matched[i].Room < matched[j].Room
		}
		return matched[i].Sequence < matched[j].Sequence
	})
	if query.Limit > 0 && len(matched) > int(query.Limit) {
		matched = matched[:query.Limit]
	}
	return matched, nil
}

// Page tokens are opaque and encoded, so nobody builds one by hand and then
// depends on its shape when the ordering key changes.
func encodeToken(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: page token is not one we issued", control.ErrInvalid)
	}
	if !strings.ContainsRune(string(decoded), 0) {
		return string(decoded), nil
	}
	return "", fmt.Errorf("%w: page token is malformed", control.ErrInvalid)
}

// upperBound returns the first index where predicate holds, or n.
func upperBound(n int, predicate func(int) bool) int {
	low, high := 0, n
	for low < high {
		middle := (low + high) / 2
		if predicate(middle) {
			high = middle
		} else {
			low = middle + 1
		}
	}
	return low
}
