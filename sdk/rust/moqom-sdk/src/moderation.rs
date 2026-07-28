//! Moderator and administrative actions.
//!
//! Every call here returns the [`Entry`] it produced, for two reasons and both about
//! after the fact. The entry is the record an appeal is heard against, so a backend that
//! wants to show a moderator what they did has it without a second read. And its
//! [`Entry::enforced`] field is the honest answer to "did that work": true means the
//! relays holding the affected sessions confirmed the directive, false means the record
//! exists and enforcement is still catching up.

use std::time::{Duration, SystemTime};

use crate::client::{Client, Room, call};
use crate::convert::{
    ban_scope_out, code_out, entry_in, grants_out, media_out, mute_scope_out, preset_out, ref_out,
    subject_out, time_out, verb_out,
};
use crate::error::Result;
use crate::grants::{Grants, Preset};
use crate::proto;
use crate::types::{
    Actor, BanScope, BanSubject, Code, Entry, Identity, Media, MuteScope, Page, ParticipantRef,
    Verb,
};

/// Key a ban on an account: every device signed in as it.
#[must_use]
pub fn banned_account(username: impl Into<String>) -> BanSubject {
    BanSubject::Account(username.into())
}

/// Key a ban on one device, which survives a new account on the same hardware.
#[must_use]
pub fn banned_device(identity: Identity) -> BanSubject {
    BanSubject::Device(identity)
}

/// Key a ban on an address or CIDR prefix. Blunt; expect collateral.
#[must_use]
pub fn banned_address(prefix: impl Into<String>) -> BanSubject {
    BanSubject::Address(prefix.into())
}

/// Removes somebody from a room.
#[derive(Debug, Clone, Default)]
pub struct KickRequest {
    /// Who to remove. [`crate::target`] for every device, [`crate::device`] for one.
    pub target: ParticipantRef,

    /// Sent to the client so it can say why. [`Code::OperatorAction`] when a human decided
    /// and no category fits.
    pub code: Code,

    /// Who is asking, and why.
    pub actor: Actor,
}

/// Refuses future sessions and ends current ones.
#[derive(Debug, Clone)]
pub struct BanRequest {
    /// What the ban is keyed on.
    pub subject: BanSubject,

    /// How long. Zero is permanent, which is what the word means -- a duration is the
    /// softer choice and should be the deliberate one.
    pub duration: Duration,

    /// Why, as a category.
    pub code: Code,

    /// Who is asking, and why.
    pub actor: Actor,
}

impl BanRequest {
    /// A permanent ban on a subject.
    #[must_use]
    pub fn permanent(subject: BanSubject) -> Self {
        Self {
            subject,
            duration: Duration::ZERO,
            code: Code::Unset,
            actor: Actor::default(),
        }
    }

    /// A ban that lapses.
    #[must_use]
    pub fn lasting(subject: BanSubject, duration: Duration) -> Self {
        Self {
            subject,
            duration,
            code: Code::Unset,
            actor: Actor::default(),
        }
    }

    /// Record why, as a category and in words.
    #[must_use]
    pub fn coded(mut self, code: Code, actor: Actor) -> Self {
        self.code = code;
        self.actor = actor;
        self
    }
}

/// Stops or restores somebody's media.
#[derive(Debug, Clone, Default)]
pub struct MuteRequest {
    /// Whose media.
    pub target: ParticipantRef,

    /// Which kinds. Empty means all of them.
    pub media: Vec<Media>,

    /// Who stops receiving it. Required: a participant can be under both an everyone-mute
    /// and an audience-mute at once, and acting on the wrong one looks like the call did
    /// nothing.
    pub scope: MuteScope,

    /// How long, when muting. Zero holds until explicitly lifted.
    pub duration: Duration,

    /// Who is asking, and why.
    pub actor: Actor,
}

/// Changes what somebody may publish and moderate.
#[derive(Debug, Clone, Default)]
pub struct GrantsRequest {
    /// Whose grants.
    pub target: ParticipantRef,

    /// The complete resulting state, not a delta.
    ///
    /// Absolute because a delta cannot be made idempotent: "add cohost" applied twice is
    /// ambiguous about intent, while "these are their grants" applied twice is not.
    pub grants: Grants,

    /// Replaces `grants.moderation` with the preset's expansion. `grants.publish` is still
    /// taken from `grants`, since a preset says nothing about publishing.
    pub preset: Option<Preset>,

    /// Who is asking, and why.
    pub actor: Actor,
}

/// Filters the audit trail.
#[derive(Debug, Clone, Default)]
pub struct LogQuery {
    /// Only entries at or after this time.
    pub since: Option<SystemTime>,

    /// Only entries before this time.
    pub until: Option<SystemTime>,

