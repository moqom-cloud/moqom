// Package mls is the delivery service for MoqomKit's MLS-managed end-to-end
// encryption: it holds key packages and puts each room's commits in one order.
//
// # What it sees, and what it never sees
//
// Key packages, which are signed public keys. Commits, whose only secret is an
// HPKE ciphertext to each member. Welcomes, whose secrets are HPKE ciphertexts
// to the joiners' init keys and whose group information is AEAD-sealed under a
// key derived from the joiner secret. Nothing here, and nothing the control
// plane stores, derives a media key; a compromise of this service can withhold
// or reorder handshake messages, and every member would notice a fork through a
// failed confirmation tag, but it cannot decrypt anything.
//
// What it does decide is admission, and that is deliberate. A key package is
// accepted only from a token whose subject and device are the identity inside
// it, so a member's view of who is in the group is exactly the control plane's
// view of who joined the room. A compromised operator could therefore mint a
// token and add a device; every member sees it in the roster, and the epoch
// authenticator is the value two people compare to rule it out.
//
// # Why HTTP on the join listener
//
// Three carriers were possible. The admin gRPC API is reached over mutual TLS
// from a tenant's backend and never from a phone, so it is the wrong door.
// MOQT tracks on the relay would need the relay to persist key packages, order
// commits across relays in different regions and enforce one-time use -- all
// of which is control-plane state, and the relay is deliberately stateless
// about rooms beyond what the control plane pushes it. The join listener is
// already the one internet-facing endpoint a device talks to before it dials,
// with the certificate devices already pin, beside the store that knows rooms.
// So the delivery service is a handful of JSON routes there, authenticated by
// the participant token the relay also accepts. A long poll stands in for
// push: a device asks what has happened since its cursor and the request is
// held until something has, which costs one idle connection per member and no
// new protocol.
//
// The rules a client relies on are all here and in Service: one group per
// room, in the cipher suite its founder named; a device's key packages are all
// in one suite, and one in a suite other than the group's is not handed out;
// a commit is accepted only for the room's current epoch, and the first
// one wins; a key package is handed out once, except the last-resort one; a
// Welcome is only ever returned to the devices it names; a room nobody in the
// group has been heard from for OnlineWindow drops its group so the next
// device can found a fresh one.
package mls

import (
	"context"
	"errors"
	"time"
)

// Failures a caller distinguishes.
var (
	ErrNotFound  = errors.New("mls: not found")
	ErrConflict  = errors.New("mls: already exists")
	ErrStale     = errors.New("mls: commit is not for the current epoch")
	ErrInvalid   = errors.New("mls: invalid")
	ErrForbidden = errors.New("mls: not permitted")
)

// Room names one room's group.
type Room struct {
	Tenant string
	Room   string
}

// Caller is who a request is from, as the token says.
type Caller struct {
	Room
	// Device is the control plane's identity string, `username/device`, which
	// is also the identity inside the device's key packages.
	Device string
	// Viewer is a device that may not publish and is not an agent. It is
	// sealed the group's keys as audience and never added to the group.
	Viewer bool
}

// Group is the room's current group.
type Group struct {
	ID    []byte
	Epoch uint64
	// CipherSuite is the suite its founder declared. The service cannot check
	// it against the group's own GroupContext, which it never sees, but it is
	// what tells a device which suite of key packages to publish, and a
	// package in any other is not handed out.
	CipherSuite CipherSuite
}

// MessageKind is what a sequenced message is.
type MessageKind string

const (
	KindCommit  MessageKind = "commit"
	KindWelcome MessageKind = "welcome"
)

// Message is one entry in a room's log.
type Message struct {
	Seq   uint64
	Kind  MessageKind
	Epoch uint64
	// Sender is the device that submitted it.
	Sender string
	// Recipients is who a Welcome is for. Empty for a commit, which is for
	// everybody.
	Recipients []string
	Data       []byte
	At         time.Time
}

// Commit is a commit as submitted, with the bookkeeping that comes with it.
type Commit struct {
	GroupID []byte
	Epoch   uint64
	Sender  string
	Data    []byte
	Welcome []byte
	Joiners []string
	Removed []string
}

// Presence is what the store knows about one device in a room.
type Presence struct {
	Device         string
	Member         bool
	LastSeen       time.Time
	HasKeyPackages bool
}

// Store is where the delivery service keeps its state.
//
// Every method is one atomic step; the service composes them and the ordering
// rule lives in AppendCommit, which is the one place two members can race.
type Store interface {
	// PutKeyPackages replaces a device's packages for a room.
	PutKeyPackages(ctx context.Context, room Room, device string, oneTime [][]byte, lastResort []byte) error
	// ClaimKeyPackage removes and returns one one-time package, or returns the
	// last-resort one without removing it. ErrNotFound if there is neither.
	ClaimKeyPackage(ctx context.Context, room Room, device string) ([]byte, error)

	// Touch records that a device was heard from.
	Touch(ctx context.Context, room Room, device string, at time.Time) error
	// SetMember marks a device in or out of the group without touching it.
	SetMember(ctx context.Context, room Room, device string, member bool) error
	// Leave forgets a device and its packages in a room.
	Leave(ctx context.Context, room Room, device string) error
	Presence(ctx context.Context, room Room) ([]Presence, error)

	// Group is the room's current group, ErrNotFound if it has none.
	Group(ctx context.Context, room Room) (Group, error)
	// CreateGroup founds a group in suite at epoch 0 with founder as its only
	// member. ErrConflict if the room has one.
	CreateGroup(ctx context.Context, room Room, id []byte, suite CipherSuite, founder string, at time.Time) (Group, error)
	// DropGroup removes the group if its ID is still id, with its log and
	// every device's membership. Sequence numbers are not reused afterwards.
	DropGroup(ctx context.Context, room Room, id []byte) error
	// AppendCommit orders a commit and its Welcome, advances the epoch, and
	// applies the membership bookkeeping, all or nothing. ErrStale unless the
	// commit is for the current group and epoch.
	AppendCommit(ctx context.Context, room Room, commit Commit, at time.Time) (uint64, error)
	// Messages returns up to limit entries after `after` that device may see,
	// and the cursor to ask from next.
	Messages(ctx context.Context, room Room, after uint64, device string, limit int) ([]Message, uint64, error)

	// PutAudienceKey records a viewer's public key and that it was heard from.
	PutAudienceKey(ctx context.Context, room Room, device string, key []byte, at time.Time) error
	// LeaveAudience forgets a viewer and its envelope.
	LeaveAudience(ctx context.Context, room Room, device string) error
	// Audience lists a room's viewers.
	Audience(ctx context.Context, room Room) ([]AudienceSeat, error)
	// AudienceCount counts the viewers heard from since `since`, and of those
	// the ones whose newest envelope is not for ref.
	AudienceCount(ctx context.Context, room Room, ref EpochRef, since time.Time) (present, waiting int, err error)
	// PutEnvelopes replaces each named viewer's envelope. Envelopes for devices
	// that are not viewers are dropped.
	PutEnvelopes(ctx context.Context, room Room, envelopes []Envelope) error
	// Envelope is a viewer's newest envelope, ErrNotFound if it has none.
	Envelope(ctx context.Context, room Room, device string) (Envelope, error)
}
