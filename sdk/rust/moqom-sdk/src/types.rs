//! The vocabulary a room is described in.

use std::time::{Duration, SystemTime};

use crate::grants::Grants;

/// Who a participant is: an account and one of its devices.
///
/// There is no numeric user ID anywhere in MOQOM and nowhere to put one. The username is
/// the customer's own string, and the device is derived from the device key's JWK
/// thumbprint -- so a device ban and the thumbprint passed when minting are the same
/// fact, and a migration from a platform with its own UID space needs no mapping table.
#[derive(Debug, Clone, Default, PartialEq, Eq, Hash)]
pub struct Identity {
    /// The account.
    pub username: String,
    /// Which device.
    pub device: String,
}

/// Who to act on.
///
/// An empty `device` is meaningful rather than missing: it reaches every device that
/// person is signed in on, which is almost always what a kick or a ban means. Naming one
/// device is for the narrower case -- one compromised laptop -- and confusing the two is
/// how a ban fails to land.
#[derive(Debug, Clone, Default, PartialEq, Eq, Hash)]
pub struct ParticipantRef {
    /// The account.
    pub username: String,
    /// One device, or empty for all of them.
    pub device: String,
}

/// Every device of an account. The usual case.
#[must_use]
pub fn target(username: impl Into<String>) -> ParticipantRef {
    ParticipantRef {
        username: username.into(),
        device: String::new(),
    }
}

/// One device of one account.
#[must_use]
pub fn device(username: impl Into<String>, device: impl Into<String>) -> ParticipantRef {
    ParticipantRef {
        username: username.into(),
        device: device.into(),
    }
}

/// Who an action is attributed to, and why.
///
/// The service account is established by the credentials on the connection and cannot be
/// set here -- a caller that could name its own actor could forge an audit trail. What
/// this adds is the human the backend is acting for, which the server has no way to know:
/// a kick from a customer's own moderation console is attributable to that moderator
/// rather than to an API key.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Actor {
    /// The moderator this action is on behalf of. Empty for something the backend
    /// decided on its own, such as an automated safety verdict.
    pub on_behalf_of: String,

    /// Free text, stored on the moderation entry and shown in appeals. Worth insisting
    /// on internally even where the server does not require it.
    pub reason: String,

    /// Deduplication key.
    ///
    /// Left empty, one is generated per call, which covers a retry inside this process
    /// but not a redelivery from a queue -- for that, derive a key from the message and
    /// set it here.
    pub idempotency_key: String,
}

impl Actor {
    /// Attribute this action to a person.
    #[must_use]
    pub fn on_behalf_of(username: impl Into<String>) -> Self {
        Self {
            on_behalf_of: username.into(),
            ..Self::default()
        }
    }

    /// Record why.
    #[must_use]
    pub fn because(mut self, reason: impl Into<String>) -> Self {
        self.reason = reason.into();
        self
    }

    /// Use a specific idempotency key, so a redelivery from another process
    /// deduplicates against this attempt.
    #[must_use]
    pub fn keyed(mut self, key: impl Into<String>) -> Self {
        self.idempotency_key = key.into();
        self
    }
}

/// A kind of media, for mutes.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Media {
    /// Microphone.
    Audio,
    /// Camera.
    Video,
    /// Screen or window capture.
    ScreenShare,
}

impl Media {
    /// Every kind, for a mute that means all of it.
    pub const EVERYTHING: [Media; 3] = [Media::Audio, Media::Video, Media::ScreenShare];
}

/// Whether someone is here.
///
/// Published on the room's control track rather than inferred from silence, because every
/// client would otherwise infer it differently and none could tell a tunnel from a
/// hang-up.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Presence {
    /// Here.
    Connected,
    /// Transport lost, inside the window where it is expected back. Subscriptions are
    /// held open and nobody is told to rediscover anything.
    Reconnecting,
    /// Gone long enough to say so, not long enough to be treated as having left.
    Away,
    /// Suspended by the operating system rather than by the network. Distinct because
    /// the cause is known and the return is usually immediate.
    Backgrounded,
    /// Gone.
    Left,
    /// A state this version does not know, from a newer control plane.
    Unknown,
}

/// What phase a room is in.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Lifecycle {
    /// Created with a start time, not yet open.
    Scheduled,
    /// Open.
    Live,
    /// The owner is absent and a cohost holds moderation powers temporarily. Not a
    /// transfer: it is reclaimed on the owner's return and ownership never moves.
    ActingHost,
    /// Over.
    Ended,
    /// A phase this version does not know.
    Unknown,
}