    /// Only entries about this person or device.
    pub target: Option<ParticipantRef>,

    /// Only these actions. Empty for all of them.
    pub verb: Vec<Verb>,

    /// How many per page.
    pub page_size: u32,

    /// Where to resume.
    pub page_token: String,
}

impl Room<'_> {
    /// Remove a participant now, without a lasting record.
    ///
    /// The difference from [`Room::ban`] is rejoining: a kicked participant may come
    /// straight back, which is the right response to a bad moment rather than a bad actor.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `KICK`, or without standing over the target --
    /// which two moderators with identical powers do not have over each other.
    pub async fn kick(&self, request: KickRequest) -> Result<Entry> {
        if request.target.username.is_empty() {
            return Err(crate::Error::request("moqom: a kick needs a target"));
        }

        let wire = proto::KickRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            target: Some(ref_out(&request.target)),
            code: code_out(request.code),
            actor: Some(self.client.actor(&request.actor)),
        };

        let entry = call(self.client.retry, "room.kick", || {
            let mut moderation = self.client.moderation.clone();
            let wire = wire.clone();
            async move { moderation.kick(wire).await }
        })
        .await?;

        Ok(entry_in(entry.into_inner()))
    }

    /// Refuse future sessions in this room and end current ones.
    ///
    /// Enforced in relay state and rechecked on every subscribe and publish, so a ban
    /// issued while the target is briefly offline still lands when they reconnect.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `BAN_FROM_ROOM`; [`crate::Error::Invalid`] for a
    /// malformed address.
    pub async fn ban(&self, request: BanRequest) -> Result<Entry> {
        ban(self.client, &self.name, BanScope::Room, request).await
    }

    /// Lift a ban on this room before it expires.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn unban(&self, subject: BanSubject, actor: Actor) -> Result<Entry> {
        unban(self.client, &self.name, BanScope::Room, subject, actor).await
    }

    /// Stop somebody's media without asking their client to cooperate.
    ///
    /// [`MuteScope::Everyone`] is relay-enforced and survives reconnection.
    /// [`MuteScope::Audience`] narrows the subscription scope instead, so the other hosts
    /// still hear them -- no second encode, no second track.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Request`] without a scope, since the scope is which mute rather
    /// than a detail.
    pub async fn force_mute(&self, request: MuteRequest) -> Result<Entry> {
        if request.target.username.is_empty() {
            return Err(crate::Error::request("moqom: a mute needs a target"));
        }
        if request.scope == MuteScope::Unset {
            return Err(crate::Error::request("moqom: a mute needs a scope"));
        }

        let wire = proto::ForceMuteRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            target: Some(ref_out(&request.target)),
            media: media_out(&request.media),
            scope: mute_scope_out(request.scope),
            duration: crate::types::optional(request.duration),
            actor: Some(self.client.actor(&request.actor)),
        };

        let entry = call(self.client.retry, "room.force_mute", || {
            let mut moderation = self.client.moderation.clone();
            let wire = wire.clone();
            async move { moderation.force_mute(wire).await }
        })
        .await?;

        Ok(entry_in(entry.into_inner()))
    }

    /// Lift a forced mute.
    ///
    /// The scope has to match the mute being lifted. Lifting an audience-mute does nothing
    /// to an everyone-mute, and the call will report having succeeded because it did.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Request`] without a scope.
    pub async fn unmute(&self, request: MuteRequest) -> Result<Entry> {
        if request.target.username.is_empty() {
            return Err(crate::Error::request("moqom: an unmute needs a target"));
        }
        if request.scope == MuteScope::Unset {
            return Err(crate::Error::request("moqom: an unmute needs a scope"));
        }

        let wire = proto::UnmuteRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            target: Some(ref_out(&request.target)),
            media: media_out(&request.media),
            scope: mute_scope_out(request.scope),
            actor: Some(self.client.actor(&request.actor)),
        };

        let entry = call(self.client.retry, "room.unmute", || {
            let mut moderation = self.client.moderation.clone();
            let wire = wire.clone();
            async move { moderation.unmute(wire).await }
        })
        .await?;

        Ok(entry_in(entry.into_inner()))
    }

    /// Change what somebody may publish and moderate.
    ///
    /// Promotion is additive and rides a token reissue. Demotion is subtractive and does
    /// not wait for one: a demoted cohost who kept publishing until their token expired
    /// would be a hole, so the relay is told directly.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] unless the actor's own capability set contains what is
    /// being granted. `APPOINT_MODERATORS` is never delegated by anyone but an owner.
    pub async fn set_grants(&self, request: GrantsRequest) -> Result<Entry> {
        if request.target.username.is_empty() {
            return Err(crate::Error::request(
                "moqom: setting grants needs a target",
            ));
        }

        let wire = proto::SetGrantsRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            target: Some(ref_out(&request.target)),
            grants: Some(grants_out(request.grants)),
            preset: preset_out(request.preset),
            actor: Some(self.client.actor(&request.actor)),
        };

        let entry = call(self.client.retry, "room.set_grants", || {
            let mut moderation = self.client.moderation.clone();
            let wire = wire.clone();
            async move { moderation.set_grants(wire).await }
        })
        .await?;

        Ok(entry_in(entry.into_inner()))
    }

    /// Put one participant on everyone's main tile, or clear it with `None`.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `SPOTLIGHT`.
    pub async fn spotlight(&self, target: Option<ParticipantRef>, actor: Actor) -> Result<Entry> {
        let wire = proto::SpotlightRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            target: target.as_ref().map(ref_out),
            actor: Some(self.client.actor(&actor)),
        };

        let entry = call(self.client.retry, "room.spotlight", || {
            let mut moderation = self.client.moderation.clone();
            let wire = wire.clone();
            async move { moderation.spotlight(wire).await }
        })
        .await?;

        Ok(entry_in(entry.into_inner()))
    }

    /// Read this room's audit trail.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `READ_MODERATION_LOG`, which is deliberately not
    /// implied by the power to act: the people who can ban and the people who audit bans
    /// are not always the same people.
    pub async fn log(&self, query: LogQuery) -> Result<Page<Entry>> {
        read_log(self.client, &self.name, query).await
    }
}

