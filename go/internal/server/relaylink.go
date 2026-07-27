package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// The relay side of the control plane: where a moderation decision becomes
// somebody's connection closing.

type relayLinkService struct {
	relaypb.UnimplementedRelayLinkServer
	core *core
}

func registerRelayLink(grpcServer grpc.ServiceRegistrar, core *core) {
	relaypb.RegisterRelayLinkServer(grpcServer, &relayLinkService{core: core})
}

// relayRegistry is which relays are attached and what they are serving.
//
// In memory, and therefore per control-plane replica. That is a real limitation
// with a real consequence: a moderation call handled by one replica cannot push a
// directive to a relay attached to another. It is survivable rather than fine,
// because the state a directive carries is also written durably -- so the worst case
// is enforcement delayed until the relay next asks for a snapshot, and the call
// reports `enforced: false` meanwhile rather than claiming success. Making it not a
// limitation needs a shared bus, and is noted in docs/PLAN.md rather than pretended
// away here.
type relayRegistry struct {
	mu     sync.Mutex
	relays map[string]*relayConn
	rooms  map[roomRef]map[string]struct{}
}

type roomRef struct {
	tenant string
	room   string
}

type relayConn struct {
	id     string
	region string

	// Buffered, because a slow relay must not block a moderation call. When it
	// fills, the send fails and the action is reported unenforced -- which is
	// true, and better than a moderator's request hanging on somebody else's
	// network.
	send chan *relaypb.ControlMessage

	mu       sync.Mutex
	sessions map[string]*relaypb.SessionState
	pending  map[string]chan *relaypb.DirectiveResult
}

const relaySendQueue = 64

func newRelayRegistry() *relayRegistry {
	return &relayRegistry{
		relays: map[string]*relayConn{},
		rooms:  map[roomRef]map[string]struct{}{},
	}
}

func (r *relayRegistry) attach(id, region string) *relayConn {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A reattaching relay replaces its previous entry. The same relay ID appearing
	// twice is a restart or a broken stream, not two machines, and leaving the old
	// entry would mean directives sent into a channel nobody reads.
	if existing, ok := r.relays[id]; ok {
		close(existing.send)
	}
	connection := &relayConn{
		id:       id,
		region:   region,
		send:     make(chan *relaypb.ControlMessage, relaySendQueue),
		sessions: map[string]*relaypb.SessionState{},
		pending:  map[string]chan *relaypb.DirectiveResult{},
	}
	r.relays[id] = connection
	return connection
}

func (r *relayRegistry) detach(connection *relayConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if current, ok := r.relays[connection.id]; ok && current == connection {
		delete(r.relays, connection.id)
	}
	for ref, serving := range r.rooms {
		delete(serving, connection.id)
		if len(serving) == 0 {
			delete(r.rooms, ref)
		}
	}
}

func (r *relayRegistry) noteRoom(ref roomRef, relayID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rooms[ref] == nil {
		r.rooms[ref] = map[string]struct{}{}
	}
	_, known := r.rooms[ref][relayID]
	r.rooms[ref][relayID] = struct{}{}
	return !known
}

// serving returns the relays that may be holding sessions for a room.
//
// Every relay that has reported a session in the room, including ones whose
// sessions have since closed. Over-broad on purpose: a directive to a relay with no
// matching session is answered with zero affected sessions and costs nothing, while
// missing a relay means a ban that does not land.
func (r *relayRegistry) serving(ref roomRef) []*relayConn {
	r.mu.Lock()
	defer r.mu.Unlock()

	var connections []*relayConn
	for id := range r.rooms[ref] {
		if connection, ok := r.relays[id]; ok {
			connections = append(connections, connection)
		}
	}
	return connections
}

func (c *relayConn) trackSession(session *relaypb.SessionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[session.GetSessionId()] = session
}

func (c *relayConn) dropSession(id string) *relaypb.SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	session := c.sessions[id]
	delete(c.sessions, id)
	return session
}

