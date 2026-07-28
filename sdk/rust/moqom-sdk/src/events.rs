//! What is happening in a room, as a stream.
//!
//! A backend's substitute for being in the room. A Rust process *could* subscribe to the
//! room's control track over MOQT and see this as media; this exists because most
//! backends do not want a QUIC stack in their dependency graph to find out that somebody
//! joined.
//!
//! Delivery is at-least-once and ordered per room. Every event carries a sequence number
//! contiguous within its room, so a consumer that records the last one it handled can
//! resume exactly where it stopped -- and can tell that it missed something, which a
//! timestamp cannot.

use std::time::SystemTime;

use tonic::Streaming;

use crate::client::{Client, Room};
use crate::convert::{
    code_in, entry_in, grants_in, identity_in, lifecycle_in, media_in, participant_in, presence_in,
    time_in, watchdog_in,
};
use crate::error::{Error, Result};
use crate::grants::Grants;
use crate::proto;
use crate::types::{Code, Entry, Identity, Lifecycle, Media, Participant, Presence, Watchdog};

/// What happened.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum EventKind {
    /// Somebody's device joined.
    ParticipantJoined,
    /// Somebody's device left.
    ParticipantLeft,
    /// Somebody's presence changed.
    PresenceChanged,
    /// Somebody was promoted or demoted.
    GrantsChanged,
    /// Somebody's mute state changed.
    MuteChanged,
    /// The spotlight moved.
    SpotlightChanged,
    /// A track started.
    PublishStarted,
    /// A track stopped.
    PublishStopped,
    /// An observer attached, changed health, or left.
    WatchdogChanged,
    /// The room changed phase.
    LifecycleChanged,
    /// A moderation action was applied.
    ModerationApplied,
    /// A kind this version does not know, from a newer control plane.
    Unknown,
}

/// One thing that happened in a room.
#[derive(Debug, Clone)]
pub struct Event {
    /// Which tenant.
    pub tenant: String,

    /// Which room.
    pub room: String,

    /// Contiguous within a room and never reused. Persist it: a consumer that resumes from
    /// it can tell it missed something, which a timestamp cannot, and a gap is worth
    /// alerting on rather than papering over.
    pub sequence: u64,

    /// When.
    pub at: Option<SystemTime>,

    /// What kind of thing happened, which says which body is set.
    pub kind: EventKind,

    /// What happened, in detail.
    pub body: Body,
}

/// The detail of an event.
///
/// An enum rather than a set of options, because exactly one applies and a caller matching
/// on it cannot forget to check whether the one it read was populated.
#[derive(Debug, Clone)]
#[non_exhaustive]
pub enum Body {
    /// Somebody arrived.
    Joined(ParticipantJoined),
    /// Somebody left.
    Left(ParticipantLeft),
    /// A presence transition.
    Presence(PresenceChanged),
    /// A promotion or demotion.
    GrantsChanged(GrantsChanged),
    /// A mute change, in full.
    Mute(MuteChanged),
    /// The spotlight moved.
    Spotlight(SpotlightChanged),
    /// A track started or stopped.
    Publish(PublishChanged),
    /// An observer changed.
    Watchdog(WatchdogChanged),
    /// The room changed phase.
    Lifecycle(LifecycleChanged),
    /// The same record the acting caller received, emitted here too so a backend that did
    /// not issue the action still learns of it -- including actions by a safety classifier
    /// or another operator's console.
    Moderation(Entry),
    /// A body this version does not know.
    Unknown,
}

/// Somebody arriving.
#[derive(Debug, Clone)]
pub struct ParticipantJoined {
    /// Who, and their state on arrival.
    pub participant: Participant,

    /// True when this is the person's first device in the room. A room that says "alice
    /// joined" three times because she has three devices is a room whose integrator was
    /// not given this flag.
    pub first_device: bool,
}

/// Somebody going.
#[derive(Debug, Clone)]
pub struct ParticipantLeft {
    /// Who.
    pub identity: Identity,

    /// Whether they were removed rather than leaving. The moderation entry that did it
    /// arrives separately; this is here so a simple consumer does not have to correlate.
    pub was_removed: bool,

    /// Why, when removed.
    pub code: Code,

    /// Whether this was their last device in the room.
    pub last_device: bool,
}

/// A transition between presence states.
#[derive(Debug, Clone)]
pub struct PresenceChanged {
    /// Who.
    pub identity: Identity,
    /// Where from.
    pub from: Presence,
    /// Where to.
    pub to: Presence,
}

/// A promotion or a demotion.
#[derive(Debug, Clone)]
pub struct GrantsChanged {
    /// Who.
    pub identity: Identity,
    /// What they had.
    pub from: Grants,
    /// What they have.
    pub to: Grants,
}

