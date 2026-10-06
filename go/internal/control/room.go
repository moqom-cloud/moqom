package control

import (
	"errors"
	"fmt"
	"time"
)

// Rule violations, distinguished because they mean different things to a caller.
//
// A gRPC layer maps these to status codes, and getting the mapping right matters
// more than usual here: a moderation tool that retries a PermissionDenied is
// broken, and one that gives up on an Unavailable loses an action a moderator
// believes they took.
var (
	ErrNotFound        = errors.New("not found")
	ErrAlreadyExists   = errors.New("already exists")
	ErrPermission      = errors.New("permission denied")
	ErrInvalid         = errors.New("invalid argument")
	ErrRoomClosed      = errors.New("room is closed")
	ErrImmutablePolicy = errors.New("policy field cannot be changed")
)

// Lifecycle is what phase a room is in.
type Lifecycle string

const (
	LifecycleScheduled  Lifecycle = "scheduled"
	LifecycleLive       Lifecycle = "live"
	LifecycleActingHost Lifecycle = "acting_host"
	LifecycleEnded      Lifecycle = "ended"
)

// RecordingPolicy is whether a room may, must, or must not be recorded.
type RecordingPolicy string

const (
	RecordingAllowed    RecordingPolicy = "allowed"
	RecordingRequired   RecordingPolicy = "required"
	RecordingProhibited RecordingPolicy = "prohibited"
)

// E2EEPolicy is whether a room's media must be end-to-end encrypted.
//
// The strings are protocol: they are the values of the `e2ee` token claim, and
// MoqomKit compares against them byte for byte.
type E2EEPolicy string

const (
	// E2EERequired is the default, and what an unset policy means. A client with
	// no key refuses to publish into such a room and drops what it cannot open.
	E2EERequired E2EEPolicy = "required"

	// E2EEOptional lets media travel in the clear when a client has no key.
	// Reached only by an explicit, reasoned, logged policy change.
	E2EEOptional E2EEPolicy = "optional"
)

// Defaults chosen to be defensible rather than generous. Both limits are real
// costs -- a stage slot is a decode on every client, a watchdog is a fan-out on
// the relay -- so the default is what a room can carry, not what it can imagine.
const (
	DefaultStageSlots    = 8
	DefaultWatchdogLimit = 8
)

type AgeGate struct {
	MinimumAge          uint32
	RequireVerification bool
}

type Policy struct {
	AgeGate         AgeGate
	Recording       RecordingPolicy
	ResidencyZone   string
	MaxParticipants uint32
	StageSlots      uint32
	WatchdogLimit   uint32

	// E2EE is whether media must be end-to-end encrypted. Empty means required,
	// and WithDefaults makes that explicit.
	E2EE E2EEPolicy
}

// WithDefaults fills the zero values that have sensible answers.
//
// For security settings the sensible answer is the strict one. An unset E2EE
// policy is required, so a caller that predates the field -- or simply did not
// think about it -- gets an encrypted room, and an unencrypted one always traces
// back to somebody who asked for it.
func (p Policy) WithDefaults() Policy {
	if p.Recording == "" {
		p.Recording = RecordingAllowed
	}
	if p.E2EE == "" {
		p.E2EE = E2EERequired
	}
	if p.StageSlots == 0 {
		p.StageSlots = DefaultStageSlots
	}
	if p.WatchdogLimit == 0 {
		p.WatchdogLimit = DefaultWatchdogLimit
	}
	return p
}

// ValidateChange reports whether moving from p to next is allowed.
//
// Only residency is immutable, and it is immutable because changing it would be
// a lie rather than because it is hard: data already written in one jurisdiction
// is not recalled by editing a field, and a room whose zone can change is a room
// whose recordings have no answer to "where is this stored".
func (p Policy) ValidateChange(next Policy) error {
	if p.ResidencyZone != next.ResidencyZone {
		return fmt.Errorf("%w: residency_zone, %q to %q", ErrImmutablePolicy, p.ResidencyZone, next.ResidencyZone)
	}
	switch next.E2EE {
	case "", E2EERequired, E2EEOptional:
	default:
		return fmt.Errorf("%w: unknown e2ee policy %q", ErrInvalid, next.E2EE)
	}
	return nil
}

