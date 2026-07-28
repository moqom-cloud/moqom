//! Translation between this crate's types and the wire.
//!
//! The SDK does not expose protobuf types, and that costs this file. It buys two things
//! worth more: a caller composes capabilities as `Capability::KICK | Capability::FORCE_MUTE`
//! instead of building a vector of enum discriminants, and a field renumbered in the proto
//! is a change here rather than in every caller's code.
//!
//! Every mapping is total outward and lenient inward. An unmapped value going out is a bug
//! in this crate; one coming in is a newer control plane talking to an older SDK, and is
//! surfaced as an `Unknown` variant rather than guessed at.

use std::time::SystemTime;

use crate::grants::{Capability, Grants, Preset, Publish};
use crate::proto;
use crate::types::{
    AgeGate, BanScope, BanSubject, Code, Entry, Identity, Lifecycle, Media, MuteScope, Participant,
    ParticipantRef, Policy, Presence, RecordingPolicy, RoomState, Verb, Watchdog, WatchdogHealth,
    WatchdogKind, WatchdogOperator,
};

/// Capability bits paired with their wire values.
///
/// A table rather than arithmetic: the bit positions and the enum numbers are independent
/// by design, since the bits are protocol and the numbers are not.
const CAPABILITIES: &[(Capability, proto::Capability)] = &[
    (Capability::DELETE_MESSAGE, proto::Capability::DeleteMessage),
    (Capability::TIMEOUT_USER, proto::Capability::TimeoutUser),
    (Capability::BAN_FROM_CHAT, proto::Capability::BanFromChat),
    (Capability::SET_CHAT_MODE, proto::Capability::SetChatMode),
    (Capability::MANAGE_FILTERS, proto::Capability::ManageFilters),
    (
        Capability::PROMOTE_TO_GUEST,
        proto::Capability::PromoteToGuest,
    ),
    (
        Capability::PROMOTE_TO_COHOST,
        proto::Capability::PromoteToCohost,
    ),
    (Capability::DEMOTE, proto::Capability::Demote),
    (Capability::FORCE_MUTE, proto::Capability::ForceMute),
    (Capability::SPOTLIGHT, proto::Capability::Spotlight),
    (Capability::MANAGE_QUEUE, proto::Capability::ManageQueue),
    (Capability::LOCK_STAGE, proto::Capability::LockStage),
    (Capability::KICK, proto::Capability::Kick),
    (Capability::BAN_FROM_ROOM, proto::Capability::BanFromRoom),
    (
        Capability::BAN_FROM_TENANT,
        proto::Capability::BanFromTenant,
    ),
    (Capability::CLOSE_ROOM, proto::Capability::CloseRoom),
    (
        Capability::CHANGE_ROOM_POLICY,
        proto::Capability::ChangeRoomPolicy,
    ),
    (
        Capability::MANAGE_WATCHDOGS,
        proto::Capability::ManageWatchdogs,
    ),
    (
        Capability::APPOINT_MODERATORS,
        proto::Capability::AppointModerators,
    ),
    (
        Capability::READ_MODERATION_LOG,
        proto::Capability::ReadModerationLog,
    ),
    (Capability::HANDLE_APPEALS, proto::Capability::HandleAppeals),
];

const SCOPES: &[(Publish, proto::PublishScope)] = &[
    (Publish::AUDIO, proto::PublishScope::Audio),
    (Publish::VIDEO, proto::PublishScope::Video),
    (Publish::SCREEN_SHARE, proto::PublishScope::ScreenShare),
    (Publish::DATA, proto::PublishScope::Data),
];

const VERBS: &[(Verb, proto::ModerationVerb)] = &[
    (Verb::Kick, proto::ModerationVerb::Kick),
    (Verb::Ban, proto::ModerationVerb::Ban),
    (Verb::Unban, proto::ModerationVerb::Unban),
    (Verb::ForceMute, proto::ModerationVerb::ForceMute),
    (Verb::Unmute, proto::ModerationVerb::Unmute),
    (Verb::SetGrants, proto::ModerationVerb::SetGrants),
    (Verb::Spotlight, proto::ModerationVerb::Spotlight),
    (Verb::CloseRoom, proto::ModerationVerb::CloseRoom),
    (Verb::AttachWatchdog, proto::ModerationVerb::AttachWatchdog),
    (Verb::DetachWatchdog, proto::ModerationVerb::DetachWatchdog),
    (
        Verb::UpdateRoomPolicy,
        proto::ModerationVerb::UpdateRoomPolicy,
    ),
];

