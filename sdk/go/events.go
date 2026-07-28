package moqom

import (
	"context"
	"errors"
	"io"
	"iter"
	"time"

	"google.golang.org/grpc"

	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// EventKind is what happened.
type EventKind string

const (
	EventParticipantJoined EventKind = "participant_joined"
	EventParticipantLeft   EventKind = "participant_left"
	EventPresenceChanged   EventKind = "presence_changed"
	EventGrantsChanged     EventKind = "grants_changed"
	EventMuteChanged       EventKind = "mute_changed"
	EventSpotlightChanged  EventKind = "spotlight_changed"
	EventPublishStarted    EventKind = "publish_started"
	EventPublishStopped    EventKind = "publish_stopped"
	EventWatchdogChanged   EventKind = "watchdog_changed"
	EventLifecycleChanged  EventKind = "lifecycle_changed"
	EventModerationApplied EventKind = "moderation_applied"
)

// Event is one thing that happened in a room.
//
// Exactly one of the body fields is set, and Kind says which. A switch on Kind that
// forgets a case gets a zero body rather than a wrong one.
type Event struct {
	Tenant string
	Room   string

	// Sequence is contiguous within a room and never reused. Persist it: a
	// consumer that resumes from it can tell it missed something, which a
	// timestamp cannot, and a gap is worth alerting on rather than papering over.
	Sequence uint64

	At   time.Time
	Kind EventKind

	// Joined is set for EventParticipantJoined.
	Joined *ParticipantJoined
	// Left is set for EventParticipantLeft.
	Left *ParticipantLeft
	// PresenceChanged is set for EventPresenceChanged.
	PresenceChanged *PresenceChanged
	// GrantsChanged is set for EventGrantsChanged.
	GrantsChanged *GrantsChanged
	// MuteChanged is set for EventMuteChanged.
	MuteChanged *MuteChanged
	// SpotlightChanged is set for EventSpotlightChanged. Its Identity is empty
	// when the spotlight was cleared.
	SpotlightChanged *SpotlightChanged
	// PublishChanged is set for EventPublishStarted and EventPublishStopped.
	PublishChanged *PublishChanged
	// WatchdogChanged is set for EventWatchdogChanged.
	WatchdogChanged *WatchdogChanged
	// LifecycleChanged is set for EventLifecycleChanged.
	LifecycleChanged *LifecycleChanged
	// Moderation is set for EventModerationApplied. The same record the acting
	// caller received, emitted here too so a backend that did not issue the action
	// still learns of it -- including actions by a safety classifier or another
	// operator's console.
	Moderation *ModerationEntry
}

// ParticipantJoined is somebody arriving.
type ParticipantJoined struct {
	Participant Participant

	// FirstDevice is true when this is the person's first device in the room. A
	// room that says "alice joined" three times because she has three devices is a
	// room whose integrator was not given this flag.
	FirstDevice bool
}

// ParticipantLeft is somebody going.
type ParticipantLeft struct {
	Identity Identity

	// WasRemoved is whether they were removed rather than leaving. The moderation
	// entry that did it arrives separately; this is here so a simple consumer does
	// not have to correlate.
	WasRemoved bool

	// Code is set when WasRemoved.
	Code ModerationCode

	LastDevice bool
}

// PresenceChanged is a transition between presence states.
type PresenceChanged struct {
	Identity Identity
	From     Presence
	To       Presence
}

// GrantsChanged is a promotion or a demotion.
type GrantsChanged struct {
	Identity Identity
	From     Grants
	To       Grants
}

// MuteChanged is the participant's mute state after a change.
//
// In full rather than as a delta: there are three independent axes, and reconstructing
// them from deltas is a bug waiting to be written once per consumer.
type MuteChanged struct {
	Identity               Identity
	SelfMuted              []MediaKind
	ForcedMutedForEveryone []MediaKind
	ForcedMutedForAudience []MediaKind
}

// SpotlightChanged is the spotlight moving. Identity is empty when it was cleared.
type SpotlightChanged struct {
	Identity Identity
}

// PublishChanged is a track starting or stopping.
type PublishChanged struct {
	Identity Identity

	// Track is the full track name -- what started or stopped in the room's own
	// vocabulary rather than a summary of it.
	Track string

	Started bool
}

// WatchdogChanged is an observer attaching, changing health, or leaving.
type WatchdogChanged struct {
	Watchdog Watchdog

	// Detached is true when it left rather than changed state. A recorder
	// detaching is the event a compliance consumer is waiting for.
	Detached bool
}

// LifecycleChanged is a room changing phase.
type LifecycleChanged struct {
	From Lifecycle
	To   Lifecycle

	// ActingHost is set when To is LifecycleActingHost.
	ActingHost Identity
}

// EventQuery selects which events to follow.
type EventQuery struct {
	// After resumes past this sequence number.
	//
	// Zero starts at the present, which is right for a dashboard and wrong for
	// anything that must not miss an event. Persist Event.Sequence and pass it
	// here, and a crash you did not plan for resumes correctly.
	After uint64

	// Kinds filters to these kinds. Empty for all of them.
	Kinds []EventKind
}

// EventStream is an open subscription to a room's events.
//
// Not safe for concurrent use: one goroutine reads it. Cancel the context to stop.
type EventStream struct {
	stream grpc.ServerStreamingClient[adminpb.RoomEvent]
	cancel context.CancelFunc
}

// Events follows one room.
//
// The returned stream is live by the time this returns: the server sends headers once
// its subscription exists, and this waits for them. That matters for a caller that
// subscribes and then does something it expects to see -- without the wait, the action
// could be published before anybody was listening.
//
// Delivery is at-least-once and ordered per room. Handle a repeat.
func (r *RoomHandle) Events(ctx context.Context, query EventQuery) (*EventStream, error) {
	return openEvents(ctx, r.client, r.name, query)
}

// Events follows every room in the tenant, which is what a moderation queue wants and
// what a per-room integration should not use.
//
// EventQuery.After is refused here, because sequence numbers are per room and there is
// no tenant-wide ordering to resume from. A consumer that needs resumption follows the
// rooms it cares about.
func (c *Client) Events(ctx context.Context, query EventQuery) (*EventStream, error) {
	if query.After != 0 {
		return nil, errors.New("moqom: a tenant-wide stream cannot resume; sequence numbers are per room")
	}
	return openEvents(ctx, c, "", query)
}

func openEvents(ctx context.Context, client *Client, room string, query EventQuery) (*EventStream, error) {
	wire := &adminpb.StreamRoomEventsRequest{
		Tenant:        client.tenant,
		Room:          room,
		AfterSequence: query.After,
	}
	for _, kind := range query.Kinds {
		wire.Kind = append(wire.Kind, kindToProto(kind))
	}

	// The cancel is held rather than deferred: the stream outlives this call, and a
	// deferred cancel here would close it on the way out.
	streaming, cancel := context.WithCancel(ctx)
	stream, err := client.events.StreamRoomEvents(streaming, wire)
	if err != nil {
		cancel()
		return nil, wrap("Events", err)
	}
	if _, err := stream.Header(); err != nil {
		cancel()
		return nil, wrap("Events", err)
	}
	return &EventStream{stream: stream, cancel: cancel}, nil
}

// Recv returns the next event.
//
// Returns io.EOF when the server closed the stream cleanly, which happens on a deploy
// as well as at the end of a room. Reconnect from the last sequence you handled rather
// than from the present, or the events during the restart are lost silently.
func (e *EventStream) Recv() (Event, error) {
	wire, err := e.stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Event{}, io.EOF
		}
		return Event{}, wrap("Events.Recv", err)
	}
	return eventFromProto(wire), nil
}

