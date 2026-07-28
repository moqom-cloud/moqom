package moqom

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// Translation between the SDK's types and the wire.
//
// The SDK does not expose protobuf types, and that costs this file. It buys two
// things worth more than it: a caller composes capabilities as `CapKick | CapForceMute`
// instead of building a slice of enum values, and a field renumbered in the proto is a
// change here rather than in every caller's code.
//
// Every mapping is total in one direction and lenient in the other. Sending an unknown
// value is a bug in this SDK, so those cases panic-free-fail into the zero value the
// server will reject; receiving one is a newer server talking to an older SDK, so those
// pass through as an empty string rather than being guessed at.

func capabilitiesToProto(capabilities Capability) []adminpb.Capability {
	var wire []adminpb.Capability
	for _, entry := range capabilityWire {
		if capabilities.Has(entry.capability) {
			wire = append(wire, entry.wire)
		}
	}
	return wire
}

func capabilitiesFromProto(wire []adminpb.Capability) Capability {
	var capabilities Capability
	for _, value := range wire {
		for _, entry := range capabilityWire {
			if entry.wire == value {
				capabilities |= entry.capability
			}
		}
	}
	return capabilities
}

func publishToProto(scope PublishScope) []adminpb.PublishScope {
	var wire []adminpb.PublishScope
	for _, entry := range publishWire {
		if scope.Has(entry.scope) {
			wire = append(wire, entry.wire)
		}
	}
	return wire
}

func publishFromProto(wire []adminpb.PublishScope) PublishScope {
	var scope PublishScope
	for _, value := range wire {
		for _, entry := range publishWire {
			if entry.wire == value {
				scope |= entry.scope
			}
		}
	}
	return scope
}

func grantsToProto(grants Grants) *adminpb.Grants {
	return &adminpb.Grants{
		Publish:    publishToProto(grants.Publish),
		Moderation: capabilitiesToProto(grants.Moderation),
		IsOwner:    grants.IsOwner,
	}
}

func grantsFromProto(wire *adminpb.Grants) Grants {
	if wire == nil {
		return Grants{}
	}
	return Grants{
		Publish:    publishFromProto(wire.GetPublish()),
		Moderation: capabilitiesFromProto(wire.GetModeration()),
		IsOwner:    wire.GetIsOwner(),
	}
}

func presetToProto(preset Preset) (adminpb.CapabilityPreset, error) {
	switch preset {
	case "":
		return adminpb.CapabilityPreset_CAPABILITY_PRESET_UNSPECIFIED, nil
	case PresetNone:
		return adminpb.CapabilityPreset_CAPABILITY_PRESET_NONE, nil
	case PresetModerator:
		return adminpb.CapabilityPreset_CAPABILITY_PRESET_MODERATOR, nil
	case PresetSuperModerator:
		return adminpb.CapabilityPreset_CAPABILITY_PRESET_SUPER_MODERATOR, nil
	case PresetOwner:
		return adminpb.CapabilityPreset_CAPABILITY_PRESET_OWNER, nil
	default:
		return 0, fmt.Errorf("moqom: unknown preset %q", preset)
	}
}

func mediaToProto(kinds []MediaKind) []adminpb.MediaKind {
	var wire []adminpb.MediaKind
	for _, kind := range kinds {
		switch kind {
		case MediaAudio:
			wire = append(wire, adminpb.MediaKind_MEDIA_KIND_AUDIO)
		case MediaVideo:
			wire = append(wire, adminpb.MediaKind_MEDIA_KIND_VIDEO)
		case MediaScreenShare:
			wire = append(wire, adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE)
		}
	}
	return wire
}

func mediaFromProto(wire []adminpb.MediaKind) []MediaKind {
	var kinds []MediaKind
	for _, value := range wire {
		switch value {
		case adminpb.MediaKind_MEDIA_KIND_AUDIO:
			kinds = append(kinds, MediaAudio)
		case adminpb.MediaKind_MEDIA_KIND_VIDEO:
			kinds = append(kinds, MediaVideo)
		case adminpb.MediaKind_MEDIA_KIND_SCREEN_SHARE:
			kinds = append(kinds, MediaScreenShare)
		}
	}
	return kinds
}

