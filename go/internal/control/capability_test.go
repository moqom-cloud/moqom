package control

import "testing"

// The bit positions are protocol: a token minted here is read by a Rust relay
// and a Swift client, and all three have to agree. This table is the Swift
// enum's rawValues, copied deliberately rather than derived, so a change to
// either side fails here instead of failing in the field as a moderator whose
// buttons stopped working.
func TestCapabilityBitsMatchTheWireContract(t *testing.T) {
	expected := map[string]uint32{
		"delete_message":      1 << 0,
		"timeout_user":        1 << 1,
		"ban_from_chat":       1 << 2,
		"set_chat_mode":       1 << 3,
		"manage_filters":      1 << 4,
		"promote_to_guest":    1 << 8,
		"promote_to_cohost":   1 << 9,
		"demote":              1 << 10,
		"force_mute":          1 << 11,
		"spotlight":           1 << 12,
		"manage_queue":        1 << 13,
		"lock_stage":          1 << 14,
		"kick":                1 << 16,
		"ban_from_room":       1 << 17,
		"ban_from_tenant":     1 << 18,
		"close_room":          1 << 19,
		"change_room_policy":  1 << 20,
		"manage_watchdogs":    1 << 21,
		"appoint_moderators":  1 << 24,
		"read_moderation_log": 1 << 25,
		"handle_appeals":      1 << 26,
	}

	if len(capabilityNames) != len(expected) {
		t.Fatalf("capability count changed: %d named, %d in the contract", len(capabilityNames), len(expected))
	}
	for _, entry := range capabilityNames {
		want, ok := expected[entry.name]
		if !ok {
			t.Errorf("%s is not in the wire contract", entry.name)
			continue
		}
		if uint32(entry.bit) != want {
			t.Errorf("%s is bit %#x, contract says %#x", entry.name, uint32(entry.bit), want)
		}
	}
}

// Two moderators with identical powers must not be able to act on each other.
// This is the property a level scheme cannot express, and the reason the
// comparison is a strict superset rather than a comparison of sizes.
func TestPeersCannotModerateEachOther(t *testing.T) {
	mod := Grants{Moderation: CapChatGroup}
	other := Grants{Moderation: CapChatGroup}

	if mod.CanModerate(other) {
		t.Error("a moderator acted on a peer with identical powers")
	}

	super := Grants{Moderation: CapChatGroup | CapStageGroup}
	if !super.CanModerate(mod) {
		t.Error("a super moderator could not act on a moderator")
	}
	if mod.CanModerate(super) {
		t.Error("a moderator acted on someone more powerful")
	}
}

// Specialised moderators are incomparable, so neither may act on the other even
// though each holds powers the other lacks. Any scheme that ranked them would
// have to pick one, and would be wrong for somebody.
func TestIncomparableSetsCannotActEitherWay(t *testing.T) {
	chat := Grants{Moderation: CapChatGroup}
	stage := Grants{Moderation: CapStageGroup}

	if chat.CanModerate(stage) || stage.CanModerate(chat) {
		t.Error("incomparable capability sets were ordered")
	}
}

func TestOwnerIsUntouchableAndUniversal(t *testing.T) {
	owner := Grants{IsOwner: true}
	// Deliberately more capable than the owner's own set, which is empty here:
	// the point is that no accumulation of capabilities reaches ownership.
	everything := Grants{Moderation: CapAll}

	if everything.CanModerate(owner) {
		t.Error("a fully capable moderator acted on the owner")
	}
	if !owner.CanModerate(everything) {
		t.Error("the owner could not act on a moderator")
	}
}

// Appointing moderators is the one power that must not spread on its own, or a
// single compromised moderator account grows into as many as it likes.
func TestAppointingModeratorsIsNeverDelegated(t *testing.T) {
	appointer := Grants{Moderation: CapAll &^ 0} // holds appoint_moderators
	if !appointer.Moderation.Has(CapAppointModerators) {
		t.Fatal("test setup: appointer should hold appoint_moderators")
	}
	if appointer.CanGrant(CapAppointModerators) {
		t.Error("a non-owner delegated the power to appoint moderators")
	}

	owner := Grants{IsOwner: true}
	if !owner.CanGrant(CapAppointModerators) {
		t.Error("the owner could not appoint someone who can appoint moderators")
	}
}

