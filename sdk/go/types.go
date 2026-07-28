package moqom

import "time"

// Identity is who a participant is: an account and one of its devices.
//
// There is no numeric user ID anywhere in MOQOM and nowhere to put one. The username
// is the customer's own string, and the device is derived from the device key's JWK
// thumbprint -- so a device ban and the thumbprint a backend passed to MintToken are
// the same fact, and a migration from a platform with its own UID space needs no
// mapping table.
type Identity struct {
	Username string
	Device   string
}

// ParticipantRef is who to act on.
//
// An empty Device is meaningful rather than missing: it reaches every device that
// person is signed in on, which is almost always what a kick or a ban means. Naming
// one device is for the narrower case -- one compromised laptop -- and confusing the
// two is how a ban fails to land.
type ParticipantRef struct {
	Username string
	Device   string
}

// User targets every device of an account. The usual case.
func User(username string) ParticipantRef {
	return ParticipantRef{Username: username}
}

// Device targets one device of one account.
func Device(username, device string) ParticipantRef {
	return ParticipantRef{Username: username, Device: device}
}

// MediaKind is a kind of media, for mutes.
type MediaKind string

const (
	MediaAudio       MediaKind = "audio"
	MediaVideo       MediaKind = "video"
	MediaScreenShare MediaKind = "screen_share"
)

// AllMedia is every kind, for a mute that means all of it.
var AllMedia = []MediaKind{MediaAudio, MediaVideo, MediaScreenShare}

// Presence is whether someone is here.
//
// Published on the room's control track rather than inferred from silence, because
// every client would otherwise infer it differently and none could tell a tunnel from
// a hang-up.
type Presence string

const (
	PresenceConnected Presence = "connected"
	// PresenceReconnecting is transport lost inside the window where it is expected
	// back. Subscriptions are held open and nobody is told to rediscover anything.
	PresenceReconnecting Presence = "reconnecting"
	// PresenceAway is gone long enough to say so, not long enough to be treated as
	// having left.
	PresenceAway Presence = "away"
	// PresenceBackgrounded is suspended by the operating system rather than by the
	// network. Distinct because the cause is known and the return is usually
	// immediate.
	PresenceBackgrounded Presence = "backgrounded"
	PresenceLeft         Presence = "left"
)

// Lifecycle is what phase a room is in.
type Lifecycle string

const (
	LifecycleScheduled Lifecycle = "scheduled"
	LifecycleLive      Lifecycle = "live"
	// LifecycleActingHost means the owner is absent and a cohost holds moderation
	// powers temporarily. Not a transfer: it is reclaimed on the owner's return and
	// Grants.IsOwner never moves.
	LifecycleActingHost Lifecycle = "acting_host"
	LifecycleEnded      Lifecycle = "ended"
)

// RecordingPolicy is whether recording is permitted, required, or refused.
type RecordingPolicy string

const (
	RecordingAllowed RecordingPolicy = "allowed"
	// RecordingRequired is a room that cannot start without a recorder attached,
	// for customers whose obligation is to have the tape rather than to be allowed
	// one.
	RecordingRequired RecordingPolicy = "required"
	// RecordingProhibited refuses a recording watchdog, including the tenant's own.
	RecordingProhibited RecordingPolicy = "prohibited"
)

// ModerationCode is why a session ended, when a moderator ended it.
//
// Sent to the client so it can say something true before the stream goes dark. A
// stream that simply stops is indistinguishable from a crashed app, and the support
// ticket that follows is expensive.
type ModerationCode string

const (
	CodeAdultContent            ModerationCode = "adult_content"
	CodeViolence                ModerationCode = "violence"
	CodeHarassment              ModerationCode = "harassment"
	CodeSelfHarm                ModerationCode = "self_harm"
	CodeCopyright               ModerationCode = "copyright"
	CodeAgeVerificationRequired ModerationCode = "age_verification_required"
	CodeTenantPolicy            ModerationCode = "tenant_policy"
	// CodeOperatorAction is a human deciding, where no policy category fits.
	CodeOperatorAction ModerationCode = "operator_action"
)

// ModerationVerb is which action an audit entry records.
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

// BanScope is how far a ban reaches.
type BanScope string

const (
	BanFromRoom   BanScope = "room"
	BanFromTenant BanScope = "tenant"
)

// MuteScope is who stops receiving the media.
type MuteScope string

const (
	// MuteForEveryone stops it for everybody, including the other hosts. Relay
	// enforced, so it holds whether or not the client cooperates.
	MuteForEveryone MuteScope = "everyone"
	// MuteForAudience stops it for viewers while hosts and cohosts still receive
	// it. A subscription scope rather than a second copy of the track, which is
	// what makes "cohosts can still talk to each other" free.
	MuteForAudience MuteScope = "audience"
)