/// The participant's mute state after a change.
///
/// In full rather than as a delta: there are three independent axes, and reconstructing
/// them from deltas is a bug waiting to be written once per consumer.
#[derive(Debug, Clone)]
pub struct MuteChanged {
    /// Who.
    pub identity: Identity,
    /// What they chose not to send.
    pub self_muted: Vec<Media>,
    /// What a moderator stopped for everybody.
    pub forced_muted_for_everyone: Vec<Media>,
    /// What the audience cannot hear.
    pub forced_muted_for_audience: Vec<Media>,
}

/// The spotlight moving.
#[derive(Debug, Clone)]
pub struct SpotlightChanged {
    /// Who is spotlighted, or `None` when it was cleared.
    pub identity: Option<Identity>,
}

/// A track starting or stopping.
#[derive(Debug, Clone)]
pub struct PublishChanged {
    /// Whose track.
    pub identity: Identity,

    /// The full track name -- what started or stopped in the room's own vocabulary rather
    /// than a summary of it.
    pub track: String,

    /// Whether it started.
    pub started: bool,
}

/// An observer attaching, changing health, or leaving.
#[derive(Debug, Clone)]
pub struct WatchdogChanged {
    /// Its state.
    pub watchdog: Watchdog,

    /// True when it left rather than changed state. A recorder detaching is the event a
    /// compliance consumer is waiting for.
    pub detached: bool,
}

/// A room changing phase.
#[derive(Debug, Clone)]
pub struct LifecycleChanged {
    /// Where from.
    pub from: Lifecycle,
    /// Where to.
    pub to: Lifecycle,
    /// Set when moving to [`Lifecycle::ActingHost`].
    pub acting_host: Option<Identity>,
}

/// Selects which events to follow.
#[derive(Debug, Clone, Default)]
pub struct EventQuery {
    /// Resume past this sequence number.
    ///
    /// Zero starts at the present, which is right for a dashboard and wrong for anything
    /// that must not miss an event. Persist [`Event::sequence`] and pass it here, and a
    /// crash you did not plan for resumes correctly.
    pub after: u64,

    /// Filter to these kinds. Empty for all of them.
    pub kinds: Vec<EventKind>,
}

/// An open subscription to a room's events.
pub struct EventStream {
    inner: Streaming<proto::RoomEvent>,
}

impl std::fmt::Debug for EventStream {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("EventStream")
    }
}

impl EventStream {
    /// The next event, or `None` when the server closed the stream.
    ///
    /// A clean close happens on a deploy as well as at the end of a room. Reconnect from
    /// the last sequence you handled rather than from the present, or the events during
    /// the restart are lost silently.
    ///
    /// # Errors
    ///
    /// Fails if the stream does.
    pub async fn next(&mut self) -> Result<Option<Event>> {
        match self.inner.message().await {
            Ok(Some(wire)) => Ok(Some(event_in(wire))),
            Ok(None) => Ok(None),
            Err(status) => Err(Error::from_status("events.next", &status)),
        }
    }
}

impl Room<'_> {
    /// Follow this room.
    ///
    /// The stream is live by the time this returns: the control plane sends headers once
    /// its subscription exists, and this waits for them. That matters for a caller that
    /// subscribes and then does something it expects to see -- without the wait, the action
    /// could be published before anybody was listening.
    ///
    /// # Errors
    ///
    /// Fails if the stream cannot be opened.
    pub async fn events(&self, query: EventQuery) -> Result<EventStream> {
        open(self.client, &self.name, query).await
    }
}

impl Client {
    /// Follow every room in the tenant, which is what a moderation queue wants and what a
    /// per-room integration should not use.
    ///
    /// # Errors
    ///
    /// [`Error::Request`] if a cursor is given: sequence numbers are per room and there is
    /// no tenant-wide ordering to resume from. A consumer that needs resumption follows the
    /// rooms it cares about.
    pub async fn events(&self, query: EventQuery) -> Result<EventStream> {
        if query.after != 0 {
            return Err(Error::request(
                "moqom: a tenant-wide stream cannot resume; sequence numbers are per room",
            ));
        }
        open(self, "", query).await
    }
}

async fn open(client: &Client, room: &str, query: EventQuery) -> Result<EventStream> {
    let wire = proto::StreamRoomEventsRequest {
        tenant: client.tenant.clone(),
        room: room.to_owned(),
        after_sequence: query.after,
        kind: query.kinds.iter().copied().filter_map(kind_out).collect(),
    };

    // Not retried. A stream that failed to open may have delivered events to a listener
    // that no longer exists, and retrying silently would hide a gap; the caller knows its
    // cursor and can reopen from it.
    let stream = client
        .events
        .clone()
        .stream_room_events(wire)
        .await
        .map_err(|status| Error::from_status("events", &status))?;

    Ok(EventStream {
        inner: stream.into_inner(),
    })
}