pub(crate) fn grants_out(grants: Grants) -> proto::Grants {
    proto::Grants {
        publish: SCOPES
            .iter()
            .filter(|(scope, _)| grants.publish.contains(*scope))
            .map(|(_, wire)| *wire as i32)
            .collect(),
        moderation: CAPABILITIES
            .iter()
            .filter(|(capability, _)| grants.moderation.contains(*capability))
            .map(|(_, wire)| *wire as i32)
            .collect(),
        is_owner: grants.is_owner,
    }
}

pub(crate) fn grants_in(wire: Option<proto::Grants>) -> Grants {
    let Some(wire) = wire else {
        return Grants::default();
    };

    let mut grants = Grants {
        is_owner: wire.is_owner,
        ..Grants::default()
    };
    for value in wire.moderation {
        if let Some((capability, _)) = CAPABILITIES.iter().find(|(_, w)| *w as i32 == value) {
            grants.moderation |= *capability;
        }
    }
    for value in wire.publish {
        if let Some((scope, _)) = SCOPES.iter().find(|(_, w)| *w as i32 == value) {
            grants.publish |= *scope;
        }
    }
    grants
}

pub(crate) fn preset_out(preset: Option<Preset>) -> i32 {
    let value = match preset {
        None => proto::CapabilityPreset::Unspecified,
        Some(Preset::None) => proto::CapabilityPreset::None,
        Some(Preset::Moderator) => proto::CapabilityPreset::Moderator,
        Some(Preset::SuperModerator) => proto::CapabilityPreset::SuperModerator,
        Some(Preset::Owner) => proto::CapabilityPreset::Owner,
    };
    value as i32
}

pub(crate) fn media_out(media: &[Media]) -> Vec<i32> {
    media
        .iter()
        .map(|kind| {
            let wire = match kind {
                Media::Audio => proto::MediaKind::Audio,
                Media::Video => proto::MediaKind::Video,
                Media::ScreenShare => proto::MediaKind::ScreenShare,
            };
            wire as i32
        })
        .collect()
}

pub(crate) fn media_in(wire: &[i32]) -> Vec<Media> {
    wire.iter()
        .filter_map(|value| match proto::MediaKind::try_from(*value) {
            Ok(proto::MediaKind::Audio) => Some(Media::Audio),
            Ok(proto::MediaKind::Video) => Some(Media::Video),
            Ok(proto::MediaKind::ScreenShare) => Some(Media::ScreenShare),
            _ => None,
        })
        .collect()
}

pub(crate) fn presence_in(value: i32) -> Presence {
    match proto::Presence::try_from(value) {
        Ok(proto::Presence::Connected) => Presence::Connected,
        Ok(proto::Presence::Reconnecting) => Presence::Reconnecting,
        Ok(proto::Presence::Away) => Presence::Away,
        Ok(proto::Presence::Backgrounded) => Presence::Backgrounded,
        Ok(proto::Presence::Left) => Presence::Left,
        _ => Presence::Unknown,
    }
}

pub(crate) fn lifecycle_out(lifecycle: Lifecycle) -> i32 {
    let wire = match lifecycle {
        Lifecycle::Scheduled => proto::RoomLifecycle::Scheduled,
        Lifecycle::Live => proto::RoomLifecycle::Live,
        Lifecycle::ActingHost => proto::RoomLifecycle::ActingHost,
        Lifecycle::Ended => proto::RoomLifecycle::Ended,
        Lifecycle::Unknown => proto::RoomLifecycle::Unspecified,
    };
    wire as i32
}

pub(crate) fn lifecycle_in(value: i32) -> Lifecycle {
    match proto::RoomLifecycle::try_from(value) {
        Ok(proto::RoomLifecycle::Scheduled) => Lifecycle::Scheduled,
        Ok(proto::RoomLifecycle::Live) => Lifecycle::Live,
        Ok(proto::RoomLifecycle::ActingHost) => Lifecycle::ActingHost,
        Ok(proto::RoomLifecycle::Ended) => Lifecycle::Ended,
        _ => Lifecycle::Unknown,
    }
}

