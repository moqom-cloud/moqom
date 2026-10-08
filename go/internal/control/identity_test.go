package control

import (
	"strings"
	"testing"
	"time"
)

// The derivation has to match MoqomKit's, because a device ban is keyed on this
// value and a session is identified by it. Two derivations means a ban that
// silently fails to match the thing it was aimed at.
func TestDeviceIsTheThumbprintPrefix(t *testing.T) {
	thumbprint := "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmM"

	device, err := DeviceFromThumbprint(thumbprint)
	if err != nil {
		t.Fatalf("derivation failed: %v", err)
	}
	if len(device) != DeviceIDLength {
		t.Errorf("device is %d characters, want %d", len(device), DeviceIDLength)
	}
	if !strings.HasPrefix(thumbprint, device) {
		t.Errorf("%q is not a prefix of the thumbprint", device)
	}
}

// A thumbprint shorter than the prefix length is kept whole rather than padded.
// Padding would map two different short keys to the same device.
func TestShortThumbprintIsKeptWhole(t *testing.T) {
	device, err := DeviceFromThumbprint("abc")
	if err != nil {
		t.Fatalf("derivation failed: %v", err)
	}
	if device != "abc" {
		t.Errorf("got %q, want the thumbprint unchanged", device)
	}
}

// Two spellings of the same name must not be two accounts, or a ban on one does
// not cover the other and an impostor registers the twin.
func TestUsernamesAreCanonicalised(t *testing.T) {
	composed := "Ren\u00e9"    // é as one code point
	decomposed := "Rene\u0301" // e + combining acute

	first, err := NormaliseUsername(composed)
	if err != nil {
		t.Fatalf("normalising %q failed: %v", composed, err)
	}
	second, err := NormaliseUsername(decomposed)
	if err != nil {
		t.Fatalf("normalising %q failed: %v", decomposed, err)
	}
	if first != second {
		t.Errorf("%q and %q normalised to different names", composed, decomposed)
	}
}

func TestControlCharactersAreRefusedInNames(t *testing.T) {
	if _, err := NormaliseUsername("alice\nadmin"); err == nil {
		t.Error("a username containing a newline was accepted")
	}
	if _, err := NormaliseUsername(""); err == nil {
		t.Error("an empty username was accepted")
	}
}

