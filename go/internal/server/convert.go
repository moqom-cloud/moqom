package server

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/timecode"
)

// Translation between the wire and the domain.
//
// A layer that could be skipped by using the generated types throughout, and is
// not, for two reasons. Protobuf enums default to UNSPECIFIED, so a domain that
// used them would have an unspecified state in every field and a check for it at
// every use; and the capability bitmask that goes into a token is not the repeated
// enum the API exposes, so something has to convert -- once, here, rather than at
// each call site.

var capabilityByProto = map[adminpb.Capability]control.Capability{
	adminpb.Capability_CAPABILITY_DELETE_MESSAGE:      control.CapDeleteMessage,
	adminpb.Capability_CAPABILITY_TIMEOUT_USER:        control.CapTimeoutUser,
	adminpb.Capability_CAPABILITY_BAN_FROM_CHAT:       control.CapBanFromChat,
	adminpb.Capability_CAPABILITY_SET_CHAT_MODE:       control.CapSetChatMode,
	adminpb.Capability_CAPABILITY_MANAGE_FILTERS:      control.CapManageFilters,
	adminpb.Capability_CAPABILITY_PROMOTE_TO_GUEST:    control.CapPromoteToGuest,
	adminpb.Capability_CAPABILITY_PROMOTE_TO_COHOST:   control.CapPromoteToCohost,
	adminpb.Capability_CAPABILITY_DEMOTE:              control.CapDemote,
	adminpb.Capability_CAPABILITY_FORCE_MUTE:          control.CapForceMute,
	adminpb.Capability_CAPABILITY_SPOTLIGHT:           control.CapSpotlight,
	adminpb.Capability_CAPABILITY_MANAGE_QUEUE:        control.CapManageQueue,
	adminpb.Capability_CAPABILITY_LOCK_STAGE:          control.CapLockStage,
	adminpb.Capability_CAPABILITY_KICK:                control.CapKick,
	adminpb.Capability_CAPABILITY_BAN_FROM_ROOM:       control.CapBanFromRoom,
	adminpb.Capability_CAPABILITY_BAN_FROM_TENANT:     control.CapBanFromTenant,
	adminpb.Capability_CAPABILITY_CLOSE_ROOM:          control.CapCloseRoom,
	adminpb.Capability_CAPABILITY_CHANGE_ROOM_POLICY:  control.CapChangeRoomPolicy,
	adminpb.Capability_CAPABILITY_MANAGE_WATCHDOGS:    control.CapManageWatchdogs,
	adminpb.Capability_CAPABILITY_APPOINT_MODERATORS:  control.CapAppointModerators,
	adminpb.Capability_CAPABILITY_READ_MODERATION_LOG: control.CapReadModerationLog,
	adminpb.Capability_CAPABILITY_HANDLE_APPEALS:      control.CapHandleAppeals,
}

var protoByCapability = func() map[control.Capability]adminpb.Capability {
	inverted := make(map[control.Capability]adminpb.Capability, len(capabilityByProto))
	for wire, domain := range capabilityByProto {
		inverted[domain] = wire
	}
	return inverted
}()

var publishByProto = map[adminpb.PublishScope]control.PublishScope{
	adminpb.PublishScope_PUBLISH_SCOPE_AUDIO:        control.PublishAudio,
	adminpb.PublishScope_PUBLISH_SCOPE_VIDEO:        control.PublishVideo,
	adminpb.PublishScope_PUBLISH_SCOPE_SCREEN_SHARE: control.PublishScreenShare,
	adminpb.PublishScope_PUBLISH_SCOPE_DATA:         control.PublishData,
}

var mediaByProto = map[adminpb.MediaKind]control.MediaKind{
	adminpb.MediaKind_MEDIA_KIND_AUDIO:        control.MediaAudio,
	adminpb.MediaKind_MEDIA_KIND_VIDEO:        control.MediaVideo,
	adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE: control.MediaScreenShare,
}