pub(crate) fn recording_out(policy: RecordingPolicy) -> i32 {
    let wire = match policy {
        RecordingPolicy::Unset => proto::RecordingPolicy::Unspecified,
        RecordingPolicy::Allowed => proto::RecordingPolicy::Allowed,
        RecordingPolicy::Required => proto::RecordingPolicy::Required,
        RecordingPolicy::Prohibited => proto::RecordingPolicy::Prohibited,
    };
    wire as i32
}

pub(crate) fn recording_in(value: i32) -> RecordingPolicy {
    match proto::RecordingPolicy::try_from(value) {
        Ok(proto::RecordingPolicy::Allowed) => RecordingPolicy::Allowed,
        Ok(proto::RecordingPolicy::Required) => RecordingPolicy::Required,
        Ok(proto::RecordingPolicy::Prohibited) => RecordingPolicy::Prohibited,
        _ => RecordingPolicy::Unset,
    }
}

pub(crate) fn code_out(code: Code) -> i32 {
    let wire = match code {
        Code::Unset => proto::ModerationCode::Unspecified,
        Code::AdultContent => proto::ModerationCode::AdultContent,
        Code::Violence => proto::ModerationCode::Violence,
        Code::Harassment => proto::ModerationCode::Harassment,
        Code::SelfHarm => proto::ModerationCode::SelfHarm,
        Code::Copyright => proto::ModerationCode::Copyright,
        Code::AgeVerificationRequired => proto::ModerationCode::AgeVerificationRequired,
        Code::TenantPolicy => proto::ModerationCode::TenantPolicy,
        Code::OperatorAction => proto::ModerationCode::OperatorAction,
    };
    wire as i32
}

pub(crate) fn code_in(value: i32) -> Code {
    match proto::ModerationCode::try_from(value) {
        Ok(proto::ModerationCode::AdultContent) => Code::AdultContent,
        Ok(proto::ModerationCode::Violence) => Code::Violence,
        Ok(proto::ModerationCode::Harassment) => Code::Harassment,
        Ok(proto::ModerationCode::SelfHarm) => Code::SelfHarm,
        Ok(proto::ModerationCode::Copyright) => Code::Copyright,
        Ok(proto::ModerationCode::AgeVerificationRequired) => Code::AgeVerificationRequired,
        Ok(proto::ModerationCode::TenantPolicy) => Code::TenantPolicy,
        Ok(proto::ModerationCode::OperatorAction) => Code::OperatorAction,
        _ => Code::Unset,
    }
}

pub(crate) fn verb_out(verb: Verb) -> i32 {
    let wire = VERBS
        .iter()
        .find(|(candidate, _)| *candidate == verb)
        .map_or(proto::ModerationVerb::Unspecified, |(_, wire)| *wire);
    wire as i32
}

pub(crate) fn verb_in(value: i32) -> Verb {
    VERBS
        .iter()
        .find(|(_, wire)| *wire as i32 == value)
        .map_or(Verb::Unknown, |(verb, _)| *verb)
}

pub(crate) fn ban_scope_out(scope: BanScope) -> i32 {
    let wire = match scope {
        BanScope::Unset => proto::BanScope::Unspecified,
        BanScope::Room => proto::BanScope::Room,
        BanScope::Tenant => proto::BanScope::Tenant,
    };
    wire as i32
}

pub(crate) fn ban_scope_in(value: i32) -> BanScope {
    match proto::BanScope::try_from(value) {
        Ok(proto::BanScope::Room) => BanScope::Room,
        Ok(proto::BanScope::Tenant) => BanScope::Tenant,
        _ => BanScope::Unset,
    }
}

pub(crate) fn mute_scope_out(scope: MuteScope) -> i32 {
    let wire = match scope {
        MuteScope::Unset => proto::MuteScope::Unspecified,
        MuteScope::Everyone => proto::MuteScope::Everyone,
        MuteScope::Audience => proto::MuteScope::Audience,
    };
    wire as i32
}

pub(crate) fn mute_scope_in(value: i32) -> MuteScope {
    match proto::MuteScope::try_from(value) {
        Ok(proto::MuteScope::Everyone) => MuteScope::Everyone,
        Ok(proto::MuteScope::Audience) => MuteScope::Audience,
        _ => MuteScope::Unset,
    }
}

pub(crate) fn watchdog_kind_out(kind: &WatchdogKind) -> i32 {
    let wire = match kind {
        WatchdogKind::Unspecified => proto::WatchdogKind::Unspecified,
        WatchdogKind::Recording => proto::WatchdogKind::Recording,
        WatchdogKind::Safety => proto::WatchdogKind::Safety,
        WatchdogKind::Analytics => proto::WatchdogKind::Analytics,
        WatchdogKind::Compliance => proto::WatchdogKind::Compliance,
        WatchdogKind::Custom => proto::WatchdogKind::Custom,
    };
    wire as i32
}