func presenceFromProto(wire adminpb.Presence) Presence {
	switch wire {
	case adminpb.Presence_PRESENCE_CONNECTED:
		return PresenceConnected
	case adminpb.Presence_PRESENCE_RECONNECTING:
		return PresenceReconnecting
	case adminpb.Presence_PRESENCE_AWAY:
		return PresenceAway
	case adminpb.Presence_PRESENCE_BACKGROUNDED:
		return PresenceBackgrounded
	case adminpb.Presence_PRESENCE_LEFT:
		return PresenceLeft
	default:
		return ""
	}
}

func lifecycleToProto(lifecycle Lifecycle) adminpb.RoomLifecycle {
	switch lifecycle {
	case LifecycleScheduled:
		return adminpb.RoomLifecycle_ROOM_LIFECYCLE_SCHEDULED
	case LifecycleLive:
		return adminpb.RoomLifecycle_ROOM_LIFECYCLE_LIVE
	case LifecycleActingHost:
		return adminpb.RoomLifecycle_ROOM_LIFECYCLE_ACTING_HOST
	case LifecycleEnded:
		return adminpb.RoomLifecycle_ROOM_LIFECYCLE_ENDED
	default:
		return adminpb.RoomLifecycle_ROOM_LIFECYCLE_UNSPECIFIED
	}
}

func lifecycleFromProto(wire adminpb.RoomLifecycle) Lifecycle {
	switch wire {
	case adminpb.RoomLifecycle_ROOM_LIFECYCLE_SCHEDULED:
		return LifecycleScheduled
	case adminpb.RoomLifecycle_ROOM_LIFECYCLE_LIVE:
		return LifecycleLive
	case adminpb.RoomLifecycle_ROOM_LIFECYCLE_ACTING_HOST:
		return LifecycleActingHost
	case adminpb.RoomLifecycle_ROOM_LIFECYCLE_ENDED:
		return LifecycleEnded
	default:
		return ""
	}
}

func recordingToProto(policy RecordingPolicy) adminpb.RecordingPolicy {
	switch policy {
	case RecordingAllowed:
		return adminpb.RecordingPolicy_RECORDING_POLICY_ALLOWED
	case RecordingRequired:
		return adminpb.RecordingPolicy_RECORDING_POLICY_REQUIRED
	case RecordingProhibited:
		return adminpb.RecordingPolicy_RECORDING_POLICY_PROHIBITED
	default:
		return adminpb.RecordingPolicy_RECORDING_POLICY_UNSPECIFIED
	}
}

func recordingFromProto(wire adminpb.RecordingPolicy) RecordingPolicy {
	switch wire {
	case adminpb.RecordingPolicy_RECORDING_POLICY_ALLOWED:
		return RecordingAllowed
	case adminpb.RecordingPolicy_RECORDING_POLICY_REQUIRED:
		return RecordingRequired
	case adminpb.RecordingPolicy_RECORDING_POLICY_PROHIBITED:
		return RecordingProhibited
	default:
		return ""
	}
}

func codeToProto(code ModerationCode) adminpb.ModerationCode {
	switch code {
	case CodeAdultContent:
		return adminpb.ModerationCode_MODERATION_CODE_ADULT_CONTENT
	case CodeViolence:
		return adminpb.ModerationCode_MODERATION_CODE_VIOLENCE
	case CodeHarassment:
		return adminpb.ModerationCode_MODERATION_CODE_HARASSMENT
	case CodeSelfHarm:
		return adminpb.ModerationCode_MODERATION_CODE_SELF_HARM
	case CodeCopyright:
		return adminpb.ModerationCode_MODERATION_CODE_COPYRIGHT
	case CodeAgeVerificationRequired:
		return adminpb.ModerationCode_MODERATION_CODE_AGE_VERIFICATION_REQUIRED
	case CodeTenantPolicy:
		return adminpb.ModerationCode_MODERATION_CODE_TENANT_POLICY
	case CodeOperatorAction:
		return adminpb.ModerationCode_MODERATION_CODE_OPERATOR_ACTION
	default:
		return adminpb.ModerationCode_MODERATION_CODE_UNSPECIFIED
	}
}

func codeFromProto(wire adminpb.ModerationCode) ModerationCode {
	switch wire {
	case adminpb.ModerationCode_MODERATION_CODE_ADULT_CONTENT:
		return CodeAdultContent
	case adminpb.ModerationCode_MODERATION_CODE_VIOLENCE:
		return CodeViolence
	case adminpb.ModerationCode_MODERATION_CODE_HARASSMENT:
		return CodeHarassment
	case adminpb.ModerationCode_MODERATION_CODE_SELF_HARM:
		return CodeSelfHarm
	case adminpb.ModerationCode_MODERATION_CODE_COPYRIGHT:
		return CodeCopyright
	case adminpb.ModerationCode_MODERATION_CODE_AGE_VERIFICATION_REQUIRED:
		return CodeAgeVerificationRequired
	case adminpb.ModerationCode_MODERATION_CODE_TENANT_POLICY:
		return CodeTenantPolicy
	case adminpb.ModerationCode_MODERATION_CODE_OPERATOR_ACTION:
		return CodeOperatorAction
	default:
		return ""
	}
}

