package moqom

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is a set of moderation powers.
//
// A set, never a level. Levels force an ordering between powers that have none --
// "ban permanently" and "promote to cohost" are incomparable -- and the ordering that
// does matter is the partial one: an actor may act on a target only when the actor's
// set strictly contains the target's. Two moderators with identical powers therefore
// cannot act on each other, which is the property every level scheme gets wrong.
//
// Compose with bitwise or:
//
//	moqom.CapKick | moqom.CapForceMute
//
// The bit positions are protocol rather than an implementation detail: a token carries
// them and the relay reads them, and MoqomKit's Capability.rawValue uses the same
// ones. The gaps between the groups are deliberate, so a power added later takes an
// unused bit in its own range instead of shifting every bit after it and quietly
// reinterpreting tokens already issued.
type Capability uint32

const (
	// Chat. Enforced by the control plane, since chat does not pass through a relay.
	CapDeleteMessage Capability = 1 << 0
	CapTimeoutUser   Capability = 1 << 1
	CapBanFromChat   Capability = 1 << 2
	CapSetChatMode   Capability = 1 << 3
	CapManageFilters Capability = 1 << 4

	// Stage. Enforced by the relay, so these ride in the token.
	CapPromoteToGuest  Capability = 1 << 8
	CapPromoteToCohost Capability = 1 << 9
	CapDemote          Capability = 1 << 10
	CapForceMute       Capability = 1 << 11
	CapSpotlight       Capability = 1 << 12
	CapManageQueue     Capability = 1 << 13
	CapLockStage       Capability = 1 << 14

	// Room.
	CapKick             Capability = 1 << 16
	CapBanFromRoom      Capability = 1 << 17
	CapBanFromTenant    Capability = 1 << 18
	CapCloseRoom        Capability = 1 << 19
	CapChangeRoomPolicy Capability = 1 << 20
	CapManageWatchdogs  Capability = 1 << 21

	// Meta. Never delegated by anyone but an owner.
	CapAppointModerators Capability = 1 << 24
	CapReadModerationLog Capability = 1 << 25
	CapHandleAppeals     Capability = 1 << 26
)

// The groups presets are built from.
const (
	CapChatGroup  = CapDeleteMessage | CapTimeoutUser | CapBanFromChat | CapSetChatMode | CapManageFilters
	CapStageGroup = CapPromoteToGuest | CapPromoteToCohost | CapDemote | CapForceMute | CapSpotlight | CapManageQueue | CapLockStage
	CapRoomGroup  = CapKick | CapBanFromRoom | CapBanFromTenant | CapCloseRoom | CapChangeRoomPolicy | CapManageWatchdogs
	CapMetaGroup  = CapAppointModerators | CapReadModerationLog | CapHandleAppeals

	// CapAll is every bit this version knows. A bit outside it is refused rather
	// than dropped, so a set this build does not understand fails loudly instead
	// of granting less than it says.
	CapAll = CapChatGroup | CapStageGroup | CapRoomGroup | CapMetaGroup
)

// Has reports whether every capability in other is present.
func (c Capability) Has(other Capability) bool { return c&other == other }