func (c *relayConn) session(id string) *relaypb.SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[id]
}

func (c *relayConn) expect(directiveID string) chan *relaypb.DirectiveResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Buffered so the reader loop never blocks handing over a result, even if the
	// caller has already given up and gone.
	results := make(chan *relaypb.DirectiveResult, 1)
	c.pending[directiveID] = results
	return results
}

func (c *relayConn) forget(directiveID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, directiveID)
}

func (c *relayConn) resolve(result *relaypb.DirectiveResult) {
	c.mu.Lock()
	waiting, ok := c.pending[result.GetDirectiveId()]
	delete(c.pending, result.GetDirectiveId())
	c.mu.Unlock()
	if ok {
		waiting <- result
	}
}

// Attach is the long-lived stream. One goroutine writes, this one reads.
func (s *relayLinkService) Attach(stream relaypb.RelayLink_AttachServer) error {
	if _, err := requireRelay(stream.Context()); err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetRelayId() == "" {
		return statusFor(fmt.Errorf("%w: the first message must be a Hello naming the relay", control.ErrInvalid))
	}

	connection := s.core.relays.attach(hello.GetRelayId(), hello.GetRegion())
	defer s.core.relays.detach(connection)

	writing, stopWriting := context.WithCancel(stream.Context())
	defer stopWriting()
	go s.write(writing, stream, connection)

	// A relay that has been serving sessions through a broken stream reports them
	// here. Without this the control plane's picture of who is where would be a
	// guess until each session happened to change.
	for _, session := range hello.GetExisting() {
		s.adoptSession(stream.Context(), connection, session)
	}

	connection.enqueue(&relaypb.ControlMessage{Body: &relaypb.ControlMessage_Welcome{
		Welcome: &relaypb.Welcome{
			At:               timestamppb.New(s.core.now()),
			Grace:            durationpb.New(s.core.relayGrace),
			SnapshotInterval: durationpb.New(s.core.snapshotInterval),
		},
	}})
	if keys := s.verificationKeys(); keys != nil {
		connection.enqueue(&relaypb.ControlMessage{Body: &relaypb.ControlMessage_VerificationKeys{
			VerificationKeys: keys,
		}})
	}

	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.handle(stream.Context(), connection, message); err != nil {
			// Logged rather than fatal. One malformed report is not worth dropping
			// a relay that is serving a room full of people.
			slog.WarnContext(stream.Context(), "relay report rejected",
				"relay", connection.id, "error", err)
		}
	}
}

func (s *relayLinkService) write(
	ctx context.Context,
	stream relaypb.RelayLink_AttachServer,
	connection *relayConn,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-connection.send:
			if !ok {
				return
			}
			if err := stream.Send(message); err != nil {
				return
			}
		}
	}
}

func (c *relayConn) enqueue(message *relaypb.ControlMessage) bool {
	select {
	case c.send <- message:
		return true
	default:
		// Dropped rather than blocked. See relaySendQueue: the caller learns this
		// as an unenforced action, which is the truth.
		return false
	}
}

func (s *relayLinkService) handle(
	ctx context.Context,
	connection *relayConn,
	message *relaypb.RelayMessage,
) error {
	switch body := message.GetBody().(type) {
	case *relaypb.RelayMessage_SessionOpened:
		s.adoptSession(ctx, connection, body.SessionOpened.GetSession())
		return nil
	case *relaypb.RelayMessage_SessionClosed:
		return s.closeSession(ctx, connection, body.SessionClosed)
	case *relaypb.RelayMessage_SessionChanged:
		return s.changeSession(ctx, connection, body.SessionChanged)
	case *relaypb.RelayMessage_DirectiveResult:
		connection.resolve(body.DirectiveResult)
		return nil
	case *relaypb.RelayMessage_SnapshotRequest:
		return s.sendSnapshot(ctx, connection, roomRef{
			tenant: body.SnapshotRequest.GetTenant(),
			room:   body.SnapshotRequest.GetRoom(),
		})
	case *relaypb.RelayMessage_Heartbeat:
		return nil
	case *relaypb.RelayMessage_Hello:
		return fmt.Errorf("%w: a second Hello on one stream", control.ErrInvalid)
	default:
		return fmt.Errorf("%w: unrecognised relay message", control.ErrInvalid)
	}
}