func verbToProto(verb ModerationVerb) adminpb.ModerationVerb {
	for _, entry := range verbWire {
		if entry.verb == verb {
			return entry.wire
		}
	}
	return adminpb.ModerationVerb_MODERATION_VERB_UNSPECIFIED
}

func verbFromProto(wire adminpb.ModerationVerb) ModerationVerb {
	for _, entry := range verbWire {
		if entry.wire == wire {
			return entry.verb
		}
	}
	return ""
}

func banScopeToProto(scope BanScope) adminpb.BanScope {
	switch scope {
	case BanFromRoom:
		return adminpb.BanScope_BAN_SCOPE_ROOM
	case BanFromTenant:
		return adminpb.BanScope_BAN_SCOPE_TENANT
	default:
		return adminpb.BanScope_BAN_SCOPE_UNSPECIFIED
	}
}

func banScopeFromProto(wire adminpb.BanScope) BanScope {
	switch wire {
	case adminpb.BanScope_BAN_SCOPE_ROOM:
		return BanFromRoom
	case adminpb.BanScope_BAN_SCOPE_TENANT:
		return BanFromTenant
	default:
		return ""
	}
}

func muteScopeToProto(scope MuteScope) adminpb.MuteScope {
	switch scope {
	case MuteForEveryone:
		return adminpb.MuteScope_MUTE_SCOPE_EVERYONE
	case MuteForAudience:
		return adminpb.MuteScope_MUTE_SCOPE_AUDIENCE
	default:
		return adminpb.MuteScope_MUTE_SCOPE_UNSPECIFIED
	}
}

func muteScopeFromProto(wire adminpb.MuteScope) MuteScope {
	switch wire {
	case adminpb.MuteScope_MUTE_SCOPE_EVERYONE:
		return MuteForEveryone
	case adminpb.MuteScope_MUTE_SCOPE_AUDIENCE:
		return MuteForAudience
	default:
		return ""
	}
}

func watchdogKindToProto(kind WatchdogKind) adminpb.WatchdogKind {
	switch kind {
	case WatchdogRecording:
		return adminpb.WatchdogKind_WATCHDOG_KIND_RECORDING
	case WatchdogSafety:
		return adminpb.WatchdogKind_WATCHDOG_KIND_SAFETY
	case WatchdogAnalytics:
		return adminpb.WatchdogKind_WATCHDOG_KIND_ANALYTICS
	case WatchdogCompliance:
		return adminpb.WatchdogKind_WATCHDOG_KIND_COMPLIANCE
	case WatchdogCustom:
		return adminpb.WatchdogKind_WATCHDOG_KIND_CUSTOM
	default:
		return adminpb.WatchdogKind_WATCHDOG_KIND_UNSPECIFIED
	}
}

func watchdogKindFromProto(wire adminpb.WatchdogKind) WatchdogKind {
	switch wire {
	case adminpb.WatchdogKind_WATCHDOG_KIND_RECORDING:
		return WatchdogRecording
	case adminpb.WatchdogKind_WATCHDOG_KIND_SAFETY:
		return WatchdogSafety
	case adminpb.WatchdogKind_WATCHDOG_KIND_ANALYTICS:
		return WatchdogAnalytics
	case adminpb.WatchdogKind_WATCHDOG_KIND_COMPLIANCE:
		return WatchdogCompliance
	case adminpb.WatchdogKind_WATCHDOG_KIND_CUSTOM:
		return WatchdogCustom
	default:
		return ""
	}
}

func operatorToProto(operator WatchdogOperator) adminpb.WatchdogOperator {
	switch operator {
	case OperatedByPlatform:
		return adminpb.WatchdogOperator_WATCHDOG_OPERATOR_PLATFORM
	case OperatedByTenant:
		return adminpb.WatchdogOperator_WATCHDOG_OPERATOR_TENANT
	case OperatedByThirdParty:
		return adminpb.WatchdogOperator_WATCHDOG_OPERATOR_THIRD_PARTY
	default:
		return adminpb.WatchdogOperator_WATCHDOG_OPERATOR_UNSPECIFIED
	}
}