func TestNobodyGrantsWhatTheyDoNotHold(t *testing.T) {
	chat := Grants{Moderation: CapChatGroup}

	if chat.CanGrant(CapKick) {
		t.Error("a chat moderator granted a room power")
	}
	if !chat.CanGrant(CapDeleteMessage) {
		t.Error("a chat moderator could not grant a power it holds")
	}
}

func TestPresetsExpandToTheDocumentedSets(t *testing.T) {
	cases := []struct {
		preset Preset
		want   Capability
	}{
		{PresetNone, 0},
		{PresetModerator, CapChatGroup},
		{PresetSuperModerator, CapChatGroup | CapStageGroup},
		{PresetOwner, CapAll},
	}
	for _, c := range cases {
		got, ok := c.preset.Expand()
		if !ok {
			t.Errorf("%s did not expand", c.preset)
			continue
		}
		if got != c.want {
			t.Errorf("%s expanded to %s, want %s", c.preset, got, c.want)
		}
	}
	if _, ok := Preset("archbishop").Expand(); ok {
		t.Error("an unknown preset expanded instead of being refused")
	}
}

// A preset is a bundle over the same set, not a parallel concept: super
// moderator has to be a strict superset of moderator or the two presets would be
// unable to act on each other, which is not what "super" means to anyone.
func TestSuperModeratorOutranksModerator(t *testing.T) {
	mod, _ := PresetModerator.Expand()
	super, _ := PresetSuperModerator.Expand()

	if !super.IsStrictSupersetOf(mod) {
		t.Error("super moderator does not strictly contain moderator")
	}
}

func TestUnknownCapabilityBitsAreVisible(t *testing.T) {
	future := Capability(1 << 30)
	if future.Unknown() != future {
		t.Error("an unrecognised bit was treated as known")
	}
	if (CapKick | future).Unknown() != future {
		t.Error("Unknown should report only the unrecognised bits")
	}
}

func TestRoleIsDerivedNotStored(t *testing.T) {
	cases := []struct {
		name   string
		grants Grants
		want   Role
	}{
		{"owner", Grants{IsOwner: true}, RoleOwner},
		{"cohost", Grants{Publish: PublishCamera, Moderation: CapChatGroup}, RoleCohost},
		{"guest", Grants{Publish: PublishCamera}, RoleGuest},
		{"audio guest", Grants{Publish: PublishAudio}, RoleGuest},
		{"moderator", Grants{Moderation: CapChatGroup}, RoleModerator},
		{"viewer", Grants{}, RoleViewer},
	}
	for _, c := range cases {
		if got := c.grants.Role(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// A verb with no case in the switch must be denied rather than allowed, so
// adding one and forgetting to guard it fails closed.
func TestUnknownVerbIsRefused(t *testing.T) {
	err := Authorise(Grants{IsOwner: true}, ModerationVerb("vaporise"), nil)
	if err == nil {
		t.Fatal("an unknown verb was authorised for the owner")
	}
}

func TestAuthoriseNeedsBothCapabilityAndStanding(t *testing.T) {
	// Holds kick, but the target holds more than they do.
	actor := Grants{Moderation: CapKick}
	target := Grants{Moderation: CapKick | CapBanFromRoom}

	if err := Authorise(actor, VerbKick, &target); err == nil {
		t.Error("kicking someone more powerful was allowed")
	}
	if err := Authorise(actor, VerbKick, &Grants{}); err != nil {
		t.Errorf("kicking a viewer was refused: %v", err)
	}
	if err := Authorise(Grants{}, VerbKick, &Grants{}); err == nil {
		t.Error("kicking without the kick capability was allowed")
	}
}