var presetByProto = map[adminpb.CapabilityPreset]control.Preset{
	adminpb.CapabilityPreset_CAPABILITY_PRESET_NONE:            control.PresetNone,
	adminpb.CapabilityPreset_CAPABILITY_PRESET_MODERATOR:       control.PresetModerator,
	adminpb.CapabilityPreset_CAPABILITY_PRESET_SUPER_MODERATOR: control.PresetSuperModerator,
	adminpb.CapabilityPreset_CAPABILITY_PRESET_OWNER:           control.PresetOwner,
}

// grantsFromProto reads grants, applying a preset if one is named.
//
// An unrecognised capability is refused rather than ignored: a caller that names
// something this build does not implement is asking for a power, and silently
// granting less than was asked for is how an escalation goes unnoticed in the other
// direction too.
func grantsFromProto(wire *adminpb.Grants, preset adminpb.CapabilityPreset) (control.Grants, error) {
	var grants control.Grants
	if wire != nil {
		for _, scope := range wire.GetPublish() {
			mapped, ok := publishByProto[scope]
			if !ok {
				return control.Grants{}, fmt.Errorf("%w: unknown publish scope %v", control.ErrInvalid, scope)
			}
			grants.Publish |= mapped
		}
		for _, capability := range wire.GetModeration() {
			mapped, ok := capabilityByProto[capability]
			if !ok {
				return control.Grants{}, fmt.Errorf("%w: unknown capability %v", control.ErrInvalid, capability)
			}
			grants.Moderation |= mapped
		}
		grants.IsOwner = wire.GetIsOwner()
	}
	if preset != adminpb.CapabilityPreset_CAPABILITY_PRESET_UNSPECIFIED {
		named, ok := presetByProto[preset]
		if !ok {
			return control.Grants{}, fmt.Errorf("%w: unknown preset %v", control.ErrInvalid, preset)
		}
		expanded, ok := named.Expand()
		if !ok {
			return control.Grants{}, fmt.Errorf("%w: preset %s does not expand", control.ErrInvalid, named)
		}
		// The preset replaces the moderation half and leaves publishing alone: a
		// preset says nothing about what someone may send, and inferring it would
		// make "make them a moderator" silently mute them.
		grants.Moderation = expanded
	}
	return grants, nil
}

func grantsToProto(grants control.Grants) *adminpb.Grants {
	wire := &adminpb.Grants{IsOwner: grants.IsOwner}
	// Ordered by the enum rather than by iteration over a map, so two equal grant
	// sets serialise identically and a diff between two reads means a difference.
	for _, scope := range []adminpb.PublishScope{
		adminpb.PublishScope_PUBLISH_SCOPE_AUDIO,
		adminpb.PublishScope_PUBLISH_SCOPE_VIDEO,
		adminpb.PublishScope_PUBLISH_SCOPE_SCREEN_SHARE,
		adminpb.PublishScope_PUBLISH_SCOPE_DATA,
	} {
		if grants.Publish.Has(publishByProto[scope]) {
			wire.Publish = append(wire.Publish, scope)
		}
	}
	for _, entry := range orderedCapabilities {
		if grants.Moderation.Has(entry) {
			wire.Moderation = append(wire.Moderation, protoByCapability[entry])
		}
	}
	return wire
}

var orderedCapabilities = []control.Capability{
	control.CapDeleteMessage, control.CapTimeoutUser, control.CapBanFromChat,
	control.CapSetChatMode, control.CapManageFilters,
	control.CapPromoteToGuest, control.CapPromoteToCohost, control.CapDemote,
	control.CapForceMute, control.CapSpotlight, control.CapManageQueue, control.CapLockStage,
	control.CapKick, control.CapBanFromRoom, control.CapBanFromTenant,
	control.CapCloseRoom, control.CapChangeRoomPolicy, control.CapManageWatchdogs,
	control.CapAppointModerators, control.CapReadModerationLog, control.CapHandleAppeals,
}

func mediaFromProto(kinds []adminpb.MediaKind) (control.MediaKind, error) {
	if len(kinds) == 0 {
		// Empty means all of it. A force-mute with no media named is "stop
		// broadcasting", which is what a moderator dealing with an emergency means.
		return control.MediaAll, nil
	}
	var media control.MediaKind
	for _, kind := range kinds {
		mapped, ok := mediaByProto[kind]
		if !ok {
			return 0, fmt.Errorf("%w: unknown media kind %v", control.ErrInvalid, kind)
		}
		media |= mapped
	}
	return media, nil
}

