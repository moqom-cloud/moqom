package mls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// OnlineWindow is how recently a device must have been heard from to count as
// present.
//
// Three long polls' worth. A device holds a sync open for up to MaxWait and
// asks again at once, so a present device is never quiet for longer than one
// poll plus a round trip; anything quieter than this has gone, and the next
// commit removes it. Shorter and a phone on a bad network is churned out and
// back in; longer and a crashed member keeps the group's committer seat, and
// its leaf key, for longer than it has any right to.
const OnlineWindow = 30 * time.Second

// MaxWait caps how long a sync is held.
const MaxWait = 25 * time.Second

// recheck bounds how stale a held sync can be when the change it is waiting
// for happened on another replica, whose in-process wake-up this one never
// hears.
const recheck = time.Second

// messagesPerSync caps one response. A device further behind than this asks
// again from the cursor it is given.
const messagesPerSync = 256

// maxCommitBytes bounds a commit or a Welcome. A commit carries an HPKE
// ciphertext per member and a Welcome carries every member's leaf, which in
// SuiteMLDSA is about 1,160 and 3,150 bytes (ML-KEM-768's 1,088-byte
// encapsulation; its 1,184-byte public key and ML-DSA-65's 1,952-byte one).
// 3 MiB keeps a room of 800 devices within bounds; past that a room uses
// sender keys.
const maxCommitBytes = 3 << 20

// MessageRetention is how long a store keeps a commit or Welcome, pruned as
// later ones arrive. Far longer than OnlineWindow: every member still in the
// group has read anything this old, and a device that has not is no longer a
// member and rejoins rather than catching up.
const MessageRetention = 10 * time.Minute

// Snapshot is what a sync returns.
type Snapshot struct {
	Group    *Group
	Messages []Message
	// Members is the members heard from within OnlineWindow.
	Members []string
	// Pending is the devices heard from, not in the group, and holding a key
	// package to be added with.
	Pending []string
	Cursor  uint64
}

// Service is the delivery service's rules over a store.
type Service struct {
	store Store
	now   func() time.Time

	mu      sync.Mutex
	waiters map[Room]chan struct{}
}

// NewService builds a service. now defaults to the wall clock.
func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now, waiters: map[Room]chan struct{}{}}
}

// changed wakes every sync held on a room.
func (s *Service) changed(room Room) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if waiting, ok := s.waiters[room]; ok {
		close(waiting)
		delete(s.waiters, room)
	}
}

func (s *Service) waitFor(room Room) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiting, ok := s.waiters[room]
	if !ok {
		waiting = make(chan struct{})
		s.waiters[room] = waiting
	}
	return waiting
}

// PublishKeyPackages replaces the caller's packages for its room.
//
// Every package must parse, verify, and name the caller. That last check is
// what binds a leaf in the group to a device the control plane admitted: the
// token says who the caller is, and a package claiming to be anyone else is a
// device trying to join under another's name.
//
// All of a device's packages are in one cipher suite, so whichever is claimed
// is in the suite the device last chose. They are not checked against the
// room's group: a device publishes before it has seen whether there is one,
// and replaces its packages when the group turns out to be in another suite.
func (s *Service) PublishKeyPackages(ctx context.Context, caller Caller, oneTime [][]byte, lastResort []byte) error {
	if len(oneTime) > maxOneTimePackages {
		return fmt.Errorf("%w: %d key packages, at most %d", ErrInvalid, len(oneTime), maxOneTimePackages)
	}
	var suite CipherSuite
	for i, data := range append(slices.Clone(oneTime), lastResort) {
		parsed, err := ParseKeyPackage(data)
		if err != nil {
			return err
		}
		if parsed.Identity != caller.Device {
			return fmt.Errorf("%w: key package is for %q, token is for %q", ErrForbidden, parsed.Identity, caller.Device)
		}
		if i == 0 {
			suite = parsed.CipherSuite
		} else if parsed.CipherSuite != suite {
			return fmt.Errorf("%w: key packages in suites %#04x and %#04x", ErrInvalid, uint16(suite), uint16(parsed.CipherSuite))
		}
	}
	if err := s.store.PutKeyPackages(ctx, caller.Room, caller.Device, oneTime, lastResort); err != nil {
		return err
	}
	if err := s.store.Touch(ctx, caller.Room, caller.Device, s.now()); err != nil {
		return err
	}
	s.changed(caller.Room)
	return nil
}