// adoptSession records a session and makes sure the relay has the room's state.
func (s *relayLinkService) adoptSession(
	ctx context.Context,
	connection *relayConn,
	session *relaypb.SessionState,
) {
	if session == nil || session.GetSessionId() == "" {
		return
	}
	connection.trackSession(session)
	ref := roomRef{tenant: session.GetTenant(), room: session.GetRoom()}
	if ref.room == "" {
		return
	}

	// First session in this room on this relay: it needs the bans and mutes it is
	// meant to be enforcing, and it needs them before it lets anybody publish.
	if first := s.core.relays.noteRoom(ref, connection.id); first {
		if err := s.sendSnapshot(ctx, connection, ref); err != nil {
			slog.WarnContext(ctx, "could not send a room snapshot",
				"relay", connection.id, "room", ref.room, "error", err)
		}
	}

	participant := control.Participant{
		Identity:   relayIdentity(session.GetIdentity()),
		Presence:   control.PresenceConnected,
		Backstage:  session.GetBackstage(),
		JoinedAt:   timeFromProto(session.GetOpenedAt()),
		RelayID:    connection.id,
		SessionID:  session.GetSessionId(),
		RemoteAddr: session.GetRemoteAddress(),
	}
	if grants, err := grantsFromProto(session.GetGrants(), adminpb.CapabilityPreset_CAPABILITY_PRESET_UNSPECIFIED); err == nil {
		participant.Grants = grants
	}

	if err := s.core.store.SaveParticipant(ctx, ref.tenant, ref.room, participant); err != nil {
		slog.WarnContext(ctx, "could not record a participant",
			"relay", connection.id, "room", ref.room, "error", err)
		return
	}
	s.core.publish(ctx, ref.tenant, ref.room, store.EventParticipantJoined, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_JOINED,
		Body: &adminpb.RoomEvent_ParticipantJoined{ParticipantJoined: &adminpb.ParticipantJoined{
			Participant: participantToProto(participant),
		}},
	})
}

// closeSession records a departure.
//
// A lost transport is not a departure. The participant is marked reconnecting and
// their record is kept, because they are probably in a tunnel -- and a room that
// forgot them would have to be rebuilt when they come back, which is what makes
// other platforms' brief network glitches look like a rejoin to everyone watching.
func (s *relayLinkService) closeSession(
	ctx context.Context,
	connection *relayConn,
	closed *relaypb.SessionClosed,
) error {
	session := connection.dropSession(closed.GetSessionId())
	if session == nil {
		return nil
	}
	tenant, room := session.GetTenant(), session.GetRoom()
	identity := relayIdentity(session.GetIdentity())

	presence := control.PresenceLeft
	if closed.GetCause() == relaypb.SessionClosed_CAUSE_TRANSPORT_LOST {
		presence = control.PresenceReconnecting
	}

	participant, err := s.core.store.GetParticipant(ctx, tenant, room, identity)
	if err != nil {
		return err
	}
	previous := participant.Presence
	participant.Presence = presence
	if err := s.core.store.SaveParticipant(ctx, tenant, room, participant); err != nil {
		return err
	}

	if presence == control.PresenceReconnecting {
		s.core.publish(ctx, tenant, room, store.EventPresenceChanged, &adminpb.RoomEvent{
			Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_PRESENCE_CHANGED,
			Body: &adminpb.RoomEvent_PresenceChanged{PresenceChanged: &adminpb.PresenceChanged{
				Identity: identityToProto(identity),
				From:     presenceByDomain[previous],
				To:       presenceByDomain[presence],
			}},
		})
		return nil
	}

	s.core.publish(ctx, tenant, room, store.EventParticipantLeft, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_LEFT,
		Body: &adminpb.RoomEvent_ParticipantLeft{ParticipantLeft: &adminpb.ParticipantLeft{
			Identity:   identityToProto(identity),
			WasRemoved: closed.GetCause() == relaypb.SessionClosed_CAUSE_EJECTED,
		}},
	})
	return nil
}