/// Whether recording is permitted, required, or refused.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum RecordingPolicy {
    /// Take the server's default.
    #[default]
    Unset,
    /// A recorder may attach.
    Allowed,
    /// A room that cannot start without a recorder attached, for customers whose
    /// obligation is to have the tape rather than to be allowed one.
    Required,
    /// A recording watchdog is refused, including the tenant's own.
    Prohibited,
}

/// Why a session ended, when a moderator ended it.
///
/// Sent to the client so it can say something true before the stream goes dark. A stream
/// that simply stops is indistinguishable from a crashed app, and the support ticket that
/// follows is expensive.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum Code {
    /// Unset.
    #[default]
    Unset,
    /// Nudity or sexual content.
    AdultContent,
    /// Violence.
    Violence,
    /// Harassment of another person.
    Harassment,
    /// Self-harm.
    SelfHarm,
    /// Copyright.
    Copyright,
    /// The room requires age verification the session did not assert.
    AgeVerificationRequired,
    /// The tenant's own policy.
    TenantPolicy,
    /// A human decided, and no policy category covers it.
    OperatorAction,
}

/// Which action an audit entry records.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum Verb {
    /// Unset, or a verb this version does not know.
    #[default]
    Unknown,
    /// Removed from the room.
    Kick,
    /// Refused future sessions.
    Ban,
    /// Lifted a ban.
    Unban,
    /// Stopped someone's media.
    ForceMute,
    /// Restored someone's media.
    Unmute,
    /// Changed what someone may do.
    SetGrants,
    /// Moved the spotlight.
    Spotlight,
    /// Ended the room.
    CloseRoom,
    /// Attached an observer.
    AttachWatchdog,
    /// Detached an observer.
    DetachWatchdog,
    /// Changed the room's policy.
    UpdateRoomPolicy,
}

/// How far a ban reaches.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum BanScope {
    /// Unset.
    #[default]
    Unset,
    /// This room only.
    Room,
    /// Every room in the tenant.
    Tenant,
}

/// Who stops receiving the media.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum MuteScope {
    /// Unset.
    #[default]
    Unset,
    /// Everybody, including the other hosts. Relay enforced, so it holds whether or not
    /// the client cooperates.
    Everyone,
    /// Viewers only; hosts and cohosts still receive it. A subscription scope rather
    /// than a second copy of the track, which is what makes "cohosts can still talk to
    /// each other" free.
    Audience,
}

/// A room's age requirement.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct AgeGate {
    /// Minimum age in years. Zero for no gate.
    pub minimum_age: u32,

    /// Demand the tenant's own verification rather than a self-declared age. The tenant
    /// performs it; MOQOM records that the room demands it and refuses sessions whose
    /// token does not assert it.
    pub require_verification: bool,
}

/// A room's settings.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Policy {
    /// Who may join, by age.
    pub age_gate: AgeGate,

    /// Whether recording is permitted, required, or refused.
    pub recording: RecordingPolicy,

    /// Where this room's data may live, including derived data: inference outputs,
    /// recordings, and moderation evidence, which is the part usually missed.
    ///
    /// Set at creation and immutable afterwards -- not because changing it is hard but
    /// because it would be a lie. Data already written cannot be recalled from another
    /// jurisdiction by editing a field.
    pub residency_zone: String,

    /// A device count rather than a person count, because devices are what consume relay
    /// resources. Zero for no limit.
    pub max_participants: u32,

    /// How many participants may publish video at once. A client-side decode budget as
    /// much as a policy, which is why the server defaults it to 8 rather than to
    /// something ambitious.
    pub stage_slots: u32,

    /// How many observers may attach. Also a real limit: each one is a subscriber the
    /// relay fans out to.
    pub watchdog_limit: u32,
}

/// A room's state.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoomState {
    /// Which tenant.
    pub tenant: String,

    /// The room's identifier within the tenant.
    pub name: String,

    /// The MOQT namespace prefix, slash-separated. Returned rather than left to the
    /// caller to construct: the tuple layout is protocol, and a backend that guesses at
    /// it breaks when the layout changes.
    pub namespace: String,

    /// Its settings.
    pub policy: Policy,

    /// What phase it is in.
    pub lifecycle: Lifecycle,

    /// Set when scheduled.
    pub starts_at: Option<SystemTime>,

    /// When it was created.
    pub created_at: Option<SystemTime>,

    /// When it ended.
    pub ended_at: Option<SystemTime>,

    /// Set when the lifecycle is [`Lifecycle::ActingHost`].
    pub acting_host: Option<Identity>,

    /// Devices currently in the room, which is not the number of people.
    pub participant_count: u32,

    /// People currently in the room, counting a person on three devices once.
    pub distinct_user_count: u32,

    /// Whether a recording watchdog is attached.
    pub is_recording: bool,
}