// ClaimKeyPackage hands a member one of a pending device's packages.
//
// Only to a member, and only for a device present in the same room and not
// yet in its group. Key packages are public keys, so this is not secrecy; it
// is so that a device's one-time packages cannot be drained by anyone holding
// a token, which would leave it addable only by its last-resort package.
func (s *Service) ClaimKeyPackage(ctx context.Context, caller Caller, device string) ([]byte, error) {
	presence, err := s.store.Presence(ctx, caller.Room)
	if err != nil {
		return nil, err
	}
	var callerIsMember, targetIsPending bool
	for _, entry := range presence {
		switch entry.Device {
		case caller.Device:
			callerIsMember = entry.Member
		case device:
			targetIsPending = !entry.Member && s.online(entry)
		}
	}
	if !callerIsMember {
		return nil, fmt.Errorf("%w: only a member adds", ErrForbidden)
	}
	if !targetIsPending {
		return nil, fmt.Errorf("%w: %q is not waiting to be added", ErrNotFound, device)
	}
	group, err := s.store.Group(ctx, caller.Room)
	if err != nil {
		return nil, err
	}
	data, err := s.store.ClaimKeyPackage(ctx, caller.Room, device)
	if err != nil {
		return nil, err
	}
	// Checked after claiming, since the store hands out packages without
	// reading them. A one-time package spent this way could never have joined
	// this group, and its device replaces the lot once it sees the group's
	// suite.
	parsed, err := ParseKeyPackage(data)
	if err != nil {
		return nil, err
	}
	if parsed.CipherSuite != group.CipherSuite {
		return nil, fmt.Errorf("%w: %q's key package is in suite %#04x, the group is in %#04x",
			ErrConflict, device, uint16(parsed.CipherSuite), uint16(group.CipherSuite))
	}
	return data, nil
}

func (s *Service) online(entry Presence) bool {
	return s.now().Sub(entry.LastSeen) <= OnlineWindow
}

// CreateGroup founds the room's group in suite with the caller as its only
// member. Only FoundingSuite is accepted: groups already founded in an older
// suite keep working, but no new one is classical in its signatures or its KEM.
func (s *Service) CreateGroup(ctx context.Context, caller Caller, id []byte, suite CipherSuite) error {
	if len(id) == 0 || len(id) > 64 {
		return fmt.Errorf("%w: group id of %d bytes", ErrInvalid, len(id))
	}
	if suite != FoundingSuite {
		return fmt.Errorf("%w: groups are founded in cipher suite %#04x, not %#04x", ErrInvalid, uint16(FoundingSuite), uint16(suite))
	}
	if err := s.store.Touch(ctx, caller.Room, caller.Device, s.now()); err != nil {
		return err
	}
	if _, err := s.store.CreateGroup(ctx, caller.Room, id, suite, caller.Device, s.now()); err != nil {
		return err
	}
	s.changed(caller.Room)
	return nil
}

// CommitRequest is a commit as a device submits it.
type CommitRequest struct {
	Epoch   uint64
	Commit  []byte
	Welcome []byte
	Joiners []string
	Removed []string
}

// SubmitCommit orders a commit, or refuses it as stale.
//
// The header is checked against the request and the room's group so a commit
// for one group cannot be filed under another's epoch. Its signature and
// confirmation tag are not checked here: they need the group's keys, which the
// service does not have, and every member checks them before applying it.
func (s *Service) SubmitCommit(ctx context.Context, caller Caller, request CommitRequest) (uint64, error) {
	if len(request.Commit) > maxCommitBytes || len(request.Welcome) > maxCommitBytes {
		return 0, fmt.Errorf("%w: commit too large", ErrInvalid)
	}
	if (len(request.Welcome) == 0) != (len(request.Joiners) == 0) {
		return 0, fmt.Errorf("%w: a Welcome needs joiners, and joiners need a Welcome", ErrInvalid)
	}
	header, err := ParseCommitHeader(request.Commit)
	if err != nil {
		return 0, err
	}
	if header.Epoch != request.Epoch {
		return 0, fmt.Errorf("%w: commit says epoch %d, request says %d", ErrInvalid, header.Epoch, request.Epoch)
	}
	presence, err := s.store.Presence(ctx, caller.Room)
	if err != nil {
		return 0, err
	}
	if !slices.ContainsFunc(presence, func(p Presence) bool { return p.Device == caller.Device && p.Member }) {
		return 0, fmt.Errorf("%w: only a member commits", ErrForbidden)
	}
	group, err := s.store.Group(ctx, caller.Room)
	if errors.Is(err, ErrNotFound) {
		return 0, ErrStale
	} else if err != nil {
		return 0, err
	}
	if !bytes.Equal(group.ID, header.GroupID) {
		return 0, fmt.Errorf("%w: commit is for another group", ErrStale)
	}

	seq, err := s.store.AppendCommit(ctx, caller.Room, Commit{
		GroupID: header.GroupID,
		Epoch:   header.Epoch,
		Sender:  caller.Device,
		Data:    request.Commit,
		Welcome: request.Welcome,
		Joiners: request.Joiners,
		Removed: request.Removed,
	}, s.now())
	if err != nil {
		return 0, err
	}
	s.changed(caller.Room)
	return seq, nil
}

