// Package control holds the room model and the rules over it.
//
// No gRPC, no database, no clock of its own. The rules about who may act on
// whom are the part of this system most worth being able to test without
// standing anything up, and they are also the part that has to agree, exactly,
// with the same rules implemented in Swift for MoqomKit. Where the two disagree
// a moderator sees a button that does nothing, or worse, one that does something
// they were not entitled to do.
package control

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is a set of moderation powers.
//
// A set, never a level. Levels force an ordering between powers that have none:
// "ban permanently" and "promote to cohost" are incomparable, and any attempt to
// rank them produces a hierarchy where a chat moderator either can or cannot
// remove a cohost, both of which are wrong for someone.
//
// The bit positions are protocol. They are what a token carries and what the
// relay reads, and MoqomKit's `Capability.rawValue` uses the same ones. The gaps
// between the groups are deliberate: a new stage power added later takes an
// unused bit in the stage range instead of shifting everything after it, which
// would silently reinterpret every token already issued.
type Capability uint32

const (
	// Chat. Enforced here, in the control plane, because chat does not pass
	// through the relay.
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

	// Meta.
	CapAppointModerators Capability = 1 << 24
	CapReadModerationLog Capability = 1 << 25
	CapHandleAppeals     Capability = 1 << 26
)

// The groups, which are what presets are built from.
const (
	CapChatGroup  = CapDeleteMessage | CapTimeoutUser | CapBanFromChat | CapSetChatMode | CapManageFilters
	CapStageGroup = CapPromoteToGuest | CapPromoteToCohost | CapDemote | CapForceMute | CapSpotlight | CapManageQueue | CapLockStage
	CapRoomGroup  = CapKick | CapBanFromRoom | CapBanFromTenant | CapCloseRoom | CapChangeRoomPolicy | CapManageWatchdogs
	CapMetaGroup  = CapAppointModerators | CapReadModerationLog | CapHandleAppeals
)

// Every bit that means something, used to reject the ones that do not.
//
// An unknown bit in a token is refused rather than ignored. Ignoring it means a
// token minted by a newer control plane quietly grants less than it says, which
// is the kind of failure that shows up as "the promote button worked yesterday".
const CapAll = CapChatGroup | CapStageGroup | CapRoomGroup | CapMetaGroup

// Has reports whether every capability in other is present.
func (c Capability) Has(other Capability) bool { return c&other == other }

// IsStrictSupersetOf reports whether c holds everything in other and more.
//
// Strict is the whole point. Two moderators with identical powers must not be
// able to act on each other -- that is the partial order a level scheme is
// always approximating and always getting wrong at the margin.
func (c Capability) IsStrictSupersetOf(other Capability) bool {
	return c != other && c.Has(other)
}

// Unknown returns the bits of c that this build does not recognise.
func (c Capability) Unknown() Capability { return c &^ CapAll }

var capabilityNames = []struct {
	bit  Capability
	name string
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

// String renders the set for a log line or an error message.
func (c Capability) String() string {
	if c == 0 {
		return "none"
	}
	var names []string
	for _, entry := range capabilityNames {
		if c.Has(entry.bit) {
			names = append(names, entry.name)
		}
	}
	if unknown := c.Unknown(); unknown != 0 {
		names = append(names, fmt.Sprintf("unknown(%#x)", uint32(unknown)))
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// Preset is a named bundle, for the cases every deployment has.
type Preset string

const (
	PresetNone           Preset = "none"
	PresetModerator      Preset = "moderator"
	PresetSuperModerator Preset = "super_moderator"
	PresetOwner          Preset = "owner"
)

// Expand returns the capability set a preset names.
//
// Presets are expanded on the way in and never stored. What is stored is the
// resulting set, so a tenant that redefines "moderator" next year does not
// silently redefine what last year's moderators were allowed to do.
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

// PublishScope is what a participant may send.
//
// Orthogonal to Capability: publishing and moderating compose freely. A chat
// moderator who cannot speak and a cohost who cannot moderate are both ordinary,
// and one enum covering both would make them special cases.
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

func (p PublishScope) Has(other PublishScope) bool { return p&other == other }

// Grants is everything a participant is allowed to do.
type Grants struct {
	Publish    PublishScope
	Moderation Capability

	// IsOwner is not a capability, and keeping it out of the bitmask is the
	// point rather than an omission.
	//
	// It cannot be granted, cannot be demoted away, and makes its holder immune
	// to every capability set however complete. If it were a bit, a sufficiently
	// powerful moderator could take it, and "sufficiently powerful" is exactly
	// what an attacker who has compromised a moderator account has.
	IsOwner bool
}

// CanModerate reports whether the holder may act on target.
//
// Three rules, in this order: an owner is untouchable, an owner may act on
// anyone else, and otherwise the actor's set must strictly contain the target's.
func (g Grants) CanModerate(target Grants) bool {
	if target.IsOwner {
		return false
	}
	if g.IsOwner {
		return true
	}
	return g.Moderation.IsStrictSupersetOf(target.Moderation)
}

// CanGrant reports whether the holder may hand out requested.
//
// Nobody grants what they do not hold, which stops the obvious escalation. The
// second rule is narrower and stops the patient version of it: appointing
// moderators is never delegated, so a moderator cannot mint a peer who can mint
// another, and only an owner can widen the set of people who can appoint.
func (g Grants) CanGrant(requested Capability) bool {
	if g.IsOwner {
		return true
	}
	if requested.Has(CapAppointModerators) {
		return false
	}
	return g.Moderation.Has(requested)
}

// Role is a label derived from grants, for display only.
//
// Never a source of truth, and never stored. It exists because interfaces need
// a word, and the words people use -- "cohost", "guest" -- are summaries of a
// capability set rather than things a room actually has.
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