/// One device in a room.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Participant {
    /// Who and which device.
    pub identity: Identity,

    /// What they are allowed to do.
    pub grants: Grants,

    /// Whether they are here.
    pub presence: Presence,

    /// What this participant chose not to send. Reported, not enforced: a self-mute is
    /// the client's decision, and the room is told so other clients can show it.
    pub self_muted: Vec<Media>,

    /// What a moderator stopped for everybody. Relay enforced, so it survives a
    /// reconnect and does not need the client's help.
    pub forced_muted_for_everyone: Vec<Media>,

    /// What the audience cannot hear but the other hosts can.
    pub forced_muted_for_audience: Vec<Media>,

    /// Whether they are on everyone's main tile.
    pub is_spotlighted: bool,

    /// Whether they are announced under the backstage prefix, outside the one viewers
    /// subscribe to. Going live is a namespace change rather than a renegotiation.
    pub is_backstage: bool,

    /// When they joined.
    pub joined_at: Option<SystemTime>,

    /// Which relay holds this session. Operational detail, useful when a support
    /// question turns out to be a question about one machine.
    pub relay_id: String,
}

/// What an observer is for.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum WatchdogKind {
    /// Unset, or a kind this version does not know.
    #[default]
    Unspecified,
    /// Writes the room to storage.
    Recording,
    /// Classifies content for policy.
    Safety,
    /// Measures.
    Analytics,
    /// Retains for a regulator.
    Compliance,
    /// Something else, named in [`Watchdog::custom_kind`].
    Custom,
}

/// Who runs an observer.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum WatchdogOperator {
    /// Unset.
    #[default]
    Unspecified,
    /// MOQOM itself.
    Platform,
    /// The tenant.
    Tenant,
    /// Somebody else, named in [`Watchdog::operator_name`].
    ThirdParty,
}

/// Whether an observer is working, which is not whether it is there.
///
/// Separate from [`Presence`] because the questions differ: a participant that stops
/// sending has left, while a recorder that stops recording is a failure somebody has to
/// be told about -- especially in a room whose policy is [`RecordingPolicy::Required`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[non_exhaustive]
pub enum WatchdogHealth {
    /// Unset, or a state this version does not know.
    #[default]
    Unspecified,
    /// Working.
    Attached,
    /// Working badly.
    Degraded,
    /// Not working.
    Failed,
}

/// An observer that is not a participant.
///
/// Recorders, safety classifiers, and analytics are subscribers with no publish rights,
/// disclosed as room state rather than hidden: being recorded is something a room is told
/// about. They are structurally incapable of appearing in the roster, because a roster
/// entry is proof of a namespace announcement and a watchdog never makes one.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Watchdog {
    /// Server-assigned identifier.
    pub id: String,

    /// What it is for.
    pub kind: WatchdogKind,

    /// Free text when the kind is [`WatchdogKind::Custom`].
    pub custom_kind: String,

    /// Who runs it.
    pub operated_by: WatchdogOperator,

    /// Named when operated by a third party, so a room can disclose who is watching
    /// rather than only that something is.
    pub operator_name: String,

    /// Shown to clients. This is what a room says about being recorded, so it is worth
    /// writing for a person rather than for a log.
    pub descriptor: String,

    /// The namespace prefix it subscribes under, which bounds what it can see. Empty for
    /// the whole room.
    pub scope: String,

    /// Whether it is working.
    pub health: WatchdogHealth,

    /// When it attached.
    pub attached_at: Option<SystemTime>,

    /// Whether its fan-out is charged to the tenant. Platform safety classifiers are
    /// not; a tenant's own recorder is.
    pub is_billable: bool,

    /// Subscribes to one rung only. A classifier usually needs 360p, and letting it take
    /// 1080p because nothing stopped it is how observation gets expensive.
    pub pinned_layer: String,
}

