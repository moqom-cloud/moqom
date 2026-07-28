//! Creating rooms, reading them, and what is watching them.

use std::time::SystemTime;

use crate::client::{Client, Room, call};
use crate::convert::{
    identity_out, lifecycle_out, operator_out, participant_in, policy_out, room_in, time_out,
    watchdog_in, watchdog_kind_out,
};
use crate::error::{Error, Result};
use crate::proto;
use crate::types::{
    Actor, Code, Identity, Lifecycle, Page, Participant, Policy, RoomState, Watchdog, WatchdogKind,
    WatchdogOperator,
};

/// The request to create a room.
#[derive(Debug, Clone, Default)]
pub struct NewRoom {
    /// The room's identifier within the tenant, supplied rather than generated: a
    /// customer's rooms already have names in their own system, and a second identifier
    /// would mean a mapping table.
    pub name: String,

    /// Its settings.
    pub policy: Policy,

    /// The initial owner. Optional -- a room may exist before anyone joins, with
    /// ownership assigned when the host's token is minted.
    pub owner: String,

    /// Set to create the room scheduled rather than live.
    pub starts_at: Option<SystemTime>,

    /// Who is asking, and why.
    pub actor: Actor,
}

/// Filters a room listing.
#[derive(Debug, Clone, Default)]
pub struct RoomQuery {
    /// Filter to these phases. Empty for all of them.
    pub lifecycle: Vec<Lifecycle>,

    /// How many per page. Zero for the server's default.
    pub page_size: u32,

    /// Where to resume, from a previous [`Page::next`].
    pub page_token: String,
}

/// Changes policy on a live room.
#[derive(Debug, Clone, Default)]
pub struct PolicyUpdate {
    /// The new policy.
    pub policy: Policy,

    /// Which parts of `policy` to apply, using the proto field names: `age_gate`,
    /// `recording`, `max_participants`, `stage_slots`, `watchdog_limit`.
    ///
    /// Empty applies all of them, which is a read-modify-write and will clobber a
    /// concurrent change. Naming the fields you meant to change is almost always what you
    /// want.
    pub fields: Vec<String>,

    /// Who is asking, and why.
    pub actor: Actor,
}

/// Filters a roster listing.
#[derive(Debug, Clone, Default)]
pub struct ParticipantQuery {
    /// Include participants who are backstage. Excluded by default, because the common
    /// question is "who is in the room" and backstage deliberately is not.
    pub include_backstage: bool,

    /// Include participants who have left but whose record is still within retention.
    /// Useful for "who was here when this happened".
    pub include_departed: bool,

    /// How many per page.
    pub page_size: u32,

    /// Where to resume.
    pub page_token: String,
}

/// The request to attach an observer.
#[derive(Debug, Clone, Default)]
pub struct NewWatchdog {
    /// What it is for.
    pub kind: WatchdogKind,

    /// Required when `kind` is [`WatchdogKind::Custom`].
    pub custom_kind: String,

    /// Who runs it.
    pub operated_by: WatchdogOperator,

    /// Required when operated by a third party: a room that discloses it is being watched
    /// should be able to say by whom.
    pub operator_name: String,

    /// Shown to clients, so write it for a person.
    pub descriptor: String,

    /// The namespace prefix to observe. Empty for the whole room.
    pub scope: String,

    /// Subscribe to one rung only. Set it for a classifier: 360p is usually enough, and
    /// taking 1080p because nothing stopped it is how observation gets expensive.
    pub pinned_layer: String,

    /// Whether the fan-out is charged to the tenant.
    pub is_billable: bool,

    /// Who is asking, and why.
    pub actor: Actor,
}

impl Client {
    /// Create a room.
    ///
    /// # Errors
    ///
    /// [`Error::Exists`] if the room is already there, rather than replacing it: a retry
    /// must not silently reset a live room's policy.
    pub async fn create_room(&self, request: NewRoom) -> Result<RoomState> {
        if request.name.is_empty() {
            return Err(Error::request("moqom: a room needs a name"));
        }

        let wire = proto::CreateRoomRequest {
            tenant: self.tenant.clone(),
            room: request.name,
            policy: Some(policy_out(&request.policy)),
            owner_username: request.owner,
            starts_at: time_out(request.starts_at),
            actor: Some(self.actor(&request.actor)),
        };

        let room = call(self.retry, "create_room", || {
            let mut rooms = self.rooms.clone();
            let wire = wire.clone();
            async move { rooms.create_room(wire).await }
        })
        .await?;

        Ok(room_in(room.into_inner()))
    }

    /// List rooms in the tenant.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn rooms(&self, query: RoomQuery) -> Result<Page<RoomState>> {
        let wire = proto::ListRoomsRequest {
            tenant: self.tenant.clone(),
            lifecycle: query
                .lifecycle
                .iter()
                .map(|lifecycle| lifecycle_out(*lifecycle))
                .collect(),
            page_size: query.page_size,
            page_token: query.page_token,
        };

        let response = call(self.retry, "rooms", || {
            let mut rooms = self.rooms.clone();
            let wire = wire.clone();
            async move { rooms.list_rooms(wire).await }
        })
        .await?
        .into_inner();