// AgeGate is a room's age requirement.
type AgeGate struct {
	// MinimumAge in years. Zero for no gate.
	MinimumAge uint32

	// RequireVerification demands the tenant's own verification rather than a
	// self-declared age. The tenant performs it; MOQOM records that the room
	// demands it and refuses sessions whose token does not assert it.
	RequireVerification bool
}

// Policy is a room's settings.
type Policy struct {
	AgeGate   AgeGate
	Recording RecordingPolicy

	// ResidencyZone is where this room's data may live, including derived data:
	// inference outputs, recordings, and moderation evidence, which is the part
	// usually missed.
	//
	// Set at creation and immutable afterwards -- not because changing it is hard
	// but because it would be a lie. Data already written cannot be recalled from
	// another jurisdiction by editing a field.
	ResidencyZone string

	// MaxParticipants is a device count rather than a person count, because
	// devices are what consume relay resources. Zero for no limit.
	MaxParticipants uint32

	// StageSlots is how many participants may publish video at once. A
	// client-side decode budget as much as a policy, which is why the server
	// defaults it to 8 rather than to something ambitious.
	StageSlots uint32

	// WatchdogLimit is how many observers may attach. Also a real limit: each one
	// is a subscriber the relay fans out to.
	WatchdogLimit uint32
}

// Room is a room's state.
type Room struct {
	Tenant string
	Name   string

	// Namespace is the MOQT namespace prefix, slash-separated. Returned rather
	// than left to the caller to construct: the tuple layout is protocol, and a
	// backend that guesses at it breaks when the layout changes.
	Namespace string

	Policy    Policy
	Lifecycle Lifecycle

	// StartsAt is set when Lifecycle is LifecycleScheduled.
	StartsAt time.Time

	CreatedAt time.Time
	EndedAt   time.Time

	// ActingHost is set when Lifecycle is LifecycleActingHost.
	ActingHost Identity

	// ParticipantCount is devices, which is not the number of people.
	ParticipantCount uint32

	// DistinctUserCount counts a person on three devices once.
	DistinctUserCount uint32

	IsRecording bool
}