pub(crate) fn watchdog_kind_in(value: i32) -> WatchdogKind {
    match proto::WatchdogKind::try_from(value) {
        Ok(proto::WatchdogKind::Recording) => WatchdogKind::Recording,
        Ok(proto::WatchdogKind::Safety) => WatchdogKind::Safety,
        Ok(proto::WatchdogKind::Analytics) => WatchdogKind::Analytics,
        Ok(proto::WatchdogKind::Compliance) => WatchdogKind::Compliance,
        Ok(proto::WatchdogKind::Custom) => WatchdogKind::Custom,
        _ => WatchdogKind::Unspecified,
    }
}

pub(crate) fn operator_out(operator: WatchdogOperator) -> i32 {
    let wire = match operator {
        WatchdogOperator::Unspecified => proto::WatchdogOperator::Unspecified,
        WatchdogOperator::Platform => proto::WatchdogOperator::Platform,
        WatchdogOperator::Tenant => proto::WatchdogOperator::Tenant,
        WatchdogOperator::ThirdParty => proto::WatchdogOperator::ThirdParty,
    };
    wire as i32
}

pub(crate) fn operator_in(value: i32) -> WatchdogOperator {
    match proto::WatchdogOperator::try_from(value) {
        Ok(proto::WatchdogOperator::Platform) => WatchdogOperator::Platform,
        Ok(proto::WatchdogOperator::Tenant) => WatchdogOperator::Tenant,
        Ok(proto::WatchdogOperator::ThirdParty) => WatchdogOperator::ThirdParty,
        _ => WatchdogOperator::Unspecified,
    }
}

pub(crate) fn health_in(value: i32) -> WatchdogHealth {
    match proto::WatchdogHealth::try_from(value) {
        Ok(proto::WatchdogHealth::Attached) => WatchdogHealth::Attached,
        Ok(proto::WatchdogHealth::Degraded) => WatchdogHealth::Degraded,
        Ok(proto::WatchdogHealth::Failed) => WatchdogHealth::Failed,
        _ => WatchdogHealth::Unspecified,
    }
}

pub(crate) fn identity_out(identity: &Identity) -> proto::Identity {
    proto::Identity {
        username: identity.username.clone(),
        device: identity.device.clone(),
    }
}

pub(crate) fn identity_in(wire: Option<proto::Identity>) -> Identity {
    wire.map(|wire| Identity {
        username: wire.username,
        device: wire.device,
    })
    .unwrap_or_default()
}

pub(crate) fn ref_out(target: &ParticipantRef) -> proto::ParticipantRef {
    proto::ParticipantRef {
        username: target.username.clone(),
        device: target.device.clone(),
    }
}

pub(crate) fn ref_in(wire: Option<proto::ParticipantRef>) -> Option<ParticipantRef> {
    wire.map(|wire| ParticipantRef {
        username: wire.username,
        device: wire.device,
    })
}

pub(crate) fn subject_out(subject: &BanSubject) -> proto::BanSubject {
    let key = match subject {
        BanSubject::Account(username) => proto::ban_subject::Key::Username(username.clone()),
        BanSubject::Device(identity) => proto::ban_subject::Key::Device(identity_out(identity)),
        BanSubject::Address(prefix) => proto::ban_subject::Key::IpPrefix(prefix.clone()),
    };
    proto::BanSubject { key: Some(key) }
}

pub(crate) fn subject_in(wire: Option<proto::BanSubject>) -> Option<BanSubject> {
    match wire?.key? {
        proto::ban_subject::Key::Username(username) => Some(BanSubject::Account(username)),
        proto::ban_subject::Key::Device(identity) => {
            Some(BanSubject::Device(identity_in(Some(identity))))
        }
        proto::ban_subject::Key::IpPrefix(prefix) => Some(BanSubject::Address(prefix)),
    }
}

pub(crate) fn policy_out(policy: &Policy) -> proto::RoomPolicy {
    proto::RoomPolicy {
        age_gate: Some(proto::AgeGate {
            minimum_age: policy.age_gate.minimum_age,
            require_verification: policy.age_gate.require_verification,
        }),
        recording: recording_out(policy.recording),
        residency_zone: policy.residency_zone.clone(),
        max_participants: policy.max_participants,
        stage_slots: policy.stage_slots,
        watchdog_limit: policy.watchdog_limit,
    }
}