func operatorFromProto(wire adminpb.WatchdogOperator) WatchdogOperator {
	switch wire {
	case adminpb.WatchdogOperator_WATCHDOG_OPERATOR_PLATFORM:
		return OperatedByPlatform
	case adminpb.WatchdogOperator_WATCHDOG_OPERATOR_TENANT:
		return OperatedByTenant
	case adminpb.WatchdogOperator_WATCHDOG_OPERATOR_THIRD_PARTY:
		return OperatedByThirdParty
	default:
		return ""
	}
}

func healthFromProto(wire adminpb.WatchdogHealth) WatchdogHealth {
	switch wire {
	case adminpb.WatchdogHealth_WATCHDOG_HEALTH_ATTACHED:
		return WatchdogAttached
	case adminpb.WatchdogHealth_WATCHDOG_HEALTH_DEGRADED:
		return WatchdogDegraded
	case adminpb.WatchdogHealth_WATCHDOG_HEALTH_FAILED:
		return WatchdogFailed
	default:
		return ""
	}
}

func identityToProto(identity Identity) *adminpb.Identity {
	return &adminpb.Identity{Username: identity.Username, Device: identity.Device}
}

func identityFromProto(wire *adminpb.Identity) Identity {
	if wire == nil {
		return Identity{}
	}
	return Identity{Username: wire.GetUsername(), Device: wire.GetDevice()}
}

func refToProto(ref ParticipantRef) *adminpb.ParticipantRef {
	return &adminpb.ParticipantRef{Username: ref.Username, Device: ref.Device}
}

func refFromProto(wire *adminpb.ParticipantRef) ParticipantRef {
	if wire == nil {
		return ParticipantRef{}
	}
	return ParticipantRef{Username: wire.GetUsername(), Device: wire.GetDevice()}
}

func subjectToProto(subject BanSubject) (*adminpb.BanSubject, error) {
	set := 0
	if subject.Username != "" {
		set++
	}
	if subject.DeviceOf.Username != "" || subject.DeviceOf.Device != "" {
		set++
	}
	if subject.IPPrefix != "" {
		set++
	}
	if set != 1 {
		return nil, fmt.Errorf("moqom: a ban needs exactly one subject, got %d", set)
	}

	switch {
	case subject.Username != "":
		return &adminpb.BanSubject{Key: &adminpb.BanSubject_Username{Username: subject.Username}}, nil
	case subject.IPPrefix != "":
		return &adminpb.BanSubject{Key: &adminpb.BanSubject_IpPrefix{IpPrefix: subject.IPPrefix}}, nil
	default:
		return &adminpb.BanSubject{Key: &adminpb.BanSubject_Device{Device: identityToProto(subject.DeviceOf)}}, nil
	}
}

func subjectFromProto(wire *adminpb.BanSubject) BanSubject {
	if wire == nil {
		return BanSubject{}
	}
	switch key := wire.GetKey().(type) {
	case *adminpb.BanSubject_Username:
		return BanSubject{Username: key.Username}
	case *adminpb.BanSubject_Device:
		return BanSubject{DeviceOf: identityFromProto(key.Device)}
	case *adminpb.BanSubject_IpPrefix:
		return BanSubject{IPPrefix: key.IpPrefix}
	default:
		return BanSubject{}
	}
}

func policyToProto(policy Policy) *adminpb.RoomPolicy {
	return &adminpb.RoomPolicy{
		AgeGate: &adminpb.AgeGate{
			MinimumAge:          policy.AgeGate.MinimumAge,
			RequireVerification: policy.AgeGate.RequireVerification,
		},
		Recording:       recordingToProto(policy.Recording),
		ResidencyZone:   policy.ResidencyZone,
		MaxParticipants: policy.MaxParticipants,
		StageSlots:      policy.StageSlots,
		WatchdogLimit:   policy.WatchdogLimit,
	}
}

func policyFromProto(wire *adminpb.RoomPolicy) Policy {
	if wire == nil {
		return Policy{}
	}
	return Policy{
		AgeGate: AgeGate{
			MinimumAge:          wire.GetAgeGate().GetMinimumAge(),
			RequireVerification: wire.GetAgeGate().GetRequireVerification(),
		},
		Recording:       recordingFromProto(wire.GetRecording()),
		ResidencyZone:   wire.GetResidencyZone(),
		MaxParticipants: wire.GetMaxParticipants(),
		StageSlots:      wire.GetStageSlots(),
		WatchdogLimit:   wire.GetWatchdogLimit(),
	}
}