func mediaToProto(media control.MediaKind) []adminpb.MediaKind {
	var kinds []adminpb.MediaKind
	for _, kind := range []adminpb.MediaKind{
		adminpb.MediaKind_MEDIA_KIND_AUDIO,
		adminpb.MediaKind_MEDIA_KIND_VIDEO,
		adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE,
	} {
		if media.Has(mediaByProto[kind]) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func identityFromProto(wire *adminpb.Identity) control.Identity {
	if wire == nil {
		return control.Identity{}
	}
	return control.Identity{Username: wire.GetUsername(), Device: wire.GetDevice()}
}

func identityToProto(id control.Identity) *adminpb.Identity {
	if id.IsZero() {
		return nil
	}
	return &adminpb.Identity{Username: id.Username, Device: id.Device}
}

// targetFromProto reads a moderation target.
//
// A reference with no device names every device that person is on, and the
// distinction is preserved rather than resolved here: resolving it to a list would
// mean a kick applies to the devices that were connected when the call arrived, and
// not to the one that reconnects a second later.
func targetFromProto(wire *adminpb.ParticipantRef) (control.Identity, error) {
	if wire == nil || wire.GetUsername() == "" {
		return control.Identity{}, fmt.Errorf("%w: target needs a username", control.ErrInvalid)
	}
	username, err := control.NormaliseUsername(wire.GetUsername())
	if err != nil {
		return control.Identity{}, err
	}
	return control.Identity{Username: username, Device: wire.GetDevice()}, nil
}

func targetToProto(id control.Identity) *adminpb.ParticipantRef {
	if id.Username == "" {
		return nil
	}
	return &adminpb.ParticipantRef{Username: id.Username, Device: id.Device}
}

var lifecycleByProto = map[adminpb.RoomLifecycle]control.Lifecycle{
	adminpb.RoomLifecycle_ROOM_LIFECYCLE_SCHEDULED:   control.LifecycleScheduled,
	adminpb.RoomLifecycle_ROOM_LIFECYCLE_LIVE:        control.LifecycleLive,
	adminpb.RoomLifecycle_ROOM_LIFECYCLE_ACTING_HOST: control.LifecycleActingHost,
	adminpb.RoomLifecycle_ROOM_LIFECYCLE_ENDED:       control.LifecycleEnded,
}

var protoByLifecycle = func() map[control.Lifecycle]adminpb.RoomLifecycle {
	inverted := map[control.Lifecycle]adminpb.RoomLifecycle{}
	for wire, domain := range lifecycleByProto {
		inverted[domain] = wire
	}
	return inverted
}()

var recordingByProto = map[adminpb.RecordingPolicy]control.RecordingPolicy{
	adminpb.RecordingPolicy_RECORDING_POLICY_ALLOWED:    control.RecordingAllowed,
	adminpb.RecordingPolicy_RECORDING_POLICY_REQUIRED:   control.RecordingRequired,
	adminpb.RecordingPolicy_RECORDING_POLICY_PROHIBITED: control.RecordingProhibited,
}

var protoByRecording = func() map[control.RecordingPolicy]adminpb.RecordingPolicy {
	inverted := map[control.RecordingPolicy]adminpb.RecordingPolicy{}
	for wire, domain := range recordingByProto {
		inverted[domain] = wire
	}
	return inverted
}()

func policyFromProto(wire *adminpb.RoomPolicy) (control.Policy, error) {
	if wire == nil {
		return control.Policy{}.WithDefaults(), nil
	}
	recording := control.RecordingAllowed
	if wire.GetRecording() != adminpb.RecordingPolicy_RECORDING_POLICY_UNSPECIFIED {
		mapped, ok := recordingByProto[wire.GetRecording()]
		if !ok {
			return control.Policy{}, fmt.Errorf("%w: unknown recording policy %v", control.ErrInvalid, wire.GetRecording())
		}
		recording = mapped
	}
	policy := control.Policy{
		AgeGate: control.AgeGate{
			MinimumAge:          wire.GetAgeGate().GetMinimumAge(),
			RequireVerification: wire.GetAgeGate().GetRequireVerification(),
		},
		Recording:       recording,
		ResidencyZone:   wire.GetResidencyZone(),
		MaxParticipants: wire.GetMaxParticipants(),
		StageSlots:      wire.GetStageSlots(),
		WatchdogLimit:   wire.GetWatchdogLimit(),
	}
	return policy.WithDefaults(), nil
}

func policyToProto(policy control.Policy) *adminpb.RoomPolicy {
	return &adminpb.RoomPolicy{
		AgeGate: &adminpb.AgeGate{
			MinimumAge:          policy.AgeGate.MinimumAge,
			RequireVerification: policy.AgeGate.RequireVerification,
		},
		Recording:       protoByRecording[policy.Recording],
		ResidencyZone:   policy.ResidencyZone,
		MaxParticipants: policy.MaxParticipants,
		StageSlots:      policy.StageSlots,
		WatchdogLimit:   policy.WatchdogLimit,
	}
}

func roomToProto(room control.Room, counts roomCounts) *adminpb.Room {
	return &adminpb.Room{
		Tenant:            room.Tenant,
		Room:              room.Room,
		Namespace:         room.Namespace().Rendered(),
		Policy:            policyToProto(room.Policy),
		Lifecycle:         protoByLifecycle[room.Lifecycle],
		StartsAt:          timeToProto(room.StartsAt),
		CreatedAt:         timeToProto(room.CreatedAt),
		EndedAt:           timeToProto(room.EndedAt),
		ActingHost:        identityToProto(room.ActingHost),
		ParticipantCount:  counts.devices,
		DistinctUserCount: counts.people,
		IsRecording:       counts.recording,
		Audiences:         audiencesToProto(counts.audiences),
	}
}

// roomCounts is what a room read reports about who is in it.
//
// Derived from participant records rather than stored on the room, so the numbers
// cannot drift from the roster they are meant to summarise.
type roomCounts struct {
	devices   uint32
	people    uint32
	recording bool
	audiences []control.Audience
}

func audiencesToProto(cohorts []control.Audience) []*adminpb.Audience {
	if len(cohorts) == 0 {
		return nil
	}
	converted := make([]*adminpb.Audience, 0, len(cohorts))
	for _, cohort := range cohorts {
		converted = append(converted, &adminpb.Audience{
			Via:        cohort.Via,
			Viewers:    int64(cohort.Viewers),
			Resolution: int64(cohort.Resolution),
		})
	}
	return converted
}

var presenceByDomain = map[control.Presence]adminpb.Presence{
	control.PresenceConnected:    adminpb.Presence_PRESENCE_CONNECTED,
	control.PresenceReconnecting: adminpb.Presence_PRESENCE_RECONNECTING,
	control.PresenceAway:         adminpb.Presence_PRESENCE_AWAY,
	control.PresenceBackgrounded: adminpb.Presence_PRESENCE_BACKGROUNDED,
	control.PresenceLeft:         adminpb.Presence_PRESENCE_LEFT,
}

func participantToProto(participant control.Participant) *adminpb.Participant {
	return &adminpb.Participant{
		Identity:               identityToProto(participant.Identity),
		Grants:                 grantsToProto(participant.Grants),
		Presence:               presenceByDomain[participant.Presence],
		SelfMuted:              mediaToProto(participant.Mute.SelfMuted),
		ForcedMutedForEveryone: mediaToProto(participant.Mute.ForcedForEveryone),
		ForcedMutedForAudience: mediaToProto(participant.Mute.ForcedForAudience),
		IsSpotlighted:          participant.Spotlighted,
		IsBackstage:            participant.Backstage,
		JoinedAt:               timeToProto(participant.JoinedAt),
		RelayId:                participant.RelayID,
		ArrivedVia:             participant.ArrivedVia,
		Locale:                 participant.Locale,
	}
}

var watchdogKindByProto = map[adminpb.WatchdogKind]control.WatchdogKind{
	adminpb.WatchdogKind_WATCHDOG_KIND_RECORDING:  control.WatchdogRecording,
	adminpb.WatchdogKind_WATCHDOG_KIND_SAFETY:     control.WatchdogSafety,
	adminpb.WatchdogKind_WATCHDOG_KIND_ANALYTICS:  control.WatchdogAnalytics,
	adminpb.WatchdogKind_WATCHDOG_KIND_COMPLIANCE: control.WatchdogCompliance,
	adminpb.WatchdogKind_WATCHDOG_KIND_CUSTOM:     control.WatchdogCustom,
}

var protoByWatchdogKind = func() map[control.WatchdogKind]adminpb.WatchdogKind {
	inverted := map[control.WatchdogKind]adminpb.WatchdogKind{}
	for wire, domain := range watchdogKindByProto {
		inverted[domain] = wire
	}
	return inverted
}()

var operatorByProto = map[adminpb.WatchdogOperator]control.WatchdogOperator{
	adminpb.WatchdogOperator_WATCHDOG_OPERATOR_PLATFORM:    control.OperatorPlatform,
	adminpb.WatchdogOperator_WATCHDOG_OPERATOR_TENANT:      control.OperatorTenant,
	adminpb.WatchdogOperator_WATCHDOG_OPERATOR_THIRD_PARTY: control.OperatorThirdParty,
}

var protoByOperator = func() map[control.WatchdogOperator]adminpb.WatchdogOperator {
	inverted := map[control.WatchdogOperator]adminpb.WatchdogOperator{}
	for wire, domain := range operatorByProto {
		inverted[domain] = wire
	}
	return inverted
}()

var healthByDomain = map[control.WatchdogHealth]adminpb.WatchdogHealth{
	control.WatchdogAttached: adminpb.WatchdogHealth_WATCHDOG_HEALTH_ATTACHED,
	control.WatchdogDegraded: adminpb.WatchdogHealth_WATCHDOG_HEALTH_DEGRADED,
	control.WatchdogFailed:   adminpb.WatchdogHealth_WATCHDOG_HEALTH_FAILED,
}

func watchdogToProto(watchdog control.Watchdog) *adminpb.Watchdog {
	return &adminpb.Watchdog{
		Id:           watchdog.ID,
		Kind:         protoByWatchdogKind[watchdog.Kind],
		CustomKind:   watchdog.CustomKind,
		OperatedBy:   protoByOperator[watchdog.OperatedBy],
		OperatorName: watchdog.OperatorName,
		Descriptor_:  watchdog.Descriptor,
		Scope:        watchdog.Scope.Rendered(),
		Health:       healthByDomain[watchdog.Health],
		AttachedAt:   timeToProto(watchdog.AttachedAt),
		IsBillable:   watchdog.Billable,
		PinnedLayer:  watchdog.PinnedLayer,
	}
}

var verbByProto = map[adminpb.ModerationVerb]control.ModerationVerb{
	adminpb.ModerationVerb_MODERATION_VERB_KICK:               control.VerbKick,
	adminpb.ModerationVerb_MODERATION_VERB_BAN:                control.VerbBan,
	adminpb.ModerationVerb_MODERATION_VERB_UNBAN:              control.VerbUnban,
	adminpb.ModerationVerb_MODERATION_VERB_FORCE_MUTE:         control.VerbForceMute,
	adminpb.ModerationVerb_MODERATION_VERB_UNMUTE:             control.VerbUnmute,
	adminpb.ModerationVerb_MODERATION_VERB_SET_GRANTS:         control.VerbSetGrants,
	adminpb.ModerationVerb_MODERATION_VERB_SPOTLIGHT:          control.VerbSpotlight,
	adminpb.ModerationVerb_MODERATION_VERB_CLOSE_ROOM:         control.VerbCloseRoom,
	adminpb.ModerationVerb_MODERATION_VERB_ATTACH_WATCHDOG:    control.VerbAttachWatchdog,
	adminpb.ModerationVerb_MODERATION_VERB_DETACH_WATCHDOG:    control.VerbDetachWatchdog,
	adminpb.ModerationVerb_MODERATION_VERB_UPDATE_ROOM_POLICY: control.VerbUpdateRoomPolicy,
}

var protoByVerb = func() map[control.ModerationVerb]adminpb.ModerationVerb {
	inverted := map[control.ModerationVerb]adminpb.ModerationVerb{}
	for wire, domain := range verbByProto {
		inverted[domain] = wire
	}
	return inverted
}()

var codeByProto = map[adminpb.ModerationCode]string{
	adminpb.ModerationCode_MODERATION_CODE_ADULT_CONTENT:             "adult_content",
	adminpb.ModerationCode_MODERATION_CODE_VIOLENCE:                  "violence",
	adminpb.ModerationCode_MODERATION_CODE_HARASSMENT:                "harassment",
	adminpb.ModerationCode_MODERATION_CODE_SELF_HARM:                 "self_harm",
	adminpb.ModerationCode_MODERATION_CODE_COPYRIGHT:                 "copyright",
	adminpb.ModerationCode_MODERATION_CODE_AGE_VERIFICATION_REQUIRED: "age_verification_required",
	adminpb.ModerationCode_MODERATION_CODE_TENANT_POLICY:             "tenant_policy",
	adminpb.ModerationCode_MODERATION_CODE_OPERATOR_ACTION:           "operator_action",
}

var protoByCode = func() map[string]adminpb.ModerationCode {
	inverted := map[string]adminpb.ModerationCode{}
	for wire, domain := range codeByProto {
		inverted[domain] = wire
	}
	return inverted
}()

func entryToProto(entry control.ModerationEntry) *adminpb.ModerationEntry {
	wire := &adminpb.ModerationEntry{
		Id:             entry.ID,
		Tenant:         entry.Tenant,
		Room:           entry.Room,
		At:             timeToProto(entry.At),
		Verb:           protoByVerb[entry.Verb],
		ServiceAccount: entry.ServiceAccount,
		OnBehalfOf:     entry.OnBehalfOf,
		Target:         targetToProto(entry.Target),
		Code:           protoByCode[entry.Code],
		Reason:         entry.Reason,
		ExpiresAt:      timeToProto(entry.ExpiresAt),
		Media:          mediaToProto(entry.Media),
		Enforced:       entry.Enforced,
		DecidedByModel: entry.DecidedByModel,
	}
	if entry.Grants != nil {
		wire.Grants = grantsToProto(*entry.Grants)
	}
	if entry.BanKind != "" {
		wire.BanSubject = banSubjectToProto(entry)
		if entry.BanScopeTenant {
			wire.BanScope = adminpb.BanScope_BAN_SCOPE_TENANT
		} else {
			wire.BanScope = adminpb.BanScope_BAN_SCOPE_ROOM
		}
	}
	if entry.Verb == control.VerbForceMute || entry.Verb == control.VerbUnmute {
		if entry.AudienceOnly {
			wire.MuteScope = adminpb.MuteScope_MUTE_SCOPE_AUDIENCE
		} else {
			wire.MuteScope = adminpb.MuteScope_MUTE_SCOPE_EVERYONE
		}
	}
	return wire
}

func banSubjectToProto(entry control.ModerationEntry) *adminpb.BanSubject {
	switch entry.BanKind {
	case control.BanByUsername:
		return &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: entry.BanSubject}}
	case control.BanByDevice:
		return &adminpb.BanSubject{Key: &adminpb.BanSubject_Device{
			Device: identityToProto(entry.Target),
		}}
	case control.BanByAddress:
		return &adminpb.BanSubject{Key: &adminpb.BanSubject_IpPrefix{IpPrefix: entry.BanSubject}}
	default:
		return nil
	}
}