// The tuple layout is protocol. A backend that guesses at it breaks when it
// changes, which is why the API returns the rendered form and this asserts the
// field order it is rendered from.
func TestNamespaceLayouts(t *testing.T) {
	id := Identity{Username: "alice", Device: "iphone-3f9a"}

	cases := []struct {
		name string
		got  Namespace
		want []string
	}{
		{"room", RoomNamespace("acme", "room-42"), []string{"acme", "r", "room-42"}},
		{"participant", ParticipantNamespace("acme", "room-42", id), []string{"acme", "r", "room-42", "alice", "iphone-3f9a"}},
		{"backstage", BackstageNamespace("acme", "room-42"), []string{"acme", "bs", "room-42"}},
		{"backstage participant", BackstageParticipantNamespace("acme", "room-42", id), []string{"acme", "bs", "room-42", "alice", "iphone-3f9a"}},
		{"personal", PersonalNamespace("acme", id), []string{"acme", "u", "alice", "iphone-3f9a"}},
	}
	for _, c := range cases {
		if len(c.got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, c.got, c.want)
			continue
		}
		for i := range c.want {
			if c.got[i] != c.want[i] {
				t.Errorf("%s: field %d is %q, want %q", c.name, i, c.got[i], c.want[i])
			}
		}
		if err := c.got.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// Backstage is a sibling of the room prefix rather than a child of it, which is
// what makes going live a namespace change nobody has to be told about. If it
// were nested, every viewer subscribed to the room would already be receiving it.
func TestBackstageIsOutsideTheRoomPrefix(t *testing.T) {
	room := RoomNamespace("acme", "room-42")
	backstage := BackstageParticipantNamespace("acme", "room-42", Identity{Username: "alice", Device: "d1"})

	if backstage.HasPrefix(room) {
		t.Error("a backstage namespace is inside the prefix viewers subscribe to")
	}
}

// Tuple comparison, not string comparison: "room-1" is a string prefix of
// "room-10" and is not a tuple prefix of it. Confusing the two would deliver one
// room's media to another room's viewers.
func TestPrefixMatchingIsPerField(t *testing.T) {
	announced := ParticipantNamespace("acme", "room-10", Identity{Username: "alice", Device: "d1"})

	if announced.HasPrefix(RoomNamespace("acme", "room-1")) {
		t.Error("room-1 matched as a prefix of room-10")
	}
	if !announced.HasPrefix(RoomNamespace("acme", "room-10")) {
		t.Error("the room's own prefix did not match")
	}
}

// A roster entry is proof that a participant announced, and the proof is a
// suffix of exactly two fields. Anything else is a track or a prefix, and
// admitting either would put things in the roster that are not people.
func TestIdentityNeedsExactlyTwoTrailingFields(t *testing.T) {
	room := RoomNamespace("acme", "room-42")

	id, ok := IdentityIn(Namespace{"acme", "r", "room-42", "alice", "d1"}, room)
	if !ok {
		t.Fatal("a participant announcement was not recognised")
	}
	if id.Username != "alice" || id.Device != "d1" {
		t.Errorf("recovered %v", id)
	}

	if _, ok := IdentityIn(Namespace{"acme", "r", "room-42", "alice"}, room); ok {
		t.Error("a one-field suffix was read as an identity")
	}
	if _, ok := IdentityIn(Namespace{"acme", "r", "room-42", "alice", "d1", "video"}, room); ok {
		t.Error("a track under a participant was read as an identity")
	}
	if _, ok := IdentityIn(Namespace{"other", "r", "room-42", "alice", "d1"}, room); ok {
		t.Error("an announcement from another tenant matched")
	}
}

func TestNamespaceLimitsAreEnforced(t *testing.T) {
	tooMany := make(Namespace, MaxNamespaceFields+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	if err := tooMany.Validate(); err == nil {
		t.Error("a namespace with too many fields was accepted")
	}

	tooBig := Namespace{strings.Repeat("x", MaxFieldBytes+1)}
	if err := tooBig.Validate(); err == nil {
		t.Error("an oversized field was accepted")
	}
}

// A dual-stack relay reports an IPv4 client as ::ffff:203.0.113.9, so a ban
// written the way a human writes it has to match that form. Getting this wrong
// fails permissively, which is the wrong direction for a ban.
func TestAddressBansMatchAcrossFamilies(t *testing.T) {
	now := time.Now()
	ban := Ban{Kind: BanByAddress, IPPrefix: "203.0.113.0/24"}

	for _, addr := range []string{
		"203.0.113.9",
		"203.0.113.9:51234",
		"::ffff:203.0.113.9",
		"[::ffff:203.0.113.9]:51234",
	} {
		if !ban.Matches(Identity{}, addr, now) {
			t.Errorf("%s was not matched by %s", addr, ban.IPPrefix)
		}
	}
	if ban.Matches(Identity{}, "198.51.100.4", now) {
		t.Error("an address outside the prefix was matched")
	}
}

func TestBanScopeAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	alice := Identity{Username: "alice", Device: "d1"}
	aliceOther := Identity{Username: "alice", Device: "d2"}

	byUser := Ban{Kind: BanByUsername, Username: "alice"}
	if !byUser.Matches(alice, "", now) || !byUser.Matches(aliceOther, "", now) {
		t.Error("a username ban did not cover every device")
	}

	byDevice := Ban{Kind: BanByDevice, Username: "alice", Device: "d1"}
	if !byDevice.Matches(alice, "", now) {
		t.Error("a device ban did not match its device")
	}
	if byDevice.Matches(aliceOther, "", now) {
		t.Error("a device ban covered another device")
	}

	expired := Ban{Kind: BanByUsername, Username: "alice", ExpiresAt: now.Add(-time.Second)}
	if expired.Matches(alice, "", now) {
		t.Error("an expired ban still matched")
	}
	permanent := Ban{Kind: BanByUsername, Username: "alice"}
	if !permanent.Matches(alice, "", now.Add(100*365*24*time.Hour)) {
		t.Error("a permanent ban expired")
	}
}

// A malformed prefix must be refused at the edge. Accepted, it would match
// nothing and the API would report a ban that does not exist -- the worst
// outcome, because the moderator stops watching.
func TestMalformedBanSubjectsAreRefused(t *testing.T) {
	if err := ValidateBanSubject(Ban{Kind: BanByAddress, IPPrefix: "203.0.113.0/33"}); err == nil {
		t.Error("an impossible CIDR prefix was accepted")
	}
	if err := ValidateBanSubject(Ban{Kind: BanByAddress, IPPrefix: "not-an-address"}); err == nil {
		t.Error("a non-address was accepted")
	}
	if err := ValidateBanSubject(Ban{Kind: BanByAddress, IPPrefix: "203.0.113.9"}); err != nil {
		t.Errorf("a bare address was refused: %v", err)
	}
	if err := ValidateBanSubject(Ban{Kind: BanByDevice, Username: "alice"}); err == nil {
		t.Error("a device ban with no device was accepted")
	}
}

// Audience-only mutes are excluded from the effective set, or a cohost would be
// told their guest had gone silent while hearing them perfectly well.
func TestEffectiveMuteExcludesAudienceOnly(t *testing.T) {
	state := MuteState{
		SelfMuted:         MediaVideo,
		ForcedForEveryone: MediaAudio,
		ForcedForAudience: MediaScreenShare,
	}
	got := state.Effective()

	if !got.Has(MediaAudio) || !got.Has(MediaVideo) {
		t.Errorf("effective mute lost a kind: %b", got)
	}
	if got.Has(MediaScreenShare) {
		t.Error("an audience-only mute appeared in the effective set")
	}
}

func TestResidencyCannotChange(t *testing.T) {
	current := Policy{ResidencyZone: "eu"}.WithDefaults()

	if err := current.ValidateChange(Policy{ResidencyZone: "us"}.WithDefaults()); err == nil {
		t.Error("residency was allowed to change")
	}
	if err := current.ValidateChange(Policy{ResidencyZone: "eu", MaxParticipants: 10}.WithDefaults()); err != nil {
		t.Errorf("an unrelated policy change was refused: %v", err)
	}
}

func TestPolicyDefaultsAreTheDocumentedLimits(t *testing.T) {
	policy := Policy{}.WithDefaults()

	if policy.StageSlots != DefaultStageSlots {
		t.Errorf("stage slots default to %d, want %d", policy.StageSlots, DefaultStageSlots)
	}
	if policy.WatchdogLimit != DefaultWatchdogLimit {
		t.Errorf("watchdog limit defaults to %d, want %d", policy.WatchdogLimit, DefaultWatchdogLimit)
	}
	if policy.Recording != RecordingAllowed {
		t.Errorf("recording defaults to %q", policy.Recording)
	}
	if grant := (Policy{}).Direct(); !grant.Allowed || grant.MaxParticipants != MaxDirectParticipants {
		t.Errorf("direct paths default to %+v", grant)
	}
	if grant := (Policy{DirectPaths: DirectRelayOnly, MaxDirectParticipants: 2}).Direct(); grant.Allowed {
		t.Errorf("a relay-only room grants %+v", grant)
	}
	for _, bad := range []Policy{{MaxDirectParticipants: 1}, {MaxDirectParticipants: 5}, {DirectPaths: "sometimes"}} {
		if bad.ValidateDirect() == nil {
			t.Errorf("%+v validated", bad)
		}
	}
}
