package mls

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"time"
)

// Memory is a Store in this process, for a laptop and for tests.
//
// It loses everything on restart, which for this state is an inconvenience
// rather than a loss: every device notices its group has gone, and the room
// founds a new one.
type Memory struct {
	mu    sync.Mutex
	rooms map[Room]*memoryRoom
}

type memoryRoom struct {
	group    *Group
	lastSeq  uint64
	messages []Message
	devices  map[string]*memoryDevice
	audience map[string]*memoryViewer
}

type memoryDevice struct {
	member     bool
	lastSeen   time.Time
	oneTime    [][]byte
	lastResort []byte
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{rooms: map[Room]*memoryRoom{}} }

func (m *Memory) room(room Room) *memoryRoom {
	found, ok := m.rooms[room]
	if !ok {
		found = &memoryRoom{devices: map[string]*memoryDevice{}}
		m.rooms[room] = found
	}
	return found
}

func (r *memoryRoom) device(device string) *memoryDevice {
	found, ok := r.devices[device]
	if !ok {
		found = &memoryDevice{}
		r.devices[device] = found
	}
	return found
}

func clone(data []byte) []byte { return append([]byte(nil), data...) }

func (m *Memory) PutKeyPackages(_ context.Context, room Room, device string, oneTime [][]byte, lastResort []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.room(room).device(device)
	entry.oneTime = nil
	for _, data := range oneTime {
		entry.oneTime = append(entry.oneTime, clone(data))
	}
	entry.lastResort = clone(lastResort)
	return nil
}

func (m *Memory) ClaimKeyPackage(_ context.Context, room Room, device string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.room(room).devices[device]
	switch {
	case !ok:
		return nil, ErrNotFound
	case len(entry.oneTime) > 0:
		first := entry.oneTime[0]
		entry.oneTime = entry.oneTime[1:]
		return clone(first), nil
	case len(entry.lastResort) > 0:
		return clone(entry.lastResort), nil
	default:
		return nil, ErrNotFound
	}
}

func (m *Memory) Touch(_ context.Context, room Room, device string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.room(room).device(device).lastSeen = at
	return nil
}

func (m *Memory) SetMember(_ context.Context, room Room, device string, member bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.room(room).device(device).member = member
	return nil
}

func (m *Memory) Leave(_ context.Context, room Room, device string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.room(room).devices, device)
	return nil
}

func (m *Memory) Presence(_ context.Context, room Room) ([]Presence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Presence
	for name, entry := range m.room(room).devices {
		out = append(out, Presence{
			Device:         name,
			Member:         entry.member,
			LastSeen:       entry.lastSeen,
			HasKeyPackages: len(entry.oneTime) > 0 || len(entry.lastResort) > 0,
		})
	}
	slices.SortFunc(out, func(a, b Presence) int {
		return bytes.Compare([]byte(a.Device), []byte(b.Device))
	})
	return out, nil
}

func (m *Memory) Group(_ context.Context, room Room) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := m.room(room).group
	if found == nil {
		return Group{}, ErrNotFound
	}
	return Group{ID: clone(found.ID), Epoch: found.Epoch, CipherSuite: found.CipherSuite}, nil
}

func (m *Memory) CreateGroup(_ context.Context, room Room, id []byte, suite CipherSuite, founder string, at time.Time) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.room(room)
	if entry.group != nil {
		return Group{}, ErrConflict
	}
	entry.group = &Group{ID: clone(id), Epoch: 0, CipherSuite: suite}
	founding := entry.device(founder)
	founding.member = true
	founding.lastSeen = at
	return Group{ID: clone(id), CipherSuite: suite}, nil
}

func (m *Memory) DropGroup(_ context.Context, room Room, id []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.room(room)
	if entry.group == nil || !bytes.Equal(entry.group.ID, id) {
		return nil
	}
	entry.group = nil
	entry.messages = nil
	for _, device := range entry.devices {
		device.member = false
	}
	return nil
}

func (m *Memory) AppendCommit(_ context.Context, room Room, commit Commit, at time.Time) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.room(room)
	if entry.group == nil || !bytes.Equal(entry.group.ID, commit.GroupID) || entry.group.Epoch != commit.Epoch {
		return 0, ErrStale
	}
	entry.messages = slices.DeleteFunc(entry.messages, func(message Message) bool {
		return message.At.Before(at.Add(-MessageRetention))
	})
	entry.lastSeq++
	seq := entry.lastSeq
	entry.messages = append(entry.messages, Message{
		Seq: seq, Kind: KindCommit, Epoch: commit.Epoch, Sender: commit.Sender, Data: clone(commit.Data), At: at,
	})
	if len(commit.Welcome) > 0 {
		entry.lastSeq++
		entry.messages = append(entry.messages, Message{
			Seq: entry.lastSeq, Kind: KindWelcome, Epoch: commit.Epoch + 1, Sender: commit.Sender,
			Recipients: slices.Clone(commit.Joiners), Data: clone(commit.Welcome), At: at,
		})
	}
	entry.group.Epoch++
	// Removed before joined, so a device replaced in the same commit -- its
	// stale leaf out, its new one in -- ends up a member.
	for _, gone := range commit.Removed {
		if device, ok := entry.devices[gone]; ok {
			device.member = false
		}
	}
	for _, joiner := range commit.Joiners {
		entry.device(joiner).member = true
	}
	return seq, nil
}

func (m *Memory) Messages(_ context.Context, room Room, after uint64, device string, limit int) ([]Message, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.room(room)
	out := []Message{}
	for _, message := range entry.messages {
		if message.Seq <= after {
			continue
		}
		if message.Kind == KindWelcome && !slices.Contains(message.Recipients, device) {
			continue
		}
		if len(out) == limit {
			return out, out[len(out)-1].Seq, nil
		}
		copied := message
		copied.Data = clone(message.Data)
		copied.Recipients = slices.Clone(message.Recipients)
		out = append(out, copied)
	}
	return out, entry.lastSeq, nil
}