impl Client {
    /// Refuse sessions across every room in the tenant.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `BAN_FROM_TENANT`, which is not implied by the
    /// power to ban from a room: a room moderator who could ban from the platform would be
    /// a room moderator with the reach of an operator.
    pub async fn ban_from_tenant(&self, request: BanRequest) -> Result<Entry> {
        ban(self, "", BanScope::Tenant, request).await
    }

    /// Lift a tenant-wide ban.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `BAN_FROM_TENANT`.
    pub async fn unban_from_tenant(&self, subject: BanSubject, actor: Actor) -> Result<Entry> {
        unban(self, "", BanScope::Tenant, subject, actor).await
    }

    /// Read the tenant-wide audit trail, which is what an appeals queue wants.
    ///
    /// # Errors
    ///
    /// [`crate::Error::Denied`] without `READ_MODERATION_LOG`.
    pub async fn log(&self, query: LogQuery) -> Result<Page<Entry>> {
        read_log(self, "", query).await
    }
}

async fn ban(client: &Client, room: &str, scope: BanScope, request: BanRequest) -> Result<Entry> {
    let wire = proto::BanRequest {
        tenant: client.tenant.clone(),
        room: room.to_owned(),
        scope: ban_scope_out(scope),
        subject: Some(subject_out(&request.subject)),
        duration: crate::types::optional(request.duration),
        code: code_out(request.code),
        actor: Some(client.actor(&request.actor)),
    };

    let entry = call(client.retry, "ban", || {
        let mut moderation = client.moderation.clone();
        let wire = wire.clone();
        async move { moderation.ban(wire).await }
    })
    .await?;

    Ok(entry_in(entry.into_inner()))
}

async fn unban(
    client: &Client,
    room: &str,
    scope: BanScope,
    subject: BanSubject,
    actor: Actor,
) -> Result<Entry> {
    let wire = proto::UnbanRequest {
        tenant: client.tenant.clone(),
        room: room.to_owned(),
        scope: ban_scope_out(scope),
        subject: Some(subject_out(&subject)),
        actor: Some(client.actor(&actor)),
    };

    let entry = call(client.retry, "unban", || {
        let mut moderation = client.moderation.clone();
        let wire = wire.clone();
        async move { moderation.unban(wire).await }
    })
    .await?;

    Ok(entry_in(entry.into_inner()))
}

async fn read_log(client: &Client, room: &str, query: LogQuery) -> Result<Page<Entry>> {
    let wire = proto::ListModerationLogRequest {
        tenant: client.tenant.clone(),
        room: room.to_owned(),
        since: time_out(query.since),
        until: time_out(query.until),
        target: query.target.as_ref().map(ref_out),
        verb: query.verb.iter().map(|verb| verb_out(*verb)).collect(),
        page_size: query.page_size,
        page_token: query.page_token,
    };

    let response = call(client.retry, "log", || {
        let mut moderation = client.moderation.clone();
        let wire = wire.clone();
        async move { moderation.list_moderation_log(wire).await }
    })
    .await?
    .into_inner();

    Ok(Page {
        items: response.entries.into_iter().map(entry_in).collect(),
        next: response.next_page_token,
    })
}
