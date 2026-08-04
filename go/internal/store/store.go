// Package store is the persistence seam.
//
// Two implementations, one interface, one test suite. The suite is the point:
// the in-memory store exists so the services can be tested without a database,
// and it is only worth having if "passes the tests" means the same thing for
// both. Every behaviour the services rely on -- idempotency, event sequence
// contiguity, the ordering of a list -- is asserted there rather than in either
// implementation, so a Postgres query that is subtly different fails rather than
// diverging quietly.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// ErrNotFound is returned for a row that is not there. Distinguished from an
// empty list, which is a legitimate answer to a query.
var ErrNotFound = control.ErrNotFound

// ErrConflict is returned when a write lost a race and is worth retrying.
//
// Separate from ErrAlreadyExists, which is a caller error and is not: one means
// "try again", the other means "you asked for something impossible".
var ErrConflict = errors.New("write conflict")

// Store is everything the control plane keeps.
//
// One interface rather than several, despite the size. The services need
// transactional relationships across these groups -- an eject writes a
// moderation entry, a ban, and an event, and a partial application of those
// three is a moderator who was told a lie -- so splitting them by noun would put
// the seam in the wrong place. Grouping is by comment.
type Store interface {
	// Rooms.
	CreateRoom(ctx context.Context, room control.Room) (control.Room, error)
	GetRoom(ctx context.Context, tenant, room string) (control.Room, error)
	ListRooms(ctx context.Context, query RoomQuery) (RoomPage, error)
	// SaveRoom writes policy and lifecycle. Whole-record rather than per-field
	// because a room is small and read-modify-write on a small record is easier
	// to reason about than a mask.
	SaveRoom(ctx context.Context, room control.Room) (control.Room, error)

	// Participants. Soft state: losing all of it loses nobody's room, which is
	// what makes a relay restart survivable.
	SaveParticipant(ctx context.Context, tenant, room string, participant control.Participant) error
	GetParticipant(ctx context.Context, tenant, room string, id control.Identity) (control.Participant, error)
	ListParticipants(ctx context.Context, query ParticipantQuery) (ParticipantPage, error)
	// DeleteParticipant removes the record entirely, for a departure that is
	// past the room's retention. Ordinary leaving sets presence instead, so
	// "who was here when this happened" still has an answer.
	DeleteParticipant(ctx context.Context, tenant, room string, id control.Identity) error

	// Enforcement state, which is what a relay is given and what it checks
	// against on every subscribe and publish.
	PutBan(ctx context.Context, ban control.Ban) (control.Ban, error)
	DeleteBan(ctx context.Context, tenant, room string, kind control.BanKind, subject control.Ban) error
	// ListBans returns the bans in force for a room, including the tenant-wide
	// ones that apply to it. A relay that had to work out that union itself
	// would be a second implementation of the rule.
	ListBans(ctx context.Context, tenant, room string) ([]control.Ban, error)

	// PutMute writes one row per media kind it names, replacing any row for the
	// same participant, scope and kind. Kinds it does not name are untouched:
	// muting a camera is not a statement about a microphone.
	PutMute(ctx context.Context, mute Mute) (Mute, error)
	// DeleteMute lifts only the kinds in media, for the given scope. Pass
	// control.MediaAll to lift everything the scope holds.
	DeleteMute(ctx context.Context, tenant, room string, id control.Identity, media control.MediaKind, audienceOnly bool) error
	ListMutes(ctx context.Context, tenant, room string) ([]Mute, error)

	// Grant overrides are what make a demotion immediate: they take precedence
	// over whatever a token already in the wild happens to claim.
	PutGrantOverride(ctx context.Context, tenant, room string, id control.Identity, grants control.Grants) error
	ListGrantOverrides(ctx context.Context, tenant, room string) (map[control.Identity]control.Grants, error)

	// Watchdogs.
	AttachWatchdog(ctx context.Context, tenant, room string, watchdog control.Watchdog) (control.Watchdog, error)
	DetachWatchdog(ctx context.Context, tenant, room, id string) (control.Watchdog, error)
	ListWatchdogs(ctx context.Context, tenant, room string) ([]control.Watchdog, error)

	// The audit trail.
	//
	// AppendModerationEntry is where idempotency lives. It has to be here rather
	// than in a service: two replicas handling a retried request concurrently
	// can only be resolved by whatever holds the uniqueness constraint.
	AppendModerationEntry(ctx context.Context, entry control.ModerationEntry) (control.ModerationEntry, error)
	FindModerationEntry(ctx context.Context, tenant, idempotencyKey string) (control.ModerationEntry, error)
	MarkEnforced(ctx context.Context, tenant, id string, enforced bool) error
	ListModerationLog(ctx context.Context, query LogQuery) (LogPage, error)

	// The event log the Events service streams from.
	//
	// AppendEvent assigns the sequence number, because contiguity within a room
	// is a promise to consumers and only the writer can keep it.
	AppendEvent(ctx context.Context, event Event) (Event, error)
	ListEvents(ctx context.Context, query EventQuery) ([]Event, error)
}

