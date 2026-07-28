//! What a participant is allowed to do.

use bitflags::bitflags;

bitflags! {
    /// A set of moderation powers.
    ///
    /// A set, never a level. Levels force an ordering between powers that have none --
    /// "ban permanently" and "promote to cohost" are incomparable -- and the ordering
    /// that does matter is the partial one: an actor may act on a target only when the
    /// actor's set strictly contains the target's. Two moderators with identical powers
    /// therefore cannot act on each other, which is the property every level scheme
    /// gets wrong.
    ///
    /// The bit positions are protocol rather than an implementation detail: a token
    /// carries them, the relay reads them, and `MoqomKit`'s `Capability.rawValue` uses
    /// the same ones. The gaps between groups are deliberate, so a power added later
    /// takes an unused bit in its own range instead of shifting every bit after it and
    /// quietly reinterpreting tokens already issued.
    #[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
    pub struct Capability: u32 {
        /// Delete a chat message.
        const DELETE_MESSAGE = 1 << 0;
        /// Time a user out of chat.
        const TIMEOUT_USER = 1 << 1;
        /// Ban a user from chat while leaving them able to watch.
        const BAN_FROM_CHAT = 1 << 2;
        /// Change chat mode: followers only, slow mode, and so on.
        const SET_CHAT_MODE = 1 << 3;
        /// Manage chat word filters.
        const MANAGE_FILTERS = 1 << 4;

        /// Promote a viewer to a guest who may speak.
        const PROMOTE_TO_GUEST = 1 << 8;
        /// Promote to cohost.
        const PROMOTE_TO_COHOST = 1 << 9;
        /// Remove someone from the stage.
        const DEMOTE = 1 << 10;
        /// Stop someone's media without their client's cooperation.
        const FORCE_MUTE = 1 << 11;
        /// Put one participant on everyone's main tile.
        const SPOTLIGHT = 1 << 12;
        /// Manage the queue of people waiting to join the stage.
        const MANAGE_QUEUE = 1 << 13;
        /// Stop anyone new joining the stage.
        const LOCK_STAGE = 1 << 14;

        /// Remove someone from the room, without a lasting record.
        const KICK = 1 << 16;
        /// Refuse someone's future sessions in this room.
        const BAN_FROM_ROOM = 1 << 17;
        /// Refuse someone's future sessions across the tenant.
        const BAN_FROM_TENANT = 1 << 18;
        /// End the room for everyone.
        const CLOSE_ROOM = 1 << 19;
        /// Change the room's policy.
        const CHANGE_ROOM_POLICY = 1 << 20;
        /// Attach and detach observers.
        const MANAGE_WATCHDOGS = 1 << 21;

        /// Appoint moderators. Never delegated by anyone but an owner.
        const APPOINT_MODERATORS = 1 << 24;
        /// Read the audit trail, which the power to act does not imply.
        const READ_MODERATION_LOG = 1 << 25;
        /// Handle appeals against moderation decisions.
        const HANDLE_APPEALS = 1 << 26;

        /// Every chat power.
        const CHAT = Self::DELETE_MESSAGE.bits()
            | Self::TIMEOUT_USER.bits()
            | Self::BAN_FROM_CHAT.bits()
            | Self::SET_CHAT_MODE.bits()
            | Self::MANAGE_FILTERS.bits();

        /// Every stage power.
        const STAGE = Self::PROMOTE_TO_GUEST.bits()
            | Self::PROMOTE_TO_COHOST.bits()
            | Self::DEMOTE.bits()
            | Self::FORCE_MUTE.bits()
            | Self::SPOTLIGHT.bits()
            | Self::MANAGE_QUEUE.bits()
            | Self::LOCK_STAGE.bits();

        /// Every room power.
        const ROOM = Self::KICK.bits()
            | Self::BAN_FROM_ROOM.bits()
            | Self::BAN_FROM_TENANT.bits()
            | Self::CLOSE_ROOM.bits()
            | Self::CHANGE_ROOM_POLICY.bits()
            | Self::MANAGE_WATCHDOGS.bits();

        /// Every meta power.
        const META = Self::APPOINT_MODERATORS.bits()
            | Self::READ_MODERATION_LOG.bits()
            | Self::HANDLE_APPEALS.bits();
    }
}