// SecurityChange describes what moving from p to next does to the room's
// security settings, as `field: from -> to`, or "" when nothing moved.
//
// Written into the audit entry for the change. A policy update that turned
// encryption off reads the same as one that changed a stage-slot count unless
// something says which it was.
func (p Policy) SecurityChange(next Policy) string {
	from, to := p.WithDefaults().E2EE, next.WithDefaults().E2EE
	if from == to {
		return ""
	}
	return fmt.Sprintf("e2ee: %s -> %s", from, to)
}

// LoosensSecurity reports whether next is weaker than p in a way that needs a
// recorded reason.
func (p Policy) LoosensSecurity(next Policy) bool {
	return p.WithDefaults().E2EE == E2EERequired && next.WithDefaults().E2EE == E2EEOptional
}

// Presence is where someone is, as published to the room.
type Presence string

const (
	PresenceConnected    Presence = "connected"
	PresenceReconnecting Presence = "reconnecting"
	PresenceAway         Presence = "away"
	PresenceBackgrounded Presence = "backgrounded"
	PresenceLeft         Presence = "left"
)

// MediaKind is which stream a mute or a layer question is about.
type MediaKind uint8

const (
	MediaAudio       MediaKind = 1 << 0
	MediaVideo       MediaKind = 1 << 1
	MediaScreenShare MediaKind = 1 << 2

	MediaAll = MediaAudio | MediaVideo | MediaScreenShare
)

func (m MediaKind) Has(other MediaKind) bool { return m&other == other }

