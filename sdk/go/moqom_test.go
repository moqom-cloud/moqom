package moqom_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	moqom "github.com/moqom-cloud/moqom-go"
	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// These tests are about the SDK, not the server, so the server is a stub that records
// what arrived and returns what it is told to. What matters here is the part a real
// control plane cannot check: that a retry reuses its idempotency key, that a
// credential does not travel in the clear, and that an error a caller has to branch on
// is one they can branch on without importing gRPC.
//
// The counterpart lives in the monorepo, where the same SDK drives the real control
// plane. Both are needed: this one can provoke failures a working server never
// produces, and that one can catch a mapping this one agrees with itself about.

// stub is a control plane that does as it is told.
type stub struct {
	adminpb.UnimplementedRoomsServer
	adminpb.UnimplementedModerationServer
	adminpb.UnimplementedTokensServer
	adminpb.UnimplementedEventsServer

	// kick is called for every Kick. Returning an error fails the call.
	kick func(*adminpb.KickRequest) (*adminpb.ModerationEntry, error)

	// setGrants is called for every SetGrants.
	setGrants func(*adminpb.SetGrantsRequest) (*adminpb.ModerationEntry, error)

	// authorization holds what the last call presented.
	authorization []string

	// events is sent to a subscriber, then the stream ends.
	events []*adminpb.RoomEvent

	// headerBeforeEvents mirrors the real server's promise that a stream is live
	// before its first event.
	headerBeforeEvents bool
}

func (s *stub) Kick(ctx context.Context, request *adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.authorization = md.Get("authorization")
	}
	if s.kick == nil {
		return &adminpb.ModerationEntry{Id: "entry"}, nil
	}
	return s.kick(request)
}

func (s *stub) GetRoom(_ context.Context, request *adminpb.GetRoomRequest) (*adminpb.Room, error) {
	if request.GetRoom() == "missing" {
		return nil, status.Error(codes.NotFound, "no such room")
	}
	return &adminpb.Room{
		Tenant:    request.GetTenant(),
		Room:      request.GetRoom(),
		Namespace: request.GetTenant() + "/r/" + request.GetRoom(),
		Lifecycle: adminpb.RoomLifecycle_ROOM_LIFECYCLE_LIVE,
		Policy: &adminpb.RoomPolicy{
			Recording:  adminpb.RecordingPolicy_RECORDING_POLICY_ALLOWED,
			StageSlots: 8,
			AgeGate:    &adminpb.AgeGate{MinimumAge: 18, RequireVerification: true},
		},
	}, nil
}

func (s *stub) StreamRoomEvents(request *adminpb.StreamRoomEventsRequest, stream grpc.ServerStreamingServer[adminpb.RoomEvent]) error {
	if s.headerBeforeEvents {
		if err := stream.SendHeader(nil); err != nil {
			return err
		}
	}
	for _, event := range s.events {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	return nil
}

// serve starts the stub and returns a client pointed at it.
func serve(t *testing.T, backing *stub, options ...moqom.Option) *moqom.Client {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	adminpb.RegisterRoomsServer(server, backing)
	adminpb.RegisterModerationServer(server, backing)
	adminpb.RegisterTokensServer(server, backing)
	adminpb.RegisterEventsServer(server, backing)

	go func() { _ = server.Serve(listener) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// The stub reads the header the SDK is expected to send, so it has to be
		// sent even though this connection supplies no credentials of its own.
		grpc.WithPerRPCCredentials(fakeCredential{}),
	)
	if err != nil {
		t.Fatalf("dialling the stub: %v", err)
	}

	options = append([]moqom.Option{
		moqom.WithTenant("acme"),
		moqom.WithConnection(conn),
	}, options...)

	client, err := moqom.Dial("ignored", options...)
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	})
	return client
}

type fakeCredential struct{}

func (fakeCredential) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer from-the-test"}, nil
}

func (fakeCredential) RequireTransportSecurity() bool { return false }

func TestDialRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := moqom.Dial("api.moqom.cloud:443", moqom.WithCredential("k")); err == nil {
		t.Fatal("a client without a tenant was accepted; every call names its tenant on the wire")
	}
	if _, err := moqom.Dial("api.moqom.cloud:443", moqom.WithTenant("acme")); err == nil {
		t.Fatal("a client without a credential was accepted")
	}
	if _, err := moqom.Dial("", moqom.WithTenant("acme"), moqom.WithCredential("k")); err == nil {
		t.Fatal("an empty target was accepted")
	}
}

// The credential is a bearer token that grants everything the service account can do,
// so it is marked as requiring transport security. Turning TLS off without saying so
// then fails at construction rather than quietly putting the token on the wire -- and
// failing loudly is the point, since the alternative is a leak nobody notices.
func TestTheCredentialWillNotTravelInTheClear(t *testing.T) {
	_, err := moqom.Dial("passthrough:///nowhere",
		moqom.WithTenant("acme"),
		moqom.WithCredential("secret"),
		moqom.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err == nil {
		t.Fatal("a client was built that would send a bearer credential over an insecure connection")
	}
	if !strings.Contains(err.Error(), "transport level security") {
		t.Fatalf("expected a refusal about transport security, got: %v", err)
	}

	// And the escape hatch works, because a sidecar on loopback is a real
	// deployment and forcing TLS there would push people to disable it wholesale.
	relaxed := serve(t, &stub{}, moqom.WithoutTransportSecurity())
	if _, err := relaxed.Room("live").Kick(context.Background(), moqom.KickRequest{Target: moqom.User("carol")}); err != nil {
		t.Fatalf("an explicitly insecure client could not call: %v", err)
	}
}

func TestTheCredentialIsSentAsABearerToken(t *testing.T) {
	backing := &stub{}
	client := serve(t, backing)

	if _, err := client.Room("live").Kick(context.Background(), moqom.KickRequest{Target: moqom.User("carol")}); err != nil {
		t.Fatalf("kicking: %v", err)
	}
	if len(backing.authorization) != 1 || !strings.HasPrefix(backing.authorization[0], "Bearer ") {
		t.Fatalf("expected one bearer header, got %v", backing.authorization)
	}
}

// The property the whole retry design exists for. A queue that redelivers "ban for 24
// hours" must not extend the ban, which only holds if every attempt carries the same
// key -- so generating one per attempt would defeat it silently, and silently is why
// this is a test rather than a comment.
func TestARetryReusesItsIdempotencyKey(t *testing.T) {
	var keys []string
	backing := &stub{}
	backing.kick = func(request *adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
		keys = append(keys, request.GetActor().GetIdempotencyKey())
		if len(keys) < 3 {
			return nil, status.Error(codes.Unavailable, "try again")
		}
		return &adminpb.ModerationEntry{Id: "entry", Enforced: true}, nil
	}

	client := serve(t, backing, moqom.WithRetry(3, time.Millisecond))
	entry, err := client.Room("live").Kick(context.Background(), moqom.KickRequest{
		Target: moqom.User("carol"),
		Code:   moqom.CodeHarassment,
	})
	if err != nil {
		t.Fatalf("kicking: %v", err)
	}
	if entry.ID != "entry" {
		t.Fatalf("expected the third attempt's entry, got %q", entry.ID)
	}
	if len(keys) != 3 {
		t.Fatalf("expected three attempts, got %d", len(keys))
	}
	if keys[0] == "" {
		t.Fatal("no idempotency key was generated")
	}
	for _, key := range keys[1:] {
		if key != keys[0] {
			t.Fatalf("attempts used different keys: %v", keys)
		}
	}
}

// A caller's own key has to win, because a key generated inside this process cannot
// survive the process. A worker that wants "this message, however many times it is
// delivered" derives one from the message.
func TestACallerSuppliedKeyIsUsedVerbatim(t *testing.T) {
	var seen string
	backing := &stub{}
	backing.kick = func(request *adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
		seen = request.GetActor().GetIdempotencyKey()
		return &adminpb.ModerationEntry{Id: "entry"}, nil
	}

	client := serve(t, backing)
	_, err := client.Room("live").Kick(context.Background(), moqom.KickRequest{
		Target: moqom.User("carol"),
		Actor:  moqom.Actor{IdempotencyKey: "queue-message-91"},
	})
	if err != nil {
		t.Fatalf("kicking: %v", err)
	}
	if seen != "queue-message-91" {
		t.Fatalf("expected the caller's key, got %q", seen)
	}
}

// Retrying a permission failure is how a moderation tool ends up hammering a door it
// will never be let through, so only codes that cannot have applied a change are
// retried.
func TestOnlyTransientFailuresAreRetried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     codes.Code
		attempts int
	}{
		{"denied", codes.PermissionDenied, 1},
		{"invalid", codes.InvalidArgument, 1},
		{"missing", codes.NotFound, 1},
		{"unavailable", codes.Unavailable, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			backing := &stub{}
			backing.kick = func(*adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
				attempts++
				return nil, status.Error(tc.code, "no")
			}

			client := serve(t, backing, moqom.WithRetry(3, time.Millisecond))
			_, err := client.Room("live").Kick(context.Background(), moqom.KickRequest{Target: moqom.User("carol")})
			if err == nil {
				t.Fatal("expected a failure")
			}
			if attempts != tc.attempts {
				t.Fatalf("%s was tried %d times, expected %d", tc.code, attempts, tc.attempts)
			}
		})
	}
}