// String renders the set for a log line.
func (c Capability) String() string {
	if c == 0 {
		return "none"
	}
	var names []string
	for _, entry := range capabilityNames {
		if c.Has(entry.capability) {
			names = append(names, entry.name)
		}
	}
	if unknown := c &^ CapAll; unknown != 0 {
		names = append(names, fmt.Sprintf("unknown(%#x)", uint32(unknown)))
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// PublishScope is what a participant may send.
//
// Orthogonal to Capability, because publishing and moderating compose freely: a chat
// moderator who cannot speak and a cohost who cannot moderate are both ordinary, and
// one ladder covering both would make each of them a special case.
//
// A grant is permission and never an action. PublishVideo does not turn a camera on.
type PublishScope uint8

const (
	PublishAudio       PublishScope = 1 << 0
	PublishVideo       PublishScope = 1 << 1
	PublishScreenShare PublishScope = 1 << 2
	PublishData        PublishScope = 1 << 3

	PublishNone   PublishScope = 0
	PublishCamera              = PublishAudio | PublishVideo
	PublishFull                = PublishAudio | PublishVideo | PublishScreenShare | PublishData
)

// Has reports whether every scope in other is present.
func (p PublishScope) Has(other PublishScope) bool { return p&other == other }

// String renders the set for a log line.
func (p PublishScope) String() string {
	if p == 0 {
		return "none"
	}
	var names []string
	for _, entry := range publishNames {
		if p.Has(entry.scope) {
			names = append(names, entry.name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// Grants is everything a participant is allowed to do.
type Grants struct {
	Publish    PublishScope
	Moderation Capability

	// IsOwner is not a capability, and keeping it out of the bitmask is the point
	// rather than an oversight: it cannot be granted, cannot be demoted away, and
	// makes its holder immune to every capability set however complete. As a bit,
	// a sufficiently powerful moderator could take it -- and "sufficiently
	// powerful" is what an attacker who has compromised a moderator account has.
	IsOwner bool
}

// Preset is a named bundle, for the cases every deployment has.
//
// Expanded by the server on the way in and never stored: what is stored is the
// resulting set, so redefining "moderator" next year does not retroactively redefine
// what this year's moderators were allowed to do.
type Preset string

const (
	// PresetNone is a viewer: no moderation powers at all.
	PresetNone Preset = "none"
	// PresetModerator is chat powers.
	PresetModerator Preset = "moderator"
	// PresetSuperModerator is chat and stage powers -- the difference a "super mod"
	// usually means, which is the power to promote and remove speakers.
	PresetSuperModerator Preset = "super_moderator"
	// PresetOwner is every capability. Note that this is still not ownership,
	// which is Grants.IsOwner and is not grantable.
	PresetOwner Preset = "owner"
)

// Expand returns the capabilities a preset names.
//
// Provided so a caller can show what a preset means before applying it. The server
// expands independently and its answer is the one that counts.
func (p Preset) Expand() (Capability, bool) {
	switch p {
	case PresetNone:
		return 0, true
	case PresetModerator:
		return CapChatGroup, true
	case PresetSuperModerator:
		return CapChatGroup | CapStageGroup, true
	case PresetOwner:
		return CapAll, true
	default:
		return 0, false
	}
}

// Role is a label derived from grants, for display only.
//
// Never a source of truth and never stored. It exists because interfaces need a word,
// and the words people use -- "cohost", "guest" -- are summaries of a capability set
// rather than anything a room actually has.
type Role string

const (
	RoleOwner     Role = "owner"
	RoleCohost    Role = "cohost"
	RoleGuest     Role = "guest"
	RoleModerator Role = "moderator"
	RoleViewer    Role = "viewer"
)

// Role summarises grants the same way MoqomKit's ParticipantRole does.
func (g Grants) Role() Role {
	switch {
	case g.IsOwner:
		return RoleOwner
	case g.Publish.Has(PublishVideo) && g.Moderation != 0:
		return RoleCohost
	case g.Publish != PublishNone:
		return RoleGuest
	case g.Moderation != 0:
		return RoleModerator
	default:
		return RoleViewer
	}
}

var capabilityNames = []struct {
	capability Capability
	name       string
}{
	{CapDeleteMessage, "delete_message"},
	{CapTimeoutUser, "timeout_user"},
	{CapBanFromChat, "ban_from_chat"},
	{CapSetChatMode, "set_chat_mode"},
	{CapManageFilters, "manage_filters"},
	{CapPromoteToGuest, "promote_to_guest"},
	{CapPromoteToCohost, "promote_to_cohost"},
	{CapDemote, "demote"},
	{CapForceMute, "force_mute"},
	{CapSpotlight, "spotlight"},
	{CapManageQueue, "manage_queue"},
	{CapLockStage, "lock_stage"},
	{CapKick, "kick"},
	{CapBanFromRoom, "ban_from_room"},
	{CapBanFromTenant, "ban_from_tenant"},
	{CapCloseRoom, "close_room"},
	{CapChangeRoomPolicy, "change_room_policy"},
	{CapManageWatchdogs, "manage_watchdogs"},
	{CapAppointModerators, "appoint_moderators"},
	{CapReadModerationLog, "read_moderation_log"},
	{CapHandleAppeals, "handle_appeals"},
}

var publishNames = []struct {
	scope PublishScope
	name  string
}{
	{PublishAudio, "audio"},
	{PublishVideo, "video"},
	{PublishScreenShare, "screen_share"},
	{PublishData, "data"},
}
