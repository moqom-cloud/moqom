package mls

import (
	"context"
	"slices"
	"strings"
	"time"
)

type memoryViewer struct {
	key      []byte
	lastSeen time.Time
	envelope *Envelope
}

func (r *memoryRoom) viewers() map[string]*memoryViewer {
	if r.audience == nil {
		r.audience = map[string]*memoryViewer{}
	}
	return r.audience
}

func (m *Memory) PutAudienceKey(_ context.Context, room Room, device string, key []byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	viewers := m.room(room).viewers()
	viewer, ok := viewers[device]
	if !ok {
		viewer = &memoryViewer{}
		viewers[device] = viewer
	}
	if string(viewer.key) != string(key) {
		// A new key cannot open the old envelope.
		viewer.envelope = nil
	}
	viewer.key = clone(key)
	viewer.lastSeen = at
	return nil
}

func (m *Memory) LeaveAudience(_ context.Context, room Room, device string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.room(room).viewers(), device)
	return nil
}

func (m *Memory) Audience(_ context.Context, room Room) ([]AudienceSeat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []AudienceSeat{}
	for device, viewer := range m.room(room).viewers() {
		seat := AudienceSeat{Device: device, Key: clone(viewer.key), LastSeen: viewer.lastSeen}
		if viewer.envelope != nil {
			ref := EpochRef{GroupID: clone(viewer.envelope.GroupID), Epoch: viewer.envelope.Epoch}
			seat.Sealed = &ref
		}
		out = append(out, seat)
	}
	slices.SortFunc(out, func(a, b AudienceSeat) int { return strings.Compare(a.Device, b.Device) })
	return out, nil
}

func (m *Memory) AudienceCount(_ context.Context, room Room, ref EpochRef, since time.Time) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	present, waiting := 0, 0
	for _, viewer := range m.room(room).viewers() {
		if viewer.lastSeen.Before(since) {
			continue
		}
		present++
		if viewer.envelope == nil || !viewer.envelope.EpochRef.Equal(ref) {
			waiting++
		}
	}
	return present, waiting, nil
}

func (m *Memory) PutEnvelopes(_ context.Context, room Room, envelopes []Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	viewers := m.room(room).viewers()
	for _, envelope := range envelopes {
		viewer, ok := viewers[envelope.Device]
		if !ok {
			continue
		}
		viewer.envelope = &Envelope{
			Device:   envelope.Device,
			EpochRef: EpochRef{GroupID: clone(envelope.GroupID), Epoch: envelope.Epoch},
			Data:     clone(envelope.Data),
		}
	}
	return nil
}

func (m *Memory) Envelope(_ context.Context, room Room, device string) (Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	viewer, ok := m.room(room).viewers()[device]
	if !ok || viewer.envelope == nil {
		return Envelope{}, ErrNotFound
	}
	found := *viewer.envelope
	found.GroupID = clone(found.GroupID)
	found.Data = clone(found.Data)
	return found, nil
}