// banFromProto reads a ban subject into the domain form, validating it.
//
// Validated here because an unparseable address prefix matches nothing, so the API
// would report a ban that does not exist -- and the moderator would stop watching.
func banFromProto(wire *adminpb.BanSubject) (control.Ban, error) {
	if wire == nil {
		return control.Ban{}, fmt.Errorf("%w: ban needs a subject", control.ErrInvalid)
	}
	var ban control.Ban
	switch key := wire.GetKey().(type) {
	case *adminpb.BanSubject_Username:
		username, err := control.NormaliseUsername(key.Username)
		if err != nil {
			return control.Ban{}, err
		}
		ban.Kind = control.BanByUsername
		ban.Username = username
	case *adminpb.BanSubject_Device:
		username, err := control.NormaliseUsername(key.Device.GetUsername())
		if err != nil {
			return control.Ban{}, err
		}
		ban.Kind = control.BanByDevice
		ban.Username = username
		ban.Device = key.Device.GetDevice()
	case *adminpb.BanSubject_IpPrefix:
		ban.Kind = control.BanByAddress
		ban.IPPrefix = key.IpPrefix
	default:
		return control.Ban{}, fmt.Errorf("%w: ban subject is empty", control.ErrInvalid)
	}
	if err := control.ValidateBanSubject(ban); err != nil {
		return control.Ban{}, err
	}
	return ban, nil
}