func (s *relayLinkService) changeSession(
	ctx context.Context,
	connection *relayConn,
	changed *relaypb.SessionChanged,
) error {
	session := connection.session(changed.GetSessionId())
	if session == nil {
		return nil
	}
	tenant, room := session.GetTenant(), session.GetRoom()
	identity := relayIdentity(session.GetIdentity())

	if changed.GetTrack() != "" {
		s.core.publish(ctx, tenant, room, store.EventPublishChanged, &adminpb.RoomEvent{
			Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_PUBLISH_STARTED,
			Body: &adminpb.RoomEvent_PublishChanged{PublishChanged: &adminpb.PublishChanged{
				Identity: identityToProto(identity),
				Track:    changed.GetTrack(),
				Started:  changed.GetPublishing(),
			}},
		})
	}
	if changed.GetBackstageChanged() {
		participant, err := s.core.store.GetParticipant(ctx, tenant, room, identity)
		if err != nil {
			return err
		}
		participant.Backstage = changed.GetBackstage()
		if err := s.core.store.SaveParticipant(ctx, tenant, room, participant); err != nil {
			return err
		}
	}
	return nil
}

// sendSnapshot gives a relay everything it needs to serve a room without asking.
func (s *relayLinkService) sendSnapshot(ctx context.Context, connection *relayConn, ref roomRef) error {
	snapshot, err := s.core.snapshot(ctx, ref)
	if err != nil {
		return err
	}
	if !connection.enqueue(&relaypb.ControlMessage{
		Body: &relaypb.ControlMessage_RoomSnapshot{RoomSnapshot: snapshot},
	}) {
		return errors.New("relay's queue is full")
	}
	return nil
}

func (c *core) snapshot(ctx context.Context, ref roomRef) (*relaypb.RoomSnapshot, error) {
	snapshot := &relaypb.RoomSnapshot{Tenant: ref.tenant, Room: ref.room}

	room, err := c.store.GetRoom(ctx, ref.tenant, ref.room)
	switch {
	case err == nil:
		snapshot.Closed = !room.IsOpen()
		snapshot.MaxParticipants = room.Policy.MaxParticipants
	case errors.Is(err, control.ErrNotFound):
		// A room nobody created, holding a session. Reported as closed rather than
		// permissive: a relay serving a room the control plane has never heard of
		// is a relay serving something unauthorised.
		snapshot.Closed = true
		return snapshot, nil
	default:
		return nil, err
	}

	bans, err := c.store.ListBans(ctx, ref.tenant, ref.room)
	if err != nil {
		return nil, err
	}
	for _, ban := range bans {
		snapshot.Bans = append(snapshot.Bans, banToRelay(ban))
	}

	mutes, err := c.store.ListMutes(ctx, ref.tenant, ref.room)
	if err != nil {
		return nil, err
	}
	for _, mute := range mutes {
		snapshot.Mutes = append(snapshot.Mutes, muteToRelay(mute))
	}

	overrides, err := c.store.ListGrantOverrides(ctx, ref.tenant, ref.room)
	if err != nil {
		return nil, err
	}
	for identity, grants := range overrides {
		snapshot.GrantOverrides = append(snapshot.GrantOverrides, &relaypb.GrantOverride{
			Identity: identityToProto(identity),
			Grants:   grantsToProto(grants),
		})
	}
	return snapshot, nil
}