        Ok(Page {
            items: response.rooms.into_iter().map(room_in).collect(),
            next: response.next_page_token,
        })
    }
}

impl Room<'_> {
    /// Read the room.
    ///
    /// # Errors
    ///
    /// [`Error::NotFound`] if there is no such room, which is also the answer for a room
    /// in another tenant.
    pub async fn get(&self) -> Result<RoomState> {
        let wire = proto::GetRoomRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
        };

        let room = call(self.client.retry, "room.get", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.get_room(wire).await }
        })
        .await?;

        Ok(room_in(room.into_inner()))
    }

    /// Change policy on a live room.
    ///
    /// Tightening applies immediately, including to participants already publishing.
    /// Loosening does not retroactively permit what was recorded under the stricter
    /// policy.
    ///
    /// # Errors
    ///
    /// [`Error::Invalid`] for an attempt to change residency, which is immutable.
    pub async fn update_policy(&self, update: PolicyUpdate) -> Result<RoomState> {
        let wire = proto::UpdateRoomPolicyRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            policy: Some(policy_out(&update.policy)),
            update_mask: update.fields,
            actor: Some(self.client.actor(&update.actor)),
        };

        let room = call(self.client.retry, "room.update_policy", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.update_room_policy(wire).await }
        })
        .await?;

        Ok(room_in(room.into_inner()))
    }

    /// End the room for everyone.
    ///
    /// Every session is ejected with the given code, so clients can say why rather than
    /// going dark. Idempotent: closing a closed room returns it unchanged.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn close(&self, code: Code, actor: Actor) -> Result<RoomState> {
        let wire = proto::CloseRoomRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            code: crate::convert::code_out(code),
            actor: Some(self.client.actor(&actor)),
        };

        let room = call(self.client.retry, "room.close", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.close_room(wire).await }
        })
        .await?;

        Ok(room_in(room.into_inner()))
    }

    /// List the roster.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn participants(&self, query: ParticipantQuery) -> Result<Page<Participant>> {
        let wire = proto::ListParticipantsRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            include_backstage: query.include_backstage,
            include_departed: query.include_departed,
            page_size: query.page_size,
            page_token: query.page_token,
        };

        let response = call(self.client.retry, "room.participants", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.list_participants(wire).await }
        })
        .await?
        .into_inner();

        Ok(Page {
            items: response
                .participants
                .into_iter()
                .map(participant_in)
                .collect(),
            next: response.next_page_token,
        })
    }

    /// Read one device's state.
    ///
    /// # Errors
    ///
    /// [`Error::Request`] without a device: asking about a person rather than a device is
    /// [`Room::participants`] with a filter, because the answer is a list.
    pub async fn participant(&self, identity: &Identity) -> Result<Participant> {
        if identity.username.is_empty() || identity.device.is_empty() {
            return Err(Error::request(
                "moqom: reading a participant needs a username and a device",
            ));
        }

        let wire = proto::GetParticipantRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            identity: Some(identity_out(identity)),
        };

        let participant = call(self.client.retry, "room.participant", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.get_participant(wire).await }
        })
        .await?;

        Ok(participant_in(participant.into_inner()))
    }

    /// Attach an observer that is not a participant.
    ///
    /// # Errors
    ///
    /// [`Error::Denied`] without `MANAGE_WATCHDOGS`, or when the room's policy prohibits
    /// recording.
    pub async fn attach_watchdog(&self, request: NewWatchdog) -> Result<Watchdog> {
        let wire = proto::AttachWatchdogRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            kind: watchdog_kind_out(&request.kind),
            custom_kind: request.custom_kind,
            operated_by: operator_out(request.operated_by),
            operator_name: request.operator_name,
            descriptor: request.descriptor,
            scope: request.scope,
            pinned_layer: request.pinned_layer,
            is_billable: request.is_billable,
            actor: Some(self.client.actor(&request.actor)),
        };

        let watchdog = call(self.client.retry, "room.attach_watchdog", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.attach_watchdog(wire).await }
        })
        .await?;

        Ok(watchdog_in(watchdog.into_inner()))
    }

    /// Remove an observer.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn detach_watchdog(&self, id: impl Into<String>, actor: Actor) -> Result<Watchdog> {
        let wire = proto::DetachWatchdogRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
            watchdog_id: id.into(),
            actor: Some(self.client.actor(&actor)),
        };

        let watchdog = call(self.client.retry, "room.detach_watchdog", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.detach_watchdog(wire).await }
        })
        .await?;

        Ok(watchdog_in(watchdog.into_inner()))
    }

    /// What is observing the room.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn watchdogs(&self) -> Result<Vec<Watchdog>> {
        let wire = proto::ListWatchdogsRequest {
            tenant: self.client.tenant.clone(),
            room: self.name.clone(),
        };

        let response = call(self.client.retry, "room.watchdogs", || {
            let mut rooms = self.client.rooms.clone();
            let wire = wire.clone();
            async move { rooms.list_watchdogs(wire).await }
        })
        .await?
        .into_inner();

        Ok(response.watchdogs.into_iter().map(watchdog_in).collect())
    }
}