func timeToProto(at time.Time) *timestamppb.Timestamp {
	if at.IsZero() {
		return nil
	}
	return timestamppb.New(at)
}

func timeFromProto(wire *timestamppb.Timestamp) time.Time {
	if wire == nil {
		return time.Time{}
	}
	return wire.AsTime()
}

var trustToProto = map[timecode.Trust]adminpb.TimeTrust{
	timecode.TrustFallback:      adminpb.TimeTrust_TIME_TRUST_FALLBACK,
	timecode.TrustSynchronised:  adminpb.TimeTrust_TIME_TRUST_SYNCHRONISED,
	timecode.TrustAuthoritative: adminpb.TimeTrust_TIME_TRUST_AUTHORITATIVE,
}

func timecodeToProto(at timecode.Timecode) *adminpb.Timecode {
	if at.IsZero() {
		return nil
	}
	trust, ok := trustToProto[at.Trust]
	if !ok {
		// An unmapped trust level is a bug, and the safe way to be buggy about
		// time is to claim less than you know rather than more.
		trust = adminpb.TimeTrust_TIME_TRUST_FALLBACK
	}
	return &adminpb.Timecode{
		At:            timestamppb.New(at.At),
		UncertaintyNs: at.Uncertainty.Nanoseconds(),
		Trust:         trust,
	}
}