// A caller should be able to branch on what went wrong without importing gRPC, and the
// operation should be in the message: "permission denied" with no verb is the kind of
// log line that costs an hour.
func TestErrorsCarryASentinelAndTheOperation(t *testing.T) {
	backing := &stub{}
	client := serve(t, backing, moqom.WithRetry(1, 0))

	for _, tc := range []struct {
		code     codes.Code
		sentinel error
	}{
		{codes.PermissionDenied, moqom.ErrDenied},
		{codes.NotFound, moqom.ErrNotFound},
		{codes.InvalidArgument, moqom.ErrInvalid},
		{codes.Unauthenticated, moqom.ErrUnauthenticated},
		{codes.AlreadyExists, moqom.ErrExists},
		{codes.FailedPrecondition, moqom.ErrClosed},
		{codes.Unavailable, moqom.ErrUnavailable},
		{codes.Internal, moqom.ErrInternal},
	} {
		backing.kick = func(*adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
			return nil, status.Error(tc.code, "the server's explanation")
		}

		_, err := client.Room("live").Kick(context.Background(), moqom.KickRequest{Target: moqom.User("carol")})
		if !errors.Is(err, tc.sentinel) {
			t.Fatalf("%s did not map to %v: %v", tc.code, tc.sentinel, err)
		}

		var detailed *moqom.Error
		if !errors.As(err, &detailed) {
			t.Fatalf("%s did not produce a *moqom.Error: %v", tc.code, err)
		}
		if detailed.Op != "Room.Kick" {
			t.Fatalf("expected the operation on the error, got %q", detailed.Op)
		}
		if detailed.Code != tc.code {
			t.Fatalf("expected the original code on the error, got %v", detailed.Code)
		}
		if !strings.Contains(err.Error(), "the server's explanation") {
			t.Fatalf("the server's message was dropped: %v", err)
		}
	}
}