/// What a ban is keyed on.
///
/// Three keys because evasion is easy against any one of them: an account ban is beaten
/// by a new account, a device ban by a factory reset, an address ban by a mobile
/// network's NAT pool. They are also unequally blunt.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BanSubject {
    /// Every device signed in as this account.
    Account(String),

    /// One device, which survives a new account on the same hardware -- the useful one
    /// against a repeat offender.
    Device(Identity),

    /// An address or CIDR prefix. Blunt: an address is shared by a household or a
    /// campus, so this is the one most likely to catch somebody innocent. Prefer the
    /// others and expect collateral.
    Address(String),
}

/// One line of the audit trail.
///
/// Denormalised on purpose: the actor, the target, and the reason are copied in rather
/// than referenced, because a record that changes meaning when a room is deleted or a
/// display name is edited is not an audit record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entry {
    /// Server-assigned identifier.
    pub id: String,
    /// Which tenant.
    pub tenant: String,
    /// Which room. Empty for a tenant-wide action.
    pub room: String,
    /// When.
    pub at: Option<SystemTime>,
    /// What was done.
    pub verb: Verb,

    /// The account that made the call, filled by the server from the credentials and
    /// never from the request.
    pub service_account: String,

    /// The human the action was attributed to, if any.
    pub on_behalf_of: String,

    /// Who it was done to. Absent for a ban keyed on an address, which is nobody's
    /// identity -- that is in [`Entry::ban_subject`].
    pub target: Option<ParticipantRef>,

    /// What a ban was keyed on.
    pub ban_subject: Option<BanSubject>,

    /// Why, as a category.
    pub code: Code,

    /// Why, in words.
    pub reason: String,

    /// When the effect lapses. Absent for permanent or instantaneous actions.
    pub expires_at: Option<SystemTime>,

    /// Which media a mute covered.
    pub media: Vec<Media>,

    /// Which mute.
    pub mute_scope: MuteScope,

    /// How far a ban reached.
    pub ban_scope: BanScope,

    /// The resulting grants, for [`Verb::SetGrants`].
    pub grants: Grants,

    /// Whether the relays holding affected sessions confirmed the directive.
    ///
    /// False means the record exists and enforcement is incomplete, because a relay was
    /// unreachable. It is surfaced rather than hidden: a moderator who is told a kick
    /// succeeded and finds the person still talking will not trust the tool again. The
    /// control plane retries; this reports the state meanwhile.
    pub enforced: bool,

    /// Names the model and version when a classifier decided rather than a person.
    pub decided_by_model: String,
}

/// A freshly minted client token.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Token {
    /// The compact form the client passes in MOQT Setup Options.
    pub token: String,

    /// When it stops working.
    pub expires_at: Option<SystemTime>,

    /// Who this token will present as, with the device already derived from the
    /// thumbprint. Returned so a backend can record what it handed out without
    /// reimplementing the derivation.
    pub identity: Identity,

    /// Which signing key was used.
    pub key_id: String,
}

/// A public key tokens can be verified with.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VerificationKey {
    /// Which key.
    pub key_id: String,

    /// The algorithm as a JOSE identifier. `EdDSA` over Ed25519 today: small signatures,
    /// fast verification on a relay's hot path, and no curve parameters to get wrong.
    pub algorithm: String,

    /// The public key, as JSON.
    pub jwk: String,

    /// When it starts signing.
    pub not_before: Option<SystemTime>,

    /// When it stops signing. It stays published until every token it signed has
    /// expired, or rotation becomes an outage.
    pub retires_at: Option<SystemTime>,
}

/// How a token is tied to its holder.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Binding {
    /// Whoever holds the token is the user, so a leaked token is a leaked account until
    /// it expires. This is what is implemented today, and the reason lifetimes are
    /// short.
    Bearer,

    /// The client proves possession of the device key on every session, so the token is
    /// useless without the secure element holding the private half. The target, and what
    /// the thumbprint already exists to support.
    DPoP,

    /// A binding this version does not know.
    Unknown,
}

/// One page of a listing, with the token for the next.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Page<T> {
    /// This page.
    pub items: Vec<T>,

    /// Empty when this was the last page.
    pub next: String,
}

/// How long a lifetime or duration is, or the server's default.
pub(crate) fn optional(duration: Duration) -> Option<prost_types::Duration> {
    if duration.is_zero() {
        None
    } else {
        Some(prost_types::Duration {
            seconds: i64::try_from(duration.as_secs()).unwrap_or(i64::MAX),
            nanos: i32::try_from(duration.subsec_nanos()).unwrap_or(0),
        })
    }
}