fn kind_out(kind: EventKind) -> Option<i32> {
    let wire = match kind {
        EventKind::ParticipantJoined => proto::RoomEventKind::ParticipantJoined,
        EventKind::ParticipantLeft => proto::RoomEventKind::ParticipantLeft,
        EventKind::PresenceChanged => proto::RoomEventKind::PresenceChanged,
        EventKind::GrantsChanged => proto::RoomEventKind::GrantsChanged,
        EventKind::MuteChanged => proto::RoomEventKind::MuteChanged,
        EventKind::SpotlightChanged => proto::RoomEventKind::SpotlightChanged,
        EventKind::PublishStarted => proto::RoomEventKind::PublishStarted,
        EventKind::PublishStopped => proto::RoomEventKind::PublishStopped,
        EventKind::WatchdogChanged => proto::RoomEventKind::WatchdogChanged,
        EventKind::LifecycleChanged => proto::RoomEventKind::LifecycleChanged,
        EventKind::ModerationApplied => proto::RoomEventKind::ModerationApplied,
        // Filtering on a kind this build cannot name is not expressible, and sending
        // "unspecified" would mean "no filter" -- which would widen the subscription
        // rather than narrow it.
        EventKind::Unknown => return None,
    };
    Some(wire as i32)
}

fn kind_in(value: i32) -> EventKind {
    match proto::RoomEventKind::try_from(value) {
        Ok(proto::RoomEventKind::ParticipantJoined) => EventKind::ParticipantJoined,
        Ok(proto::RoomEventKind::ParticipantLeft) => EventKind::ParticipantLeft,
        Ok(proto::RoomEventKind::PresenceChanged) => EventKind::PresenceChanged,
        Ok(proto::RoomEventKind::GrantsChanged) => EventKind::GrantsChanged,
        Ok(proto::RoomEventKind::MuteChanged) => EventKind::MuteChanged,
        Ok(proto::RoomEventKind::SpotlightChanged) => EventKind::SpotlightChanged,
        Ok(proto::RoomEventKind::PublishStarted) => EventKind::PublishStarted,
        Ok(proto::RoomEventKind::PublishStopped) => EventKind::PublishStopped,
        Ok(proto::RoomEventKind::WatchdogChanged) => EventKind::WatchdogChanged,
        Ok(proto::RoomEventKind::LifecycleChanged) => EventKind::LifecycleChanged,
        Ok(proto::RoomEventKind::ModerationApplied) => EventKind::ModerationApplied,
        _ => EventKind::Unknown,
    }
}

fn event_in(wire: proto::RoomEvent) -> Event {
    let body = match wire.body {
        Some(proto::room_event::Body::ParticipantJoined(joined)) => {
            Body::Joined(ParticipantJoined {
                participant: joined.participant.map_or_else(
                    || participant_in(proto::Participant::default()),
                    participant_in,
                ),
                first_device: joined.first_device,
            })
        }
        Some(proto::room_event::Body::ParticipantLeft(left)) => Body::Left(ParticipantLeft {
            identity: identity_in(left.identity),
            was_removed: left.was_removed,
            code: code_in(left.code),
            last_device: left.last_device,
        }),
        Some(proto::room_event::Body::PresenceChanged(changed)) => {
            Body::Presence(PresenceChanged {
                identity: identity_in(changed.identity),
                from: presence_in(changed.from),
                to: presence_in(changed.to),
            })
        }
        Some(proto::room_event::Body::GrantsChanged(changed)) => {
            Body::GrantsChanged(GrantsChanged {
                identity: identity_in(changed.identity),
                from: grants_in(changed.from),
                to: grants_in(changed.to),
            })
        }
        Some(proto::room_event::Body::MuteChanged(changed)) => Body::Mute(MuteChanged {
            identity: identity_in(changed.identity),
            self_muted: media_in(&changed.self_muted),
            forced_muted_for_everyone: media_in(&changed.forced_muted_for_everyone),
            forced_muted_for_audience: media_in(&changed.forced_muted_for_audience),
        }),
        Some(proto::room_event::Body::SpotlightChanged(changed)) => {
            Body::Spotlight(SpotlightChanged {
                identity: changed.identity.map(|identity| identity_in(Some(identity))),
            })
        }
        Some(proto::room_event::Body::PublishChanged(changed)) => Body::Publish(PublishChanged {
            identity: identity_in(changed.identity),
            track: changed.track,
            started: changed.started,
        }),
        Some(proto::room_event::Body::WatchdogChanged(changed)) => {
            Body::Watchdog(WatchdogChanged {
                watchdog: changed
                    .watchdog
                    .map_or_else(|| watchdog_in(proto::Watchdog::default()), watchdog_in),
                detached: changed.detached,
            })
        }
        Some(proto::room_event::Body::LifecycleChanged(changed)) => {
            Body::Lifecycle(LifecycleChanged {
                from: lifecycle_in(changed.from),
                to: lifecycle_in(changed.to),
                acting_host: changed.acting_host.map(|host| identity_in(Some(host))),
            })
        }
        Some(proto::room_event::Body::ModerationApplied(entry)) => {
            Body::Moderation(entry_in(entry))
        }
        None => Body::Unknown,
    };

    Event {
        tenant: wire.tenant,
        room: wire.room,
        sequence: wire.sequence,
        at: time_in(wire.at),
        kind: kind_in(wire.kind),
        body,
    }
}