func roomFromProto(wire *adminpb.Room) Room {
	if wire == nil {
		return Room{}
	}
	return Room{
		Tenant:            wire.GetTenant(),
		Name:              wire.GetRoom(),
		Namespace:         wire.GetNamespace(),
		Policy:            policyFromProto(wire.GetPolicy()),
		Lifecycle:         lifecycleFromProto(wire.GetLifecycle()),
		StartsAt:          timeFromProto(wire.GetStartsAt()),
		CreatedAt:         timeFromProto(wire.GetCreatedAt()),
		EndedAt:           timeFromProto(wire.GetEndedAt()),
		ActingHost:        identityFromProto(wire.GetActingHost()),
		ParticipantCount:  wire.GetParticipantCount(),
		DistinctUserCount: wire.GetDistinctUserCount(),
		IsRecording:       wire.GetIsRecording(),
	}
}

func participantFromProto(wire *adminpb.Participant) Participant {
	if wire == nil {
		return Participant{}
	}
	return Participant{
		Identity:               identityFromProto(wire.GetIdentity()),
		Grants:                 grantsFromProto(wire.GetGrants()),
		Presence:               presenceFromProto(wire.GetPresence()),
		SelfMuted:              mediaFromProto(wire.GetSelfMuted()),
		ForcedMutedForEveryone: mediaFromProto(wire.GetForcedMutedForEveryone()),
		ForcedMutedForAudience: mediaFromProto(wire.GetForcedMutedForAudience()),
		IsSpotlighted:          wire.GetIsSpotlighted(),
		IsBackstage:            wire.GetIsBackstage(),
		JoinedAt:               timeFromProto(wire.GetJoinedAt()),
		RelayID:                wire.GetRelayId(),
	}
}

func watchdogFromProto(wire *adminpb.Watchdog) Watchdog {
	if wire == nil {
		return Watchdog{}
	}
	return Watchdog{
		ID:           wire.GetId(),
		Kind:         watchdogKindFromProto(wire.GetKind()),
		CustomKind:   wire.GetCustomKind(),
		OperatedBy:   operatorFromProto(wire.GetOperatedBy()),
		OperatorName: wire.GetOperatorName(),
		Descriptor:   wire.GetDescriptor_(),
		Scope:        wire.GetScope(),
		Health:       healthFromProto(wire.GetHealth()),
		AttachedAt:   timeFromProto(wire.GetAttachedAt()),
		IsBillable:   wire.GetIsBillable(),
		PinnedLayer:  wire.GetPinnedLayer(),
	}
}

func entryFromProto(wire *adminpb.ModerationEntry) ModerationEntry {
	if wire == nil {
		return ModerationEntry{}
	}
	return ModerationEntry{
		ID:             wire.GetId(),
		Tenant:         wire.GetTenant(),
		Room:           wire.GetRoom(),
		At:             timeFromProto(wire.GetAt()),
		Verb:           verbFromProto(wire.GetVerb()),
		ServiceAccount: wire.GetServiceAccount(),
		OnBehalfOf:     wire.GetOnBehalfOf(),
		Target:         refFromProto(wire.GetTarget()),
		BanSubject:     subjectFromProto(wire.GetBanSubject()),
		Code:           codeFromProto(wire.GetCode()),
		Reason:         wire.GetReason(),
		ExpiresAt:      timeFromProto(wire.GetExpiresAt()),
		Media:          mediaFromProto(wire.GetMedia()),
		MuteScope:      muteScopeFromProto(wire.GetMuteScope()),
		BanScope:       banScopeFromProto(wire.GetBanScope()),
		Grants:         grantsFromProto(wire.GetGrants()),
		Enforced:       wire.GetEnforced(),
		DecidedByModel: wire.GetDecidedByModel(),
	}
}

// timeFromProto turns an absent timestamp into a zero time rather than the epoch.
//
// The distinction matters: an unset ExpiresAt means "permanent", and rendering that as
// 1970 would make every permanent ban look expired.
func timeFromProto(wire *timestamppb.Timestamp) time.Time {
	if wire == nil {
		return time.Time{}
	}
	return wire.AsTime()
}

func timeToProto(at time.Time) *timestamppb.Timestamp {
	if at.IsZero() {
		return nil
	}
	return timestamppb.New(at)
}