// Cancellation is the caller's own doing, and dressing it up as an SDK failure makes a
// clean shutdown look like an outage.
func TestCancellationIsNotDressedUp(t *testing.T) {
	backing := &stub{}
	backing.kick = func(*adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
		time.Sleep(time.Second)
		return &adminpb.ModerationEntry{}, nil
	}

	client := serve(t, backing, moqom.WithRetry(1, 0))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := client.Room("live").Kick(ctx, moqom.KickRequest{Target: moqom.User("carol")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// Malformed requests are refused here rather than on the wire. Not for speed: a ban
// with two subjects is ambiguous about which one was meant, and the round trip would
// only tell the caller the same thing later.
func TestObviouslyMalformedRequestsAreRefusedLocally(t *testing.T) {
	client := serve(t, &stub{})
	room := client.Room("live")
	ctx := context.Background()

	if _, err := room.Kick(ctx, moqom.KickRequest{}); err == nil {
		t.Fatal("a kick with no target was accepted")
	}
	if _, err := room.Ban(ctx, moqom.BanRequest{}); err == nil {
		t.Fatal("a ban with no subject was accepted")
	}
	if _, err := room.Ban(ctx, moqom.BanRequest{Subject: moqom.BanSubject{
		Username: "carol",
		IPPrefix: "10.0.0.0/8",
	}}); err == nil {
		t.Fatal("a ban with two subjects was accepted")
	}
	if _, err := room.ForceMute(ctx, moqom.MuteRequest{Target: moqom.User("carol")}); err == nil {
		t.Fatal("a mute with no scope was accepted; the scope is which mute, not a detail")
	}
	if _, err := room.SetGrants(ctx, moqom.GrantsRequest{
		Target: moqom.User("carol"),
		Preset: "archduke",
	}); err == nil {
		t.Fatal("an unknown preset was accepted")
	}
	if _, err := client.CreateRoom(ctx, moqom.NewRoom{}); err == nil {
		t.Fatal("a room with no name was accepted")
	}
	if _, err := room.Participant(ctx, moqom.Identity{Username: "carol"}); err == nil {
		t.Fatal("a participant read with no device was accepted; that question's answer is a list")
	}
}

func TestReadsComeBackAsDomainTypes(t *testing.T) {
	client := serve(t, &stub{})

	room, err := client.Room("live").Get(context.Background())
	if err != nil {
		t.Fatalf("reading the room: %v", err)
	}
	if room.Name != "live" || room.Namespace != "acme/r/live" {
		t.Fatalf("unexpected room: %+v", room)
	}
	if room.Lifecycle != moqom.LifecycleLive {
		t.Fatalf("expected a live room, got %q", room.Lifecycle)
	}
	if room.Policy.Recording != moqom.RecordingAllowed || room.Policy.StageSlots != 8 {
		t.Fatalf("unexpected policy: %+v", room.Policy)
	}
	if room.Policy.AgeGate.MinimumAge != 18 || !room.Policy.AgeGate.RequireVerification {
		t.Fatalf("unexpected age gate: %+v", room.Policy.AgeGate)
	}

	// An unset timestamp has to arrive as a zero time rather than the epoch:
	// EndedAt of 1970 on a live room would make every one of them look finished.
	if !room.EndedAt.IsZero() {
		t.Fatalf("an absent timestamp became %v", room.EndedAt)
	}

	if _, err := client.Room("missing").Get(context.Background()); !errors.Is(err, moqom.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Capabilities are a bitmask here and a repeated enum on the wire, so the translation
// is a place a bit could go missing. Every one is checked, in both directions.
func TestEveryCapabilityAndScopeSurvivesTheWire(t *testing.T) {
	backing := &stub{}
	client := serve(t, backing)

	// SetGrants is the round trip: the request carries the mask outward and the
	// entry carries it back.
	grants := moqom.Grants{
		Publish:    moqom.PublishFull,
		Moderation: moqom.CapAll,
	}

	entry, seen, err := grantsRoundTrip(t, client, backing, grants)
	if err != nil {
		t.Fatalf("setting grants: %v", err)
	}
	if got := len(seen.GetGrants().GetModeration()); got != 21 {
		t.Fatalf("CapAll became %d wire values, expected 21", got)
	}
	if got := len(seen.GetGrants().GetPublish()); got != 4 {
		t.Fatalf("PublishFull became %d wire values, expected 4", got)
	}
	if entry.Grants.Moderation != moqom.CapAll {
		t.Fatalf("capabilities did not survive the round trip: %s", entry.Grants.Moderation)
	}
	if entry.Grants.Publish != moqom.PublishFull {
		t.Fatalf("publish scopes did not survive the round trip: %s", entry.Grants.Publish)
	}

	// And one at a time, because a mapping table with two rows pointing at the same
	// wire value would pass the test above.
	for _, capability := range []moqom.Capability{
		moqom.CapDeleteMessage, moqom.CapTimeoutUser, moqom.CapBanFromChat,
		moqom.CapSetChatMode, moqom.CapManageFilters, moqom.CapPromoteToGuest,
		moqom.CapPromoteToCohost, moqom.CapDemote, moqom.CapForceMute,
		moqom.CapSpotlight, moqom.CapManageQueue, moqom.CapLockStage,
		moqom.CapKick, moqom.CapBanFromRoom, moqom.CapBanFromTenant,
		moqom.CapCloseRoom, moqom.CapChangeRoomPolicy, moqom.CapManageWatchdogs,
		moqom.CapAppointModerators, moqom.CapReadModerationLog, moqom.CapHandleAppeals,
	} {
		entry, _, err := grantsRoundTrip(t, client, backing, moqom.Grants{Moderation: capability})
		if err != nil {
			t.Fatalf("setting %s: %v", capability, err)
		}
		if entry.Grants.Moderation != capability {
			t.Fatalf("%s came back as %s", capability, entry.Grants.Moderation)
		}
	}
}

// grantsRoundTrip sends grants through the stub, which echoes them back on the entry.
func grantsRoundTrip(t *testing.T, client *moqom.Client, backing *stub, grants moqom.Grants) (moqom.ModerationEntry, *adminpb.SetGrantsRequest, error) {
	t.Helper()

	var seen *adminpb.SetGrantsRequest
	backing.setGrants = func(request *adminpb.SetGrantsRequest) (*adminpb.ModerationEntry, error) {
		seen = request
		return &adminpb.ModerationEntry{
			Verb:   adminpb.ModerationVerb_MODERATION_VERB_SET_GRANTS,
			Grants: request.GetGrants(),
		}, nil
	}

	entry, err := client.Room("live").SetGrants(context.Background(), moqom.GrantsRequest{
		Target: moqom.User("carol"),
		Grants: grants,
	})
	return entry, seen, err
}

func (s *stub) SetGrants(_ context.Context, request *adminpb.SetGrantsRequest) (*adminpb.ModerationEntry, error) {
	if s.setGrants == nil {
		return &adminpb.ModerationEntry{}, nil
	}
	return s.setGrants(request)
}

func TestEventsArriveInOrderAndTheIteratorEnds(t *testing.T) {
	backing := &stub{
		headerBeforeEvents: true,
		events: []*adminpb.RoomEvent{
			{
				Tenant: "acme", Room: "live", Sequence: 7,
				Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_PARTICIPANT_JOINED,
				Body: &adminpb.RoomEvent_ParticipantJoined{
					ParticipantJoined: &adminpb.ParticipantJoined{
						Participant: &adminpb.Participant{
							Identity: &adminpb.Identity{Username: "carol", Device: "phone"},
							Presence: adminpb.Presence_PRESENCE_CONNECTED,
						},
						FirstDevice: true,
					},
				},
			},
			{
				Tenant: "acme", Room: "live", Sequence: 8,
				Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_MODERATION_APPLIED,
				Body: &adminpb.RoomEvent_ModerationApplied{
					ModerationApplied: &adminpb.ModerationEntry{
						Verb:      adminpb.ModerationVerb_MODERATION_VERB_FORCE_MUTE,
						MuteScope: adminpb.MuteScope_MUTE_SCOPE_AUDIENCE,
						Media:     []adminpb.MediaKind{adminpb.MediaKind_MEDIA_KIND_AUDIO},
						Enforced:  true,
					},
				},
			},
		},
	}

	client := serve(t, backing)
	stream, err := client.Room("live").Events(context.Background(), moqom.EventQuery{})
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer stream.Close()

	var got []moqom.Event
	var final error
	for event, err := range stream.All() {
		if err != nil {
			final = err
			break
		}
		got = append(got, event)
	}

	if !errors.Is(final, io.EOF) {
		t.Fatalf("expected the stream to end with io.EOF, got %v", final)
	}
	if len(got) != 2 {
		t.Fatalf("expected two events, got %d", len(got))
	}
	if got[0].Sequence != 7 || got[0].Joined == nil {
		t.Fatalf("unexpected first event: %+v", got[0])
	}
	if got[0].Joined.Participant.Identity.Username != "carol" || !got[0].Joined.FirstDevice {
		t.Fatalf("the join did not survive: %+v", got[0].Joined)
	}
	if got[1].Moderation == nil || got[1].Moderation.MuteScope != moqom.MuteForAudience {
		t.Fatalf("the moderation entry did not survive: %+v", got[1].Moderation)
	}
	if len(got[1].Moderation.Media) != 1 || got[1].Moderation.Media[0] != moqom.MediaAudio {
		t.Fatalf("the muted media did not survive: %+v", got[1].Moderation.Media)
	}
}

// Sequence numbers are per room, so there is nothing to resume from on a tenant-wide
// stream. Refusing locally says so; sending it would earn an InvalidArgument that reads
// like a server problem.
func TestATenantWideStreamCannotResume(t *testing.T) {
	client := serve(t, &stub{headerBeforeEvents: true})
	if _, err := client.Events(context.Background(), moqom.EventQuery{After: 12}); err == nil {
		t.Fatal("a tenant-wide stream accepted a cursor")
	}
	stream, err := client.Events(context.Background(), moqom.EventQuery{})
	if err != nil {
		t.Fatalf("opening a tenant-wide stream: %v", err)
	}
	stream.Close()
}

func TestPresetsExpandTheSameWayTheServerDoes(t *testing.T) {
	for preset, expected := range map[moqom.Preset]moqom.Capability{
		moqom.PresetNone:           0,
		moqom.PresetModerator:      moqom.CapChatGroup,
		moqom.PresetSuperModerator: moqom.CapChatGroup | moqom.CapStageGroup,
		moqom.PresetOwner:          moqom.CapAll,
	} {
		got, ok := preset.Expand()
		if !ok {
			t.Fatalf("%s did not expand", preset)
		}
		if got != expected {
			t.Fatalf("%s expanded to %s, expected %s", preset, got, expected)
		}
	}
	if _, ok := moqom.Preset("archduke").Expand(); ok {
		t.Fatal("an unknown preset expanded")
	}
}

// Role is display-only, but it is what an interface labels people with, so getting it
// wrong is visible to every user.
func TestRoleSummarisesGrants(t *testing.T) {
	for _, tc := range []struct {
		name     string
		grants   moqom.Grants
		expected moqom.Role
	}{
		{"owner", moqom.Grants{IsOwner: true}, moqom.RoleOwner},
		{"cohost", moqom.Grants{Publish: moqom.PublishCamera, Moderation: moqom.CapForceMute}, moqom.RoleCohost},
		{"guest", moqom.Grants{Publish: moqom.PublishAudio}, moqom.RoleGuest},
		{"moderator", moqom.Grants{Moderation: moqom.CapChatGroup}, moqom.RoleModerator},
		{"viewer", moqom.Grants{}, moqom.RoleViewer},
	} {
		if got := tc.grants.Role(); got != tc.expected {
			t.Fatalf("%s: got %q, expected %q", tc.name, got, tc.expected)
		}
	}
}