// Mute is a force-mute in force.
//
// Keyed by identity, audience-only-ness and media kind. The first two because a
// participant can be under both scopes at once: audio silenced for everybody
// and screenshare hidden from the audience is one participant with two mutes,
// and merging them would lose which is which. The kind because the same is true
// one level down, and it is the part that was wrong first — a mute keyed only by
// scope meant the second one replaced the first, so muting a camera turned a
// microphone back on.
//
// At rest each row names exactly one kind, which is also the only way an expiry
// can be honest: a ten-minute mute of audio and an indefinite mute of video have
// no single ExpiresAt between them. Media may name several kinds on the way into
// PutMute, which writes a row for each.
type Mute struct {
	Tenant       string
	Room         string
	Identity     control.Identity
	Media        control.MediaKind
	AudienceOnly bool
	ExpiresAt    time.Time
	CreatedAt    time.Time
}

// Expired reports whether this mute has lapsed.
func (m Mute) Expired(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
}

// Event is one thing that happened in a room.
type Event struct {
	Tenant string
	Room   string
	// Sequence is assigned by the store, contiguous within a room, never reused.
	Sequence uint64
	At       time.Time
	Kind     EventKind
	// Body is the marshalled protobuf body of the event.
	//
	// Opaque here on purpose. The store's job is ordering and durability; making
	// it understand the shape of every event would mean a migration for every
	// new one, and the streaming service already has the generated types.
	Body []byte
}

// EventKind is the discriminator, kept out of the opaque body so a consumer can
// filter without the store having to decode anything.
type EventKind string

const (
	EventParticipantJoined EventKind = "participant_joined"
	EventParticipantLeft   EventKind = "participant_left"
	EventPresenceChanged   EventKind = "presence_changed"
	EventGrantsChanged     EventKind = "grants_changed"
	EventMuteChanged       EventKind = "mute_changed"
	EventSpotlightChanged  EventKind = "spotlight_changed"
	EventPublishChanged    EventKind = "publish_changed"
	EventWatchdogChanged   EventKind = "watchdog_changed"
	EventLifecycleChanged  EventKind = "lifecycle_changed"
	EventModerationApplied EventKind = "moderation_applied"
)

// Paging is deliberately a token rather than an offset.
//
// An offset over a table that is being written to skips and repeats rows, which
// for a moderation log means an auditor who cannot tell either happened. The
// token encodes the last key seen.
type Paging struct {
	PageSize  uint32
	PageToken string
}

// DefaultPageSize applies when a caller asks for none, and MaxPageSize caps what
// they can ask for: an unbounded list is a denial of service against the
// database, and a caller who wants everything can page.
const (
	DefaultPageSize = 100
	MaxPageSize     = 1000
)

// Clamp returns the page size to actually use.
func (p Paging) Clamp() uint32 {
	switch {
	case p.PageSize == 0:
		return DefaultPageSize
	case p.PageSize > MaxPageSize:
		return MaxPageSize
	default:
		return p.PageSize
	}
}

type RoomQuery struct {
	Tenant    string
	Lifecycle []control.Lifecycle
	Paging
}

type RoomPage struct {
	Rooms         []control.Room
	NextPageToken string
}

type ParticipantQuery struct {
	Tenant           string
	Room             string
	IncludeBackstage bool
	IncludeDeparted  bool
	Paging
}

type ParticipantPage struct {
	Participants  []control.Participant
	NextPageToken string
}

type LogQuery struct {
	Tenant string
	// Room empty for tenant-wide, which is what an appeals queue reads.
	Room   string
	Since  time.Time
	Until  time.Time
	Target control.Identity
	Verbs  []control.ModerationVerb
	Paging
}

type LogPage struct {
	Entries       []control.ModerationEntry
	NextPageToken string
}

type EventQuery struct {
	Tenant string
	// Room empty for every room in the tenant.
	Room string
	// AfterSequence resumes a consumer where it stopped. Only meaningful with a
	// room, since sequences are per room.
	AfterSequence uint64
	Kinds         []EventKind
	Limit         uint32
}