impl Capability {
    /// Every bit this version knows.
    ///
    /// A bit outside it is refused rather than dropped, so a set this build does not
    /// understand fails loudly instead of granting less than it says.
    pub const EVERYTHING: Self = Self::from_bits_truncate(
        Self::CHAT.bits() | Self::STAGE.bits() | Self::ROOM.bits() | Self::META.bits(),
    );
}

bitflags! {
    /// What a participant may send.
    ///
    /// Orthogonal to [`Capability`], because publishing and moderating compose freely: a
    /// chat moderator who cannot speak and a cohost who cannot moderate are both
    /// ordinary, and one ladder covering both would make each of them a special case.
    ///
    /// A grant is permission and never an action. [`Publish::VIDEO`] does not turn a
    /// camera on.
    #[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
    pub struct Publish: u8 {
        /// Microphone.
        const AUDIO = 1 << 0;
        /// Camera.
        const VIDEO = 1 << 1;
        /// Screen or window capture.
        const SCREEN_SHARE = 1 << 2;
        /// Application data on the room's data track.
        const DATA = 1 << 3;

        /// Microphone and camera, which is what a guest usually gets.
        const CAMERA = Self::AUDIO.bits() | Self::VIDEO.bits();
        /// Everything.
        const EVERYTHING = Self::AUDIO.bits()
            | Self::VIDEO.bits()
            | Self::SCREEN_SHARE.bits()
            | Self::DATA.bits();
    }
}

/// Everything a participant is allowed to do.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Grants {
    /// What they may send.
    pub publish: Publish,

    /// What they may moderate.
    pub moderation: Capability,

    /// Ownership, which is not a capability.
    ///
    /// Keeping it out of the bitmask is the point rather than an oversight: it cannot
    /// be granted, cannot be demoted away, and makes its holder immune to every
    /// capability set however complete. As a bit, a sufficiently powerful moderator
    /// could take it -- and "sufficiently powerful" is what an attacker who has
    /// compromised a moderator account has.
    pub is_owner: bool,
}

impl Grants {
    /// A viewer: no publishing, no moderation.
    #[must_use]
    pub const fn viewer() -> Self {
        Self {
            publish: Publish::empty(),
            moderation: Capability::empty(),
            is_owner: false,
        }
    }

    /// Someone who may speak and appear, with no moderation powers.
    #[must_use]
    pub const fn guest() -> Self {
        Self {
            publish: Publish::CAMERA,
            moderation: Capability::empty(),
            is_owner: false,
        }
    }

    /// A label for display only.
    ///
    /// Never a source of truth and never stored. It exists because interfaces need a
    /// word, and the words people use -- "cohost", "guest" -- are summaries of a
    /// capability set rather than anything a room actually has.
    #[must_use]
    pub fn role(&self) -> Role {
        if self.is_owner {
            Role::Owner
        } else if self.publish.contains(Publish::VIDEO) && !self.moderation.is_empty() {
            Role::Cohost
        } else if !self.publish.is_empty() {
            Role::Guest
        } else if !self.moderation.is_empty() {
            Role::Moderator
        } else {
            Role::Viewer
        }
    }
}

/// A label derived from grants, for display only.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Role {
    /// The room's owner.
    Owner,
    /// Publishes video and moderates.
    Cohost,
    /// Publishes without moderating.
    Guest,
    /// Moderates without publishing.
    Moderator,
    /// Neither.
    Viewer,
}

/// A named bundle of capabilities, for the cases every deployment has.
///
/// Expanded by the server on the way in and never stored: what is stored is the
/// resulting set, so redefining "moderator" next year does not retroactively redefine
/// what this year's moderators were allowed to do.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Preset {
    /// A viewer: nothing.
    None,
    /// Chat powers.
    Moderator,
    /// Chat and stage powers -- the difference a "super mod" usually means, which is
    /// the power to promote and remove speakers.
    SuperModerator,
    /// Every capability. Note that this is still not ownership, which is
    /// [`Grants::is_owner`] and is not grantable.
    Owner,
}

impl Preset {
    /// The capabilities this preset names.
    ///
    /// Provided so a caller can show what a preset means before applying it. The server
    /// expands independently and its answer is the one that counts.
    #[must_use]
    pub fn expand(self) -> Capability {
        match self {
            Preset::None => Capability::empty(),
            Preset::Moderator => Capability::CHAT,
            Preset::SuperModerator => Capability::CHAT | Capability::STAGE,
            Preset::Owner => Capability::EVERYTHING,
        }
    }
}