var trustFromProto = map[adminpb.TimeTrust]timecode.Trust{
	adminpb.TimeTrust_TIME_TRUST_SYNCHRONISED:  timecode.TrustSynchronised,
	adminpb.TimeTrust_TIME_TRUST_AUTHORITATIVE: timecode.TrustAuthoritative,
}

func timecodeFromProto(wire *adminpb.Timecode) timecode.Timecode {
	if wire == nil {
		return timecode.Timecode{}
	}
	// Anything not explicitly a trusted level reads as fallback time,
	// including UNSPECIFIED from a producer too old to know about the field.
	// The absence of a claim is not a claim.
	return timecode.New(
		timeFromProto(wire.GetAt()),
		time.Duration(wire.GetUncertaintyNs()),
		trustFromProto[wire.GetTrust()],
	)
}

// Relay-facing conversions.

func relayIdentity(wire *adminpb.Identity) control.Identity { return identityFromProto(wire) }

func banToRelay(ban control.Ban) *relaypb.Ban {
	wire := &relaypb.Ban{
		ExpiresAt: timeToProto(ban.ExpiresAt),
		Code:      protoByCode[ban.Code],
	}
	switch ban.Kind {
	case control.BanByUsername:
		wire.Key = &relaypb.Ban_Username{Username: ban.Username}
	case control.BanByDevice:
		wire.Key = &relaypb.Ban_Device{Device: &adminpb.Identity{Username: ban.Username, Device: ban.Device}}
	case control.BanByAddress:
		wire.Key = &relaypb.Ban_IpPrefix{IpPrefix: ban.IPPrefix}
	}
	return wire
}

func muteToRelay(mute store.Mute) *relaypb.Mute {
	return &relaypb.Mute{
		Identity:     identityToProto(mute.Identity),
		Media:        mediaToProto(mute.Media),
		AudienceOnly: mute.AudienceOnly,
		ExpiresAt:    timeToProto(mute.ExpiresAt),
	}
}
