package control

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits from draft-19's namespace tuple, which is what these become on the
// wire. Enforced here rather than at the edge of the relay because a name that
// cannot be announced is not a name, and the earliest place to say so is when a
// backend first supplies it.
const (
	MaxNamespaceFields = 32
	MaxNamespaceBytes  = 4096
	MaxFieldBytes      = 4096

	// DeviceIDLength is how much of the key thumbprint becomes the device ID.
	//
	// Sixteen base64url characters is 96 bits, which is far more than enough to
	// separate one person's handful of devices and short enough to read in a log
	// line. It must match MoqomKit's DeviceID(thumbprint:prefixLength:), because
	// a device ban keyed on one derivation and a session identified by another
	// is a ban that does not land.
	DeviceIDLength = 16
)

var (
	ErrEmptyIdentifier = errors.New("identifier is empty")
	ErrTooLong         = errors.New("identifier is too long")
	ErrControlChar     = errors.New("identifier contains a control character")
)

// Identity is a person on a device.
//
// Both halves are strings the customer already has: the username is their user
// identifier, unmapped, and the device comes from the key in that device's
// secure element. There is no numeric ID and no allocation step, which is the
// property that makes a migration off a platform with opaque UIDs a matter of
// pointing at a new server rather than backfilling a mapping table.
type Identity struct {
	Username string
	Device   string
}

func (i Identity) String() string { return i.Username + "/" + i.Device }

// IsZero reports whether this names nobody.
func (i Identity) IsZero() bool { return i.Username == "" && i.Device == "" }

// NormaliseUsername applies the same normalisation the clients do.
//
// NFC because two byte sequences that display identically must not be two
// accounts: without it, an impostor can register a visually identical name and a
// ban on one does not cover the other. This is a security property wearing the
// costume of a text-encoding detail.
func NormaliseUsername(username string) (string, error) {
	normalised := norm.NFC.String(username)
	if err := validIdentifier(normalised, MaxFieldBytes); err != nil {
		return "", fmt.Errorf("username: %w", err)
	}
	return normalised, nil
}

// DeviceFromThumbprint derives the device ID a token holder will present as.
func DeviceFromThumbprint(thumbprint string) (string, error) {
	if err := validIdentifier(thumbprint, MaxFieldBytes); err != nil {
		return "", fmt.Errorf("device thumbprint: %w", err)
	}
	if len(thumbprint) <= DeviceIDLength {
		return thumbprint, nil
	}
	return thumbprint[:DeviceIDLength], nil
}

func validIdentifier(value string, limit int) error {
	if value == "" {
		return ErrEmptyIdentifier
	}
	if len(value) > limit {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrTooLong, len(value), limit)
	}
	if !utf8.ValidString(value) {
		return errors.New("identifier is not valid UTF-8")
	}
	// Control characters are refused rather than escaped. They have no
	// legitimate use in a username and every use in making one log line look
	// like two.
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ErrControlChar
		}
	}
	return nil
}

// Namespace is a MOQT track namespace: an ordered tuple, not a path.
//
// The distinction matters on the wire. Each field is length-prefixed, so a field
// may contain a slash and no escaping is ever needed; the slash-separated
// rendering here is for people, and is not reversible. Anything that needs the
// fields back must keep them.
type Namespace []string

// Rendered joins the fields the way logs and the admin API show them.
func (n Namespace) Rendered() string { return strings.Join(n, "/") }

// Validate checks the tuple against draft-19's limits.
func (n Namespace) Validate() error {
	if len(n) == 0 {
		return errors.New("namespace has no fields")
	}
	if len(n) > MaxNamespaceFields {
		return fmt.Errorf("namespace has %d fields, limit %d", len(n), MaxNamespaceFields)
	}
	total := 0
	for i, field := range n {
		if err := validIdentifier(field, MaxFieldBytes); err != nil {
			return fmt.Errorf("namespace field %d: %w", i, err)
		}
		total += len(field)
	}
	if total > MaxNamespaceBytes {
		return fmt.Errorf("namespace is %d bytes, limit %d", total, MaxNamespaceBytes)
	}
	return nil
}

// HasPrefix reports whether n starts with prefix, field by field.
//
// Field by field rather than by string comparison, because "room-1" is not a
// prefix of "room-10" as a tuple and is as a string. A subscription that
// confused the two would deliver one room's media to another room's viewers.
func (n Namespace) HasPrefix(prefix Namespace) bool {
	if len(prefix) > len(n) {
		return false
	}
	for i, field := range prefix {
		if n[i] != field {
			return false
		}
	}
	return true
}

// The tags that distinguish one kind of namespace from another. Single
// characters because every announcement carries them and they are compared far
// more often than they are read.
const (
	tagPersonal  = "u"
	tagRoom      = "r"
	tagBackstage = "bs"
)

// RoomNamespace is the prefix every participant in a room announces under.
func RoomNamespace(tenant, room string) Namespace {
	return Namespace{tenant, tagRoom, room}
}

// BackstageNamespace is the sibling prefix for people who are not live yet.
//
// A separate prefix rather than a flag, so viewers subscribed to the room simply
// never see it. Going live is then a change of namespace with no renegotiation
// and nothing for a viewer to be told.
func BackstageNamespace(tenant, room string) Namespace {
	return Namespace{tenant, tagBackstage, room}
}

// ParticipantNamespace is what one device announces.
func ParticipantNamespace(tenant, room string, id Identity) Namespace {
	return Namespace{tenant, tagRoom, room, id.Username, id.Device}
}

// BackstageParticipantNamespace is the same, backstage.
func BackstageParticipantNamespace(tenant, room string, id Identity) Namespace {
	return Namespace{tenant, tagBackstage, room, id.Username, id.Device}
}

// PersonalNamespace is a user's own prefix, outside any room.
func PersonalNamespace(tenant string, id Identity) Namespace {
	return Namespace{tenant, tagPersonal, id.Username, id.Device}
}

// IdentityIn recovers who announced a namespace under prefix.
//
// The suffix must be exactly two fields. Fewer means the announcement is the
// prefix itself; more means something published under a participant's subtree,
// which is a track and not a participant. Accepting either would put things in
// the roster that are not people.
func IdentityIn(announced, prefix Namespace) (Identity, bool) {
	if !announced.HasPrefix(prefix) || len(announced)-len(prefix) != 2 {
		return Identity{}, false
	}
	return Identity{Username: announced[len(prefix)], Device: announced[len(prefix)+1]}, true
}
