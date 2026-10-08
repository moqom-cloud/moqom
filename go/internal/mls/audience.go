package mls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
)

// Sender keys, for an audience too large for the group.
//
// A room's group holds the devices that may publish, and its agents: tens of
// members, each of whom every other can see. A broadcast's audience is
// thousands, and adding each to the group would make every commit carry an
// HPKE ciphertext per viewer and every join a commit. So viewers are not
// members. Each posts an HPKE public key (ML-KEM-768, the suite's KEM); a group
// member — the committer, by the group's own rule — seals the epoch's SFrame
// base key to each viewer that does not yet have it, and the viewer derives any
// publisher's key from that base key and the key ID in the frame. The group
// rotates on a timer while it has an audience, so a viewer who has gone stops
// being able to read within one rotation.
//
// What this service sees is unchanged in kind: public keys, and ciphertexts
// only the viewer can open. What it decides is again admission — a viewer's key
// is accepted from the token whose device it is — and here that decision is
// not visible to every member the way a leaf is. The committer seals to the
// list it is given, so a compromised control plane could add a viewer nobody
// sees. That is the trade a broadcast makes, and why anybody who may publish is
// a member instead.

// AudienceKeyBytes is the length of an ML-KEM-768 public key.
const AudienceKeyBytes = 1184

// maxEnvelopeBytes bounds one sealed epoch key: a 1,088-byte encapsulation, a
// 32-byte key with its 16-byte tag, and the framing.
const maxEnvelopeBytes = 2048

// maxEnvelopesPerSeal bounds one SealAudience request.
const maxEnvelopesPerSeal = 1024

// EpochRef names one epoch of one group.
type EpochRef struct {
	GroupID []byte
	Epoch   uint64
}

// Equal reports whether two refer to the same epoch.
func (e EpochRef) Equal(other EpochRef) bool {
	return e.Epoch == other.Epoch && bytes.Equal(e.GroupID, other.GroupID)
}

// Envelope is an epoch's base key sealed to one viewer.
type Envelope struct {
	Device string
	EpochRef
	Data []byte
}

// AudienceSeat is one viewer as the store holds it.
type AudienceSeat struct {
	Device   string
	Key      []byte
	LastSeen time.Time
	// Sealed is the epoch of the newest envelope held for it, if any.
	Sealed *EpochRef
}

// AudienceWork is what a committer seals next: the current epoch, and the
// viewers present that do not yet hold it.
type AudienceWork struct {
	EpochRef
	Seats []AudienceSeat
}

// JoinAudience records the caller's public key as a viewer and returns its
// newest envelope once it is for an epoch other than have, holding the request
// up to wait for one to be sealed.
func (s *Service) JoinAudience(ctx context.Context, caller Caller, key []byte, have *EpochRef, wait time.Duration) (*Envelope, error) {
	if len(key) != AudienceKeyBytes {
		return nil, fmt.Errorf("%w: an audience key is %d bytes, got %d", ErrInvalid, AudienceKeyBytes, len(key))
	}
	// Not s.changed: a viewer arriving is not news a member's held sync needs at
	// once, and in a large room it would wake every member for every viewer.
	// The committer finds it on its next sync.
	if err := s.store.PutAudienceKey(ctx, caller.Room, caller.Device, key, s.now()); err != nil {
		return nil, err
	}
	wait = min(max(wait, 0), MaxWait)
	deadline := s.now().Add(wait)
	for {
		woken := s.waitFor(caller.Room)
		envelope, err := s.store.Envelope(ctx, caller.Room, caller.Device)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, err
		case have == nil || !envelope.EpochRef.Equal(*have):
			return &envelope, nil
		}
		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(min(remaining, recheck))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil
		case <-woken:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// LeaveAudience forgets the caller as a viewer.
func (s *Service) LeaveAudience(ctx context.Context, caller Caller) error {
	return s.store.LeaveAudience(ctx, caller.Room, caller.Device)
}

// AudienceWork lists the viewers the caller should seal the current epoch to.
// Only a member of the group may ask, since only a member can seal.
func (s *Service) AudienceWork(ctx context.Context, caller Caller) (AudienceWork, error) {
	current, err := s.memberGroup(ctx, caller)
	if err != nil {
		return AudienceWork{}, err
	}
	seats, err := s.store.Audience(ctx, caller.Room)
	if err != nil {
		return AudienceWork{}, err
	}
	ref := EpochRef{GroupID: current.ID, Epoch: current.Epoch}
	work := AudienceWork{EpochRef: ref, Seats: []AudienceSeat{}}
	for _, seat := range seats {
		if s.now().Sub(seat.LastSeen) > OnlineWindow {
			continue
		}
		if seat.Sealed != nil && seat.Sealed.Equal(ref) {
			continue
		}
		work.Seats = append(work.Seats, seat)
	}
	return work, nil
}

// SealAudience stores envelopes for the current epoch. ErrStale if the group
// has moved on, in which case the committer seals the new epoch instead.
func (s *Service) SealAudience(ctx context.Context, caller Caller, ref EpochRef, sealed map[string][]byte) error {
	current, err := s.memberGroup(ctx, caller)
	if err != nil {
		return err
	}
	if !bytes.Equal(current.ID, ref.GroupID) || current.Epoch != ref.Epoch {
		return ErrStale
	}
	if len(sealed) > maxEnvelopesPerSeal {
		return fmt.Errorf("%w: %d envelopes in one request, limit %d", ErrInvalid, len(sealed), maxEnvelopesPerSeal)
	}
	envelopes := make([]Envelope, 0, len(sealed))
	for device, data := range sealed {
		if len(data) == 0 || len(data) > maxEnvelopeBytes {
			return fmt.Errorf("%w: an envelope for %s is %d bytes", ErrInvalid, device, len(data))
		}
		envelopes = append(envelopes, Envelope{Device: device, EpochRef: ref, Data: data})
	}
	if err := s.store.PutEnvelopes(ctx, caller.Room, envelopes); err != nil {
		return err
	}
	s.changed(caller.Room)
	return nil
}

// memberGroup is the room's group, if the caller is an online member of it.
func (s *Service) memberGroup(ctx context.Context, caller Caller) (Group, error) {
	presence, err := s.store.Presence(ctx, caller.Room)
	if err != nil {
		return Group{}, err
	}
	member := false
	for _, entry := range presence {
		if entry.Device == caller.Device && entry.Member && s.online(entry) {
			member = true
		}
	}
	if !member {
		return Group{}, fmt.Errorf("%w: only a member of the group seals for its audience", ErrForbidden)
	}
	return s.store.Group(ctx, caller.Room)
}