func (s *relayLinkService) verificationKeys() *relaypb.VerificationKeys {
	keys := &relaypb.VerificationKeys{}
	for _, key := range s.core.keys {
		jwk, err := key.PublicJWK()
		if err != nil {
			continue
		}
		keys.Keys = append(keys.Keys, &adminpb.VerificationKey{
			KeyId:     key.ID,
			Algorithm: "EdDSA",
			Jwk:       jwk,
		})
	}
	if len(keys.Keys) == 0 {
		return nil
	}
	return keys
}

// dispatch sends a directive to every relay that might be holding an affected
// session, and waits for their answers.
//
// The waiting is what makes a moderation call able to tell the truth. It is bounded,
// and a timeout is not a failure of the action -- the state is already durable -- but
// it is reported as unenforced.
func (c *core) dispatch(
	ctx context.Context,
	ref roomRef,
	action *relaypb.Directive,
) (affected uint32, enforced bool) {
	connections := c.relays.serving(ref)
	if len(connections) == 0 {
		// Nobody is serving this room. Not a failure: the room may be empty, or
		// held by a relay attached to another replica. Either way the durable state
		// is what a session will be checked against when it arrives.
		return 0, true
	}

	action.DirectiveId = c.ids()
	action.Tenant = ref.tenant
	action.Room = ref.room

	waiting, cancel := context.WithTimeout(ctx, c.dispatchTimeout)
	defer cancel()

	type awaited struct {
		connection *relayConn
		results    chan *relaypb.DirectiveResult
	}
	var pending []awaited
	enforced = true

	for _, connection := range connections {
		results := connection.expect(action.GetDirectiveId())
		if !connection.enqueue(&relaypb.ControlMessage{
			Body: &relaypb.ControlMessage_Directive{Directive: action},
		}) {
			connection.forget(action.GetDirectiveId())
			enforced = false
			continue
		}
		pending = append(pending, awaited{connection: connection, results: results})
	}

	for _, outstanding := range pending {
		select {
		case result := <-outstanding.results:
			affected += result.GetAffectedSessions()
			if result.GetError() != "" {
				enforced = false
			}
		case <-waiting.Done():
			outstanding.connection.forget(action.GetDirectiveId())
			enforced = false
		}
	}
	return affected, enforced
}

// broadcastSnapshot re-sends room state to every relay serving it.
//
// Used after a change that a directive does not fully describe -- a policy edit, a
// ban lifted -- so a relay never has to reconstruct state from a series of deltas it
// might have missed one of.
func (c *core) broadcastSnapshot(ctx context.Context, ref roomRef) {
	snapshot, err := c.snapshot(ctx, ref)
	if err != nil {
		slog.WarnContext(ctx, "could not build a room snapshot", "room", ref.room, "error", err)
		return
	}
	for _, connection := range c.relays.serving(ref) {
		connection.enqueue(&relaypb.ControlMessage{
			Body: &relaypb.ControlMessage_RoomSnapshot{RoomSnapshot: snapshot},
		})
	}
}

// sessionsFor finds live sessions matching a target, for reporting how many devices
// an action reached.
func (c *core) sessionsFor(ref roomRef, target control.Identity) []*relaypb.SessionState {
	var matched []*relaypb.SessionState
	for _, connection := range c.relays.serving(ref) {
		connection.mu.Lock()
		for _, session := range connection.sessions {
			if session.GetRoom() != ref.room || session.GetTenant() != ref.tenant {
				continue
			}
			identity := relayIdentity(session.GetIdentity())
			if identity.Username != target.Username {
				continue
			}
			if target.Device != "" && identity.Device != target.Device {
				continue
			}
			matched = append(matched, session)
		}
		connection.mu.Unlock()
	}
	return matched
}

// relayCount is how many relays are attached, for readiness reporting.
func (c *core) relayCount() int {
	c.relays.mu.Lock()
	defer c.relays.mu.Unlock()
	return len(c.relays.relays)
}
