package server

import (
	"context"
	"log/slog"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// The event stream is how a Go backend follows a room it cannot join.
//
// Two sources, joined carefully. History comes from the store, so a consumer that
// was away gets what it missed; live events come from an in-process bus, so a
// consumer that is caught up sees things as they happen rather than on a poll
// interval. The join is the interesting part: read history first, then switch to
// live, and drop live events at or below the last sequence already sent. Without
// that overlap, an event landing between the two reads is lost -- which is exactly
// the moderation verdict somebody needed.

type eventsService struct {
	adminpb.UnimplementedEventsServer
	core *core
}

// eventBus fans live events out to open streams.
type eventBus struct {
	mu     sync.Mutex
	next   uint64
	listen map[uint64]*listener
}

type listener struct {
	tenant string
	room   string
	kinds  map[store.EventKind]bool
	// Buffered. A consumer that stops reading is disconnected rather than allowed
	// to slow down the room it is watching: an events API that can apply back
	// pressure to moderation is an events API that can stall a kick.
	events chan store.Event
}

const listenerQueue = 256

func newEventBus() *eventBus {
	return &eventBus{listen: map[uint64]*listener{}}
}

func (b *eventBus) subscribe(tenant, room string, kinds map[store.EventKind]bool) (uint64, *listener) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.next++
	entry := &listener{
		tenant: tenant,
		room:   room,
		kinds:  kinds,
		events: make(chan store.Event, listenerQueue),
	}
	b.listen[b.next] = entry
	return b.next, entry
}

func (b *eventBus) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry, ok := b.listen[id]; ok {
		delete(b.listen, id)
		close(entry.events)
	}
}

func (b *eventBus) publish(event store.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, entry := range b.listen {
		if entry.tenant != event.Tenant {
			continue
		}
		if entry.room != "" && entry.room != event.Room {
			continue
		}
		if len(entry.kinds) > 0 && !entry.kinds[event.Kind] {
			continue
		}
		select {
		case entry.events <- event:
		default:
			// The consumer is not reading. Closing its channel ends its stream with
			// a gap it can detect from the sequence numbers, which is better than
			// silently skipping events it will never know it missed.
			delete(b.listen, id)
			close(entry.events)
		}
	}
}

// publish records an event and hands it to any open streams.
//
// Durable first, live second. A live event that was never stored would be invisible
// to a consumer that reconnects, and the sequence number a consumer resumes from only
// exists once the store has assigned it.
func (c *core) publish(
	ctx context.Context,
	tenant, room string,
	kind store.EventKind,
	event *adminpb.RoomEvent,
) {
	event.Tenant = tenant
	event.Room = room
	if event.At == nil {
		event.At = timestamppb.New(c.now())
	}

	body, err := proto.Marshal(event)
	if err != nil {
		slog.WarnContext(ctx, "could not encode a room event", "kind", kind, "error", err)
		return
	}

	stored, err := c.store.AppendEvent(ctx, store.Event{
		Tenant: tenant, Room: room, Kind: kind, At: event.At.AsTime(), Body: body,
	})
	if err != nil {
		slog.WarnContext(ctx, "could not record a room event", "kind", kind, "error", err)
		return
	}
	c.bus.publish(stored)
}

func (s *eventsService) StreamRoomEvents(
	request *adminpb.StreamRoomEventsRequest,
	stream adminpb.Events_StreamRoomEventsServer,
) error {
	ctx := stream.Context()
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return err
	}

	kinds := map[store.EventKind]bool{}
	for _, kind := range request.GetKind() {
		mapped, ok := eventKindByProto[kind]
		if !ok {
			return status.Errorf(codes.InvalidArgument, "unknown event kind %v", kind)
		}
		kinds[mapped] = true
	}

	// Subscribed before reading history, so anything happening during the read is
	// queued rather than lost. The overlap is then discarded by sequence number.
	id, live := s.core.bus.subscribe(request.GetTenant(), request.GetRoom(), kinds)
	defer s.core.bus.unsubscribe(id)

	// Resumption is per room, because sequences are. A tenant-wide stream asking to
	// resume has no well-defined place to resume from, and silently starting at the
	// present would look like resumption while quietly dropping the backlog.
	if request.GetAfterSequence() > 0 && request.GetRoom() == "" {
		return status.Error(codes.InvalidArgument, "after_sequence needs a room, since sequences are per room")
	}

	highest := request.GetAfterSequence()
	if highest > 0 {
		history, err := s.core.store.ListEvents(ctx, store.EventQuery{
			Tenant:        request.GetTenant(),
			Room:          request.GetRoom(),
			AfterSequence: request.GetAfterSequence(),
			Limit:         store.MaxPageSize,
		})
		if err != nil {
			return statusFor(err)
		}
		for _, event := range history {
			if err := sendEvent(stream, event); err != nil {
				return err
			}
			if event.Sequence > highest {
				highest = event.Sequence
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case event, ok := <-live.events:
			if !ok {
				// The bus dropped this listener because it was not keeping up. Ending
				// the stream is honest: the consumer will resume from its cursor and
				// see the gap rather than skipping it.
				return status.Error(codes.ResourceExhausted, "the consumer fell behind")
			}
			if event.Sequence <= highest && event.Room == request.GetRoom() {
				continue
			}
			if err := sendEvent(stream, event); err != nil {
				return err
			}
			if event.Sequence > highest {
				highest = event.Sequence
			}
		}
	}
}

func sendEvent(stream adminpb.Events_StreamRoomEventsServer, event store.Event) error {
	var decoded adminpb.RoomEvent
	if err := proto.Unmarshal(event.Body, &decoded); err != nil {
		return statusFor(err)
	}
	// The sequence is authoritative from the store rather than from the body: the
	// body was encoded before the number was assigned.
	decoded.Sequence = event.Sequence
	return stream.Send(&decoded)
}

var eventKindByProto = map[adminpb.RoomEventKind]store.EventKind{
	adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_JOINED: store.EventParticipantJoined,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_LEFT:   store.EventParticipantLeft,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_PRESENCE_CHANGED:   store.EventPresenceChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_GRANTS_CHANGED:     store.EventGrantsChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_MUTE_CHANGED:       store.EventMuteChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_SPOTLIGHT_CHANGED:  store.EventSpotlightChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_PUBLISH_STARTED:    store.EventPublishChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_PUBLISH_STOPPED:    store.EventPublishChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_WATCHDOG_CHANGED:   store.EventWatchdogChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_LIFECYCLE_CHANGED:  store.EventLifecycleChanged,
	adminpb.RoomEventKind_ROOM_EVENT_KIND_MODERATION_APPLIED: store.EventModerationApplied,
}