// durationToProto keeps zero meaning "unset", which each caller documents: permanent
// for a ban, until-lifted for a mute, server-default for a token lifetime.
func durationToProto(duration time.Duration) *durationpb.Duration {
	if duration <= 0 {
		return nil
	}
	return durationpb.New(duration)
}

var capabilityWire = []struct {
	capability Capability
	wire       adminpb.Capability
}{
	{CapDeleteMessage, adminpb.Capability_CAPABILITY_DELETE_MESSAGE},
	{CapTimeoutUser, adminpb.Capability_CAPABILITY_TIMEOUT_USER},
	{CapBanFromChat, adminpb.Capability_CAPABILITY_BAN_FROM_CHAT},
	{CapSetChatMode, adminpb.Capability_CAPABILITY_SET_CHAT_MODE},
	{CapManageFilters, adminpb.Capability_CAPABILITY_MANAGE_FILTERS},
	{CapPromoteToGuest, adminpb.Capability_CAPABILITY_PROMOTE_TO_GUEST},
	{CapPromoteToCohost, adminpb.Capability_CAPABILITY_PROMOTE_TO_COHOST},
	{CapDemote, adminpb.Capability_CAPABILITY_DEMOTE},
	{CapForceMute, adminpb.Capability_CAPABILITY_FORCE_MUTE},
	{CapSpotlight, adminpb.Capability_CAPABILITY_SPOTLIGHT},
	{CapManageQueue, adminpb.Capability_CAPABILITY_MANAGE_QUEUE},
	{CapLockStage, adminpb.Capability_CAPABILITY_LOCK_STAGE},
	{CapKick, adminpb.Capability_CAPABILITY_KICK},
	{CapBanFromRoom, adminpb.Capability_CAPABILITY_BAN_FROM_ROOM},
	{CapBanFromTenant, adminpb.Capability_CAPABILITY_BAN_FROM_TENANT},
	{CapCloseRoom, adminpb.Capability_CAPABILITY_CLOSE_ROOM},
	{CapChangeRoomPolicy, adminpb.Capability_CAPABILITY_CHANGE_ROOM_POLICY},
	{CapManageWatchdogs, adminpb.Capability_CAPABILITY_MANAGE_WATCHDOGS},
	{CapAppointModerators, adminpb.Capability_CAPABILITY_APPOINT_MODERATORS},
	{CapReadModerationLog, adminpb.Capability_CAPABILITY_READ_MODERATION_LOG},
	{CapHandleAppeals, adminpb.Capability_CAPABILITY_HANDLE_APPEALS},
}

var publishWire = []struct {
	scope PublishScope
	wire  adminpb.PublishScope
}{
	{PublishAudio, adminpb.PublishScope_PUBLISH_SCOPE_AUDIO},
	{PublishVideo, adminpb.PublishScope_PUBLISH_SCOPE_VIDEO},
	{PublishScreenShare, adminpb.PublishScope_PUBLISH_SCOPE_SCREEN_SHARE},
	{PublishData, adminpb.PublishScope_PUBLISH_SCOPE_DATA},
}

var verbWire = []struct {
	verb ModerationVerb
	wire adminpb.ModerationVerb
}{
	{VerbKick, adminpb.ModerationVerb_MODERATION_VERB_KICK},
	{VerbBan, adminpb.ModerationVerb_MODERATION_VERB_BAN},
	{VerbUnban, adminpb.ModerationVerb_MODERATION_VERB_UNBAN},
	{VerbForceMute, adminpb.ModerationVerb_MODERATION_VERB_FORCE_MUTE},
	{VerbUnmute, adminpb.ModerationVerb_MODERATION_VERB_UNMUTE},
	{VerbSetGrants, adminpb.ModerationVerb_MODERATION_VERB_SET_GRANTS},
	{VerbSpotlight, adminpb.ModerationVerb_MODERATION_VERB_SPOTLIGHT},
	{VerbCloseRoom, adminpb.ModerationVerb_MODERATION_VERB_CLOSE_ROOM},
	{VerbAttachWatchdog, adminpb.ModerationVerb_MODERATION_VERB_ATTACH_WATCHDOG},
	{VerbDetachWatchdog, adminpb.ModerationVerb_MODERATION_VERB_DETACH_WATCHDOG},
	{VerbUpdateRoomPolicy, adminpb.ModerationVerb_MODERATION_VERB_UPDATE_ROOM_POLICY},
}