// Rejoin marks the caller as needing to be added again.
func (s *Service) Rejoin(ctx context.Context, caller Caller) error {
	if err := s.store.Touch(ctx, caller.Room, caller.Device, s.now()); err != nil {
		return err
	}
	if err := s.store.SetMember(ctx, caller.Room, caller.Device, false); err != nil {
		return err
	}
	s.changed(caller.Room)
	return nil
}

// Leave forgets the caller, so the next commit removes it.
func (s *Service) Leave(ctx context.Context, caller Caller) error {
	if err := s.store.Leave(ctx, caller.Room, caller.Device); err != nil {
		return err
	}
	s.changed(caller.Room)
	return nil
}

// Sync returns what has happened after `after`, holding the request up to
// wait for something to.
//
// It returns at once when there is something for the caller to act on: new
// messages, no group (so the caller should found one), or, for a member,
// somebody to add or remove. A member that is not the committer will be told
// the same thing again on every sync until the committer acts, which is why
// the client paces itself between syncs that brought no messages.
func (s *Service) Sync(ctx context.Context, caller Caller, after uint64, wait time.Duration) (Snapshot, error) {
	wait = min(max(wait, 0), MaxWait)
	deadline := s.now().Add(wait)
	if err := s.store.Touch(ctx, caller.Room, caller.Device, s.now()); err != nil {
		return Snapshot{}, err
	}
	for {
		// Taken before reading, so a change between the read and the wait still
		// wakes this request.
		woken := s.waitFor(caller.Room)
		snapshot, actionable, err := s.snapshot(ctx, caller, after)
		if err != nil || actionable {
			return snapshot, err
		}
		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			return snapshot, nil
		}
		timer := time.NewTimer(min(remaining, recheck))
		select {
		case <-ctx.Done():
			timer.Stop()
			return snapshot, nil
		case <-woken:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (s *Service) snapshot(ctx context.Context, caller Caller, after uint64) (Snapshot, bool, error) {
	presence, err := s.store.Presence(ctx, caller.Room)
	if err != nil {
		return Snapshot{}, false, err
	}
	group, err := s.store.Group(ctx, caller.Room)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return Snapshot{}, false, err
	default:
		// A group with no member left to add anybody is a group nobody can
		// join. Dropped, so the device asking founds a new one; a member that
		// comes back later sees a different group ID and asks to be added.
		if !slices.ContainsFunc(presence, func(p Presence) bool { return p.Member && s.online(p) }) {
			if err := s.store.DropGroup(ctx, caller.Room, group.ID); err != nil {
				return Snapshot{}, false, err
			}
			err = ErrNotFound
			for i := range presence {
				presence[i].Member = false
			}
		}
	}
	var current *Group
	if err == nil {
		current = &group
	}

	messages, cursor, err := s.store.Messages(ctx, caller.Room, after, caller.Device, messagesPerSync)
	if err != nil {
		return Snapshot{}, false, err
	}
	snapshot := Snapshot{Group: current, Messages: messages, Cursor: cursor, Members: []string{}, Pending: []string{}}
	callerIsMember, work := false, false
	for _, entry := range presence {
		online := s.online(entry)
		switch {
		case entry.Member && online:
			snapshot.Members = append(snapshot.Members, entry.Device)
			if entry.Device == caller.Device {
				callerIsMember = true
			}
		case entry.Member && current != nil:
			work = true // somebody to remove
		case !entry.Member && online && entry.HasKeyPackages && current != nil:
			snapshot.Pending = append(snapshot.Pending, entry.Device)
			if entry.Device != caller.Device {
				work = true // somebody to add
			}
		}
	}
	slices.Sort(snapshot.Members)
	slices.Sort(snapshot.Pending)
	actionable := len(messages) > 0 || current == nil || (callerIsMember && work)
	return snapshot, actionable, nil
}