// All is the stream as a range-over-func, for the common shape:
//
//	for event, err := range stream.All() {
//		if err != nil {
//			return err
//		}
//		handle(event)
//	}
//
// The iteration ends after yielding a non-nil error, io.EOF included, so a loop that
// breaks on error does not need to check for the end separately.
func (e *EventStream) All() iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for {
			event, err := e.Recv()
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// Close stops the stream.
func (e *EventStream) Close() { e.cancel() }

func kindToProto(kind EventKind) adminpb.RoomEventKind {
	for _, entry := range eventKindWire {
		if entry.kind == kind {
			return entry.wire
		}
	}
	return adminpb.RoomEventKind_ROOM_EVENT_KIND_UNSPECIFIED
}

func kindFromProto(wire adminpb.RoomEventKind) EventKind {
	for _, entry := range eventKindWire {
		if entry.wire == wire {
			return entry.kind
		}
	}
	return ""
}

func eventFromProto(wire *adminpb.RoomEvent) Event {
	event := Event{
		Tenant:   wire.GetTenant(),
		Room:     wire.GetRoom(),
		Sequence: wire.GetSequence(),
		At:       timeFromProto(wire.GetAt()),
		Kind:     kindFromProto(wire.GetKind()),
	}

	switch body := wire.GetBody().(type) {
	case *adminpb.RoomEvent_ParticipantJoined:
		event.Joined = &ParticipantJoined{
			Participant: participantFromProto(body.ParticipantJoined.GetParticipant()),
			FirstDevice: body.ParticipantJoined.GetFirstDevice(),
		}
	case *adminpb.RoomEvent_ParticipantLeft:
		event.Left = &ParticipantLeft{
			Identity:   identityFromProto(body.ParticipantLeft.GetIdentity()),
			WasRemoved: body.ParticipantLeft.GetWasRemoved(),
			Code:       codeFromProto(body.ParticipantLeft.GetCode()),
			LastDevice: body.ParticipantLeft.GetLastDevice(),
		}
	case *adminpb.RoomEvent_PresenceChanged:
		event.PresenceChanged = &PresenceChanged{
			Identity: identityFromProto(body.PresenceChanged.GetIdentity()),
			From:     presenceFromProto(body.PresenceChanged.GetFrom()),
			To:       presenceFromProto(body.PresenceChanged.GetTo()),
		}
	case *adminpb.RoomEvent_GrantsChanged:
		event.GrantsChanged = &GrantsChanged{
			Identity: identityFromProto(body.GrantsChanged.GetIdentity()),
			From:     grantsFromProto(body.GrantsChanged.GetFrom()),
			To:       grantsFromProto(body.GrantsChanged.GetTo()),
		}
	case *adminpb.RoomEvent_MuteChanged:
		event.MuteChanged = &MuteChanged{
			Identity:               identityFromProto(body.MuteChanged.GetIdentity()),
			SelfMuted:              mediaFromProto(body.MuteChanged.GetSelfMuted()),
			ForcedMutedForEveryone: mediaFromProto(body.MuteChanged.GetForcedMutedForEveryone()),
			ForcedMutedForAudience: mediaFromProto(body.MuteChanged.GetForcedMutedForAudience()),
		}
	case *adminpb.RoomEvent_SpotlightChanged:
		event.SpotlightChanged = &SpotlightChanged{
			Identity: identityFromProto(body.SpotlightChanged.GetIdentity()),
		}
	case *adminpb.RoomEvent_PublishChanged:
		event.PublishChanged = &PublishChanged{
			Identity: identityFromProto(body.PublishChanged.GetIdentity()),
			Track:    body.PublishChanged.GetTrack(),
			Started:  body.PublishChanged.GetStarted(),
		}
	case *adminpb.RoomEvent_WatchdogChanged:
		event.WatchdogChanged = &WatchdogChanged{
			Watchdog: watchdogFromProto(body.WatchdogChanged.GetWatchdog()),
			Detached: body.WatchdogChanged.GetDetached(),
		}
	case *adminpb.RoomEvent_LifecycleChanged:
		event.LifecycleChanged = &LifecycleChanged{
			From:       lifecycleFromProto(body.LifecycleChanged.GetFrom()),
			To:         lifecycleFromProto(body.LifecycleChanged.GetTo()),
			ActingHost: identityFromProto(body.LifecycleChanged.GetActingHost()),
		}
	case *adminpb.RoomEvent_ModerationApplied:
		entry := entryFromProto(body.ModerationApplied)
		event.Moderation = &entry
	}

	return event
}

var eventKindWire = []struct {
	kind EventKind
	wire adminpb.RoomEventKind
}{
	{EventParticipantJoined, adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_JOINED},
	{EventParticipantLeft, adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_LEFT},
	{EventPresenceChanged, adminpb.RoomEventKind_ROOM_EVENT_KIND_PRESENCE_CHANGED},
	{EventGrantsChanged, adminpb.RoomEventKind_ROOM_EVENT_KIND_GRANTS_CHANGED},
	{EventMuteChanged, adminpb.RoomEventKind_ROOM_EVENT_KIND_MUTE_CHANGED},
	{EventSpotlightChanged, adminpb.RoomEventKind_ROOM_EVENT_KIND_SPOTLIGHT_CHANGED},
	{EventPublishStarted, adminpb.RoomEventKind_ROOM_EVENT_KIND_PUBLISH_STARTED},
	{EventPublishStopped, adminpb.RoomEventKind_ROOM_EVENT_KIND_PUBLISH_STOPPED},
	{EventWatchdogChanged, adminpb.RoomEventKind_ROOM_EVENT_KIND_WATCHDOG_CHANGED},
	{EventLifecycleChanged, adminpb.RoomEventKind_ROOM_EVENT_KIND_LIFECYCLE_CHANGED},
	{EventModerationApplied, adminpb.RoomEventKind_ROOM_EVENT_KIND_MODERATION_APPLIED},
}