// Participant is one device in a room.
type Participant struct {
	Identity Identity
	Grants   Grants
	Presence Presence

	// SelfMuted is what this participant chose not to send. Reported, not
	// enforced: a self-mute is the client's decision, and the room is told so
	// other clients can show it.
	SelfMuted []MediaKind

	// ForcedMutedForEveryone is what a moderator stopped for everybody. Relay
	// enforced, so it survives a reconnect and does not need the client's help.
	ForcedMutedForEveryone []MediaKind

	// ForcedMutedForAudience is what the audience cannot hear but the other hosts
	// can.
	ForcedMutedForAudience []MediaKind

	IsSpotlighted bool

	// IsBackstage is true while this participant is announced under the backstage
	// prefix, outside the one viewers subscribe to. Going live is a namespace
	// change rather than a renegotiation.
	IsBackstage bool

	JoinedAt time.Time

	// RelayID is which relay holds this session. Operational detail, useful when a
	// support question turns out to be a question about one machine.
	RelayID string
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

// WatchdogOperator is who runs an observer.
type WatchdogOperator string

const (
	OperatedByPlatform   WatchdogOperator = "platform"
	OperatedByTenant     WatchdogOperator = "tenant"
	OperatedByThirdParty WatchdogOperator = "third_party"
)

// WatchdogHealth is whether an observer is working, which is not whether it is there.
//
// Separate from Presence because the questions differ: a participant that stops
// sending has left, while a recorder that stops recording is a failure somebody has to
// be told about -- especially in a room whose policy is RecordingRequired.
type WatchdogHealth string

const (
	WatchdogAttached WatchdogHealth = "attached"
	WatchdogDegraded WatchdogHealth = "degraded"
	WatchdogFailed   WatchdogHealth = "failed"
)

// Watchdog is an observer that is not a participant.
//
// Recorders, safety classifiers, and analytics are subscribers with no publish rights,
// disclosed as room state rather than hidden: being recorded is something a room is
// told about. They are structurally incapable of appearing in the roster, because a
// roster entry is proof of a namespace announcement and a watchdog never makes one.
type Watchdog struct {
	ID   string
	Kind WatchdogKind

	// CustomKind is free text when Kind is WatchdogCustom, ignored otherwise.
	CustomKind string

	OperatedBy WatchdogOperator

	// OperatorName is required when OperatedBy is OperatedByThirdParty, so a room
	// can disclose who is watching rather than only that something is.
	OperatorName string

	// Descriptor is shown to clients. This is what a room says about being
	// recorded, so it is worth writing for a person rather than for a log.
	Descriptor string

	// Scope is the namespace prefix this observer subscribes under, which bounds
	// what it can see. Empty for the whole room. A recorder scoped to one
	// participant cannot quietly widen.
	Scope string

	Health     WatchdogHealth
	AttachedAt time.Time

	// IsBillable is whether this observer's fan-out is charged to the tenant.
	// Platform safety classifiers are not; a tenant's own recorder is.
	IsBillable bool

	// PinnedLayer subscribes to one rung only. A classifier usually needs 360p,
	// and letting it take 1080p because nothing stopped it is how observation gets
	// expensive.
	PinnedLayer string
}

// BanSubject is what a ban is keyed on.
//
// Exactly one field is set. Three keys exist because evasion is easy against any one
// of them: an account ban is beaten by a new account, a device ban by a factory reset,
// an address ban by a mobile network's NAT pool.
type BanSubject struct {
	// Username bans every device signed in as this account.
	Username string

	// DeviceOf bans one device, which survives a new account on the same hardware
	// -- the useful one against a repeat offender.
	DeviceOf Identity

	// IPPrefix bans an address or CIDR prefix. Blunt: an address is shared by a
	// household or a campus, so this is the one most likely to catch somebody
	// innocent. Prefer the others and expect collateral.
	IPPrefix string
}

// BanAccount keys a ban on an account.
func BanAccount(username string) BanSubject { return BanSubject{Username: username} }

// BanDevice keys a ban on one device.
func BanDevice(identity Identity) BanSubject { return BanSubject{DeviceOf: identity} }

// BanAddress keys a ban on an address or CIDR prefix.
func BanAddress(prefix string) BanSubject { return BanSubject{IPPrefix: prefix} }

// ModerationEntry is one line of the audit trail.
//
// Denormalised on purpose: the actor, the target, and the reason are copied in rather
// than referenced, because a record that changes meaning when a room is deleted or a
// display name is edited is not an audit record.
type ModerationEntry struct {
	ID     string
	Tenant string
	Room   string
	At     time.Time
	Verb   ModerationVerb

	// ServiceAccount is the account that made the call, filled by the server from
	// the credentials and never from the request.
	ServiceAccount string

	// OnBehalfOf is the human the action was attributed to, if any.
	OnBehalfOf string

	// Target is who it was done to. Empty for a ban keyed on an address, which is
	// nobody's identity -- that is in BanSubject.
	Target     ParticipantRef
	BanSubject BanSubject

	Code   ModerationCode
	Reason string

	// ExpiresAt is when the effect lapses. Zero for permanent or instantaneous
	// actions.
	ExpiresAt time.Time

	// The verb's parameters, so an interface can render "muted audio for the
	// audience for ten minutes" without a second lookup.
	Media     []MediaKind
	MuteScope MuteScope
	BanScope  BanScope
	Grants    Grants

	// Enforced is whether the relays holding affected sessions confirmed the
	// directive.
	//
	// False means the record exists and enforcement is incomplete, because a relay
	// was unreachable. It is surfaced rather than hidden: a moderator who is told a
	// kick succeeded and finds the person still talking will not trust the tool
	// again. The control plane retries; this reports the state meanwhile.
	Enforced bool

	// DecidedByModel names the model and version when a classifier decided rather
	// than a person.
	DecidedByModel string
}

// Token is a freshly minted client token.
type Token struct {
	// Token is the compact form the client passes in MOQT Setup Options.
	Token string

	ExpiresAt time.Time

	// Identity is who this token will present as, with the device already derived
	// from the thumbprint. Returned so a backend can record what it handed out
	// without reimplementing the derivation.
	Identity Identity

	// KeyID is which signing key was used, matching a VerificationKey.
	KeyID string
}

// VerificationKey is a public key tokens can be verified with.
type VerificationKey struct {
	KeyID string

	// Algorithm as a JOSE identifier. EdDSA over Ed25519 today: small signatures,
	// fast verification on a relay's hot path, and no curve parameters to get
	// wrong.
	Algorithm string

	// JWK is the public key, as JSON.
	JWK string

	NotBefore time.Time

	// RetiresAt is when this key stops signing. It stays published until every
	// token it signed has expired, or rotation becomes an outage.
	RetiresAt time.Time
}

// TokenBinding is how a token is tied to its holder.
type TokenBinding string

const (
	// BindingBearer means whoever holds the token is the user, so a leaked token
	// is a leaked account until it expires. This is what is implemented today, and
	// the reason lifetimes are short.
	BindingBearer TokenBinding = "bearer"

	// BindingDPoP means the client proves possession of the device key on every
	// session, so the token is useless without the secure element holding the
	// private half. The target, and what the thumbprint already exists to support.
	BindingDPoP TokenBinding = "dpop"
)

// VerificationKeys is what the control plane publishes for verifying tokens.
type VerificationKeys struct {
	Keys []VerificationKey

	// Binding is what this deployment issues. A client that wants to refuse bearer
	// tokens can find out here rather than by inspecting one.
	Binding TokenBinding
}

// Page is one page of a listing, with the token for the next.
type Page[T any] struct {
	Items []T

	// Next is empty when this was the last page.
	Next string
}