pub(crate) fn policy_in(wire: Option<proto::RoomPolicy>) -> Policy {
    let Some(wire) = wire else {
        return Policy::default();
    };
    let gate = wire.age_gate.unwrap_or_default();
    Policy {
        age_gate: AgeGate {
            minimum_age: gate.minimum_age,
            require_verification: gate.require_verification,
        },
        recording: recording_in(wire.recording),
        residency_zone: wire.residency_zone,
        max_participants: wire.max_participants,
        stage_slots: wire.stage_slots,
        watchdog_limit: wire.watchdog_limit,
    }
}

pub(crate) fn room_in(wire: proto::Room) -> RoomState {
    RoomState {
        tenant: wire.tenant,
        name: wire.room,
        namespace: wire.namespace,
        policy: policy_in(wire.policy),
        lifecycle: lifecycle_in(wire.lifecycle),
        starts_at: time_in(wire.starts_at),
        created_at: time_in(wire.created_at),
        ended_at: time_in(wire.ended_at),
        acting_host: wire.acting_host.map(|host| identity_in(Some(host))),
        participant_count: wire.participant_count,
        distinct_user_count: wire.distinct_user_count,
        is_recording: wire.is_recording,
    }
}

pub(crate) fn participant_in(wire: proto::Participant) -> Participant {
    Participant {
        identity: identity_in(wire.identity),
        grants: grants_in(wire.grants),
        presence: presence_in(wire.presence),
        self_muted: media_in(&wire.self_muted),
        forced_muted_for_everyone: media_in(&wire.forced_muted_for_everyone),
        forced_muted_for_audience: media_in(&wire.forced_muted_for_audience),
        is_spotlighted: wire.is_spotlighted,
        is_backstage: wire.is_backstage,
        joined_at: time_in(wire.joined_at),
        relay_id: wire.relay_id,
    }
}

pub(crate) fn watchdog_in(wire: proto::Watchdog) -> Watchdog {
    Watchdog {
        id: wire.id,
        kind: watchdog_kind_in(wire.kind),
        custom_kind: wire.custom_kind,
        operated_by: operator_in(wire.operated_by),
        operator_name: wire.operator_name,
        descriptor: wire.descriptor,
        scope: wire.scope,
        health: health_in(wire.health),
        attached_at: time_in(wire.attached_at),
        is_billable: wire.is_billable,
        pinned_layer: wire.pinned_layer,
    }
}

pub(crate) fn entry_in(wire: proto::ModerationEntry) -> Entry {
    Entry {
        id: wire.id,
        tenant: wire.tenant,
        room: wire.room,
        at: time_in(wire.at),
        verb: verb_in(wire.verb),
        service_account: wire.service_account,
        on_behalf_of: wire.on_behalf_of,
        target: ref_in(wire.target),
        ban_subject: subject_in(wire.ban_subject),
        code: code_in(wire.code),
        reason: wire.reason,
        expires_at: time_in(wire.expires_at),
        media: media_in(&wire.media),
        mute_scope: mute_scope_in(wire.mute_scope),
        ban_scope: ban_scope_in(wire.ban_scope),
        grants: grants_in(wire.grants),
        enforced: wire.enforced,
        decided_by_model: wire.decided_by_model,
    }
}

/// An absent timestamp becomes `None` rather than the epoch.
///
/// The distinction matters: an unset expiry means "permanent", and rendering that as 1970
/// would make every permanent ban look expired.
pub(crate) fn time_in(wire: Option<prost_types::Timestamp>) -> Option<SystemTime> {
    let wire = wire?;
    let seconds = u64::try_from(wire.seconds).ok()?;
    let nanos = u32::try_from(wire.nanos).ok()?;
    Some(SystemTime::UNIX_EPOCH + std::time::Duration::new(seconds, nanos))
}

pub(crate) fn time_out(at: Option<SystemTime>) -> Option<prost_types::Timestamp> {
    let since = at?.duration_since(SystemTime::UNIX_EPOCH).ok()?;
    Some(prost_types::Timestamp {
        seconds: i64::try_from(since.as_secs()).ok()?,
        nanos: i32::try_from(since.subsec_nanos()).ok()?,
    })
}