// Each is the kinds named, one at a time, in a fixed order.
//
// For the places that hold a mute per kind rather than per mask -- the store
// and the relay's policy both do, so that muting one kind cannot disturb
// another and so that each kind can carry its own expiry.
func (m MediaKind) Each() []MediaKind {
	var kinds []MediaKind
	for _, kind := range []MediaKind{MediaAudio, MediaVideo, MediaScreenShare} {
		if m&kind != 0 {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// MuteState is the four independent kinds of mute.
//
// Four, not one, because they have four different enforcement points and
// collapsing them loses the difference between "she muted herself" and "a
// moderator muted her" -- which is the difference between a button she can undo
// and one she cannot.
type MuteState struct {
	// The participant's own choice. Reported to the room, enforced by their
	// client. A server that "enforced" it would be overruling someone about
	// their own microphone.
	SelfMuted MediaKind

	// A moderator's decision, enforced by the relay, surviving reconnection.
	ForcedForEveryone MediaKind

	// A moderator's decision that the audience should not hear this, while the
	// other hosts still do. A subscription scope, so it costs no second encode.
	ForcedForAudience MediaKind

	// The fourth kind, a viewer muting somebody on their own device, is
	// deliberately absent. It is a client-side unsubscribe with no server round
	// trip -- the bytes stop at the relay rather than being downloaded and
	// discarded -- so the server has nothing to store and no reason to be told.
}

// Effective is what nobody receives.
//
// Audience-only mutes are excluded on purpose: the track is still flowing, to a
// narrower set of subscribers, and treating it as muted would tell a cohost that
// their guest had gone silent when they can hear them perfectly well.
func (m MuteState) Effective() MediaKind {
	return m.SelfMuted | m.ForcedForEveryone
}

// Participant is one device in a room.
type Participant struct {
	Identity    Identity
	Grants      Grants
	Presence    Presence
	Mute        MuteState
	Spotlighted bool
	Backstage   bool

	// ArrivedVia is the username whose following brought them here, empty when
	// nobody did.
	//
	// Written once, on the session that first put this device in the room, and
	// deliberately not refreshed from later ones. A viewer reconnects and renews
	// tokens for as long as they stay, and taking the newest token's word for it
	// would let a refresh move somebody between audiences halfway through -- so a
	// cohost's count would drift downwards over a long stream for no reason
	// anybody could find. See Participant.Attribute.
	ArrivedVia string

	// Locale is the BCP 47 tag this participant last published on their catalog.
	// Empty until a catalog arrives.
	Locale string

	JoinedAt     time.Time
	LastSeenAt   time.Time
	RelayID      string
	SessionID    string
	RemoteAddr   string
	PublishedNow []string
}

// Attribute records who brought this participant, if nobody has yet.
//
// First writer wins, which is the whole point: provenance is a fact about an
// arrival, and this participant arrived once however many sessions they go on to
// open. Reports whether it took, so a caller can tell a genuine first attribution
// from a reconnect carrying the same claim again.
func (p *Participant) Attribute(via string) bool {
	if via == "" || p.ArrivedVia != "" {
		return false
	}
	// Somebody cannot bring themselves. Cheap to check and worth checking here as
	// well as at mint, because this is the last point before the number is
	// counted.
	if via == p.Identity.Username {
		return false
	}
	p.ArrivedVia = via
	return true
}

// Room is the record a room actually is.
//
// State here outlives every connection to it. A host whose train enters a tunnel
// has not ended their room, and the way to guarantee that is for the room to
// have never been derived from their connection in the first place.
type Room struct {
	Tenant     string
	Room       string
	Policy     Policy
	Lifecycle  Lifecycle
	Owner      string
	ActingHost Identity
	StartsAt   time.Time
	CreatedAt  time.Time
	EndedAt    time.Time
}

// Namespace is the prefix participants announce under.
func (r Room) Namespace() Namespace { return RoomNamespace(r.Tenant, r.Room) }

// IsOpen reports whether new sessions are allowed in.
func (r Room) IsOpen() bool { return r.Lifecycle != LifecycleEnded }

// BanKind is what a ban is keyed on.
type BanKind string

const (
	BanByUsername BanKind = "username"
	BanByDevice   BanKind = "device"
	BanByAddress  BanKind = "address"
)

// Ban refuses sessions.
//
// Three keys because each is separately evadable and unequally blunt: an account
// ban is beaten by a new account, a device ban by a factory reset, an address ban
// by a coffee shop -- and an address ban is also the one most likely to catch
// somebody who did nothing, since a mobile carrier can put a city behind one.
type Ban struct {
	ID        string
	Tenant    string
	Room      string // empty for tenant-wide
	Kind      BanKind
	Username  string
	Device    string
	IPPrefix  string
	Code      string
	Reason    string
	ExpiresAt time.Time // zero for permanent
	CreatedAt time.Time
	CreatedBy string
}

// Matches reports whether this ban applies to a session.
func (b Ban) Matches(id Identity, addr string, now time.Time) bool {
	if !b.ExpiresAt.IsZero() && !now.Before(b.ExpiresAt) {
		return false
	}
	switch b.Kind {
	case BanByUsername:
		return b.Username == id.Username
	case BanByDevice:
		return b.Username == id.Username && b.Device == id.Device
	case BanByAddress:
		return matchesAddress(b.IPPrefix, addr)
	default:
		return false
	}
}

// WatchdogKind is what an observer is for.
type WatchdogKind string

const (
	WatchdogRecording  WatchdogKind = "recording"
	WatchdogSafety     WatchdogKind = "safety"
	WatchdogAnalytics  WatchdogKind = "analytics"
	WatchdogCompliance WatchdogKind = "compliance"
	WatchdogCustom     WatchdogKind = "custom"
)

type WatchdogOperator string

const (
	OperatorPlatform   WatchdogOperator = "platform"
	OperatorTenant     WatchdogOperator = "tenant"
	OperatorThirdParty WatchdogOperator = "third_party"
)

type WatchdogHealth string

const (
	WatchdogAttached WatchdogHealth = "attached"
	WatchdogDegraded WatchdogHealth = "degraded"
	WatchdogFailed   WatchdogHealth = "failed"
)

// Watchdog is something observing the room that is not in it.
//
// The category exists because recorders and classifiers kept being modelled as
// invisible participants, and every place that counted participants then had to
// remember to exclude them. Making it a separate type means the roster cannot
// contain one, which is a guarantee rather than a convention -- and the room is
// still told it is being watched, because concealment was never the goal.
type Watchdog struct {
	ID           string
	Kind         WatchdogKind
	CustomKind   string
	OperatedBy   WatchdogOperator
	OperatorName string
	Descriptor   string
	Scope        Namespace
	Health       WatchdogHealth
	AttachedAt   time.Time
	Billable     bool
	PinnedLayer  string
}

// ModerationVerb is which action was taken.
type ModerationVerb string

const (
	VerbKick             ModerationVerb = "kick"
	VerbBan              ModerationVerb = "ban"
	VerbUnban            ModerationVerb = "unban"
	VerbForceMute        ModerationVerb = "force_mute"
	VerbUnmute           ModerationVerb = "unmute"
	VerbSetGrants        ModerationVerb = "set_grants"
	VerbSpotlight        ModerationVerb = "spotlight"
	VerbCloseRoom        ModerationVerb = "close_room"
	VerbAttachWatchdog   ModerationVerb = "attach_watchdog"
	VerbDetachWatchdog   ModerationVerb = "detach_watchdog"
	VerbUpdateRoomPolicy ModerationVerb = "update_room_policy"
)

// ModerationEntry is one line of the audit trail.
//
// Denormalised on purpose. An entry that resolves a room ID or a display name at
// read time changes meaning when those change, and an audit record whose meaning
// can be edited later is not evidence of anything.
type ModerationEntry struct {
	ID             string
	Tenant         string
	Room           string
	At             time.Time
	Verb           ModerationVerb
	ServiceAccount string
	OnBehalfOf     string
	Target         Identity
	// TargetAllDevices distinguishes "alice" from "alice on this phone". The
	// zero Identity.Device already implies it; this makes it explicit in storage
	// so a later reader does not have to infer intent from an empty string.
	TargetAllDevices bool
	BanKind          BanKind
	BanSubject       string
	BanScopeTenant   bool
	Code             string
	Reason           string
	ExpiresAt        time.Time
	Media            MediaKind
	AudienceOnly     bool
	Grants           *Grants
	Enforced         bool
	DecidedByModel   string
	IdempotencyKey   string

	// PolicyChange is what a policy update did to the room's security settings,
	// as Policy.SecurityChange renders it. Empty for every other verb.
	PolicyChange string
}

// Authorise reports whether an actor with these grants may use verb on target.
//
// One function rather than a check at each call site, because the interesting
// failure is the verb somebody forgets to guard. A new verb added to the switch
// with no case is denied by default, which is the right way round.
func Authorise(actor Grants, verb ModerationVerb, target *Grants) error {
	required, ok := requiredCapability(verb)
	if !ok {
		return fmt.Errorf("%w: unknown verb %q", ErrInvalid, verb)
	}
	if !actor.IsOwner && !actor.Moderation.Has(required) {
		return fmt.Errorf("%w: %s requires %s, actor holds %s", ErrPermission, verb, required, actor.Moderation)
	}
	// Verbs aimed at a person need the superset rule as well as the capability.
	// Holding "kick" says you may kick somebody; it does not say you may kick
	// the person who can also ban you.
	if target != nil && !actor.CanModerate(*target) {
		return fmt.Errorf("%w: %s may not act on %s", ErrPermission, actor.Moderation, target.Moderation)
	}
	return nil
}

func requiredCapability(verb ModerationVerb) (Capability, bool) {
	switch verb {
	case VerbKick:
		return CapKick, true
	case VerbBan, VerbUnban:
		return CapBanFromRoom, true
	case VerbForceMute, VerbUnmute:
		return CapForceMute, true
	case VerbSetGrants:
		return CapPromoteToGuest, true
	case VerbSpotlight:
		return CapSpotlight, true
	case VerbCloseRoom:
		return CapCloseRoom, true
	case VerbAttachWatchdog, VerbDetachWatchdog:
		return CapManageWatchdogs, true
	case VerbUpdateRoomPolicy:
		return CapChangeRoomPolicy, true
	default:
		return 0, false
	}
}
