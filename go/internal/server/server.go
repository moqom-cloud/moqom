// Package server implements the control plane's gRPC surface.
//
// Four public services for backends -- rooms, moderation, tokens, events -- and one
// internal stream for relays. They share the core below rather than being separate
// processes, because every moderation action touches all of them: it authorises
// against room state, writes an audit entry, pushes a directive to a relay, and
// publishes an event. Splitting that across services would put a network hop in the
// middle of an operation that has to be atomic enough to report honestly.
package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/timecode"
	"github.com/moqom-cloud/moqom/go/internal/token"
	"google.golang.org/grpc"
)

// Config is everything the control plane needs to run.
type Config struct {
	Store    store.Store
	Accounts Accounts

	// Issuer names this deployment in every token it mints, and is checked on
	// every token it verifies.
	Issuer string

	// Keys are the signing keys. The first mints; all of them verify.
	//
	// Ordered rather than a map so rotation is a deployment that prepends a key:
	// the new one starts signing, the old one keeps verifying, and it is removed
	// once the last token it signed has expired.
	Keys []token.Key

	// DispatchTimeout bounds how long a moderation call waits for relays to
	// confirm. Exceeding it is not a failure of the action -- the state is already
	// written -- but it is reported as unenforced, because a moderator who is told
	// "done" and sees otherwise stops believing the tool.
	DispatchTimeout time.Duration

	// RelayGrace is how long a relay may keep serving after losing its stream.
	RelayGrace time.Duration

	// SnapshotInterval is how often a relay re-asks for room state, which bounds
	// how stale enforcement can be when a push could not be delivered.
	SnapshotInterval time.Duration

	// Clock is where every timestamp the server writes comes from.
	//
	// A timecode rather than a bare instant because the events this server
	// records are compared against events from relays and phones elsewhere in
	// the world, and that comparison is only honest if each side says how well
	// it knew the time. Defaults to the host's disciplined system clock; see
	// `internal/timecode`.
	Clock timecode.Clock

	// Now is the legacy wall clock, kept for tests that only need to freeze
	// time. Ignored when Clock is set.
	Now func() time.Time

	IDs func() string
}

const (
	defaultDispatchTimeout  = 3 * time.Second
	defaultRelayGrace       = 30 * time.Second
	defaultSnapshotInterval = 30 * time.Second
)

type core struct {
	store    store.Store
	accounts Accounts
	issuer   string
	keys     []token.Key
	verifier *token.Verifier

	relays *relayRegistry
	bus    *eventBus

	dispatchTimeout  time.Duration
	relayGrace       time.Duration
	snapshotInterval time.Duration

	clock timecode.Clock
	ids   func() string
}

// now is the wall-clock estimate, for the many call sites that only need one.
//
// Derived from the clock rather than kept alongside it, so there is exactly one
// source of time in the process and no way for the two to disagree.
func (c *core) now() time.Time { return c.clock.Now().At }

// Server holds the services and can register them on a gRPC server.
type Server struct {
	core *core
}

// New validates the configuration and builds the services.
func New(config Config) (*Server, error) {
	if config.Store == nil {
		return nil, errors.New("control plane needs a store")
	}
	if config.Accounts == nil {
		return nil, errors.New("control plane needs a set of service accounts")
	}
	if len(config.Keys) == 0 {
		return nil, errors.New("control plane needs at least one signing key")
	}
	if config.Issuer == "" {
		return nil, errors.New("control plane needs an issuer name")
	}
	if config.Clock == nil {
		// A caller that froze `Now` gets a clock over it, marked
		// fallback time — which is the truth about an injected clock, and
		// keeps the fixture from quietly producing trustworthy-looking time.
		if config.Now != nil {
			config.Clock = timecode.FromFunc(config.Now)
		} else {
			config.Clock = timecode.NewHost()
		}
	}
	if config.IDs == nil {
		config.IDs = randomIDs()
	}
	if config.DispatchTimeout == 0 {
		config.DispatchTimeout = defaultDispatchTimeout
	}
	if config.RelayGrace == 0 {
		config.RelayGrace = defaultRelayGrace
	}
	if config.SnapshotInterval == 0 {
		config.SnapshotInterval = defaultSnapshotInterval
	}

	public := map[string]ed25519.PublicKey{}
	for _, key := range config.Keys {
		encoded, ok := key.Private.Public().(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("key %s is not Ed25519", key.ID)
		}
		public[key.ID] = encoded
	}

	return &Server{core: &core{
		store:            config.Store,
		accounts:         config.Accounts,
		issuer:           config.Issuer,
		keys:             config.Keys,
		verifier:         token.NewVerifier(config.Issuer, public),
		relays:           newRelayRegistry(),
		bus:              newEventBus(),
		dispatchTimeout:  config.DispatchTimeout,
		relayGrace:       config.RelayGrace,
		snapshotInterval: config.SnapshotInterval,
		clock:            config.Clock,
		ids:              config.IDs,
	}}, nil
}

// Register attaches every service to a gRPC server.
func (s *Server) Register(grpcServer grpc.ServiceRegistrar) {
	adminpb.RegisterRoomsServer(grpcServer, &roomsService{core: s.core})
	adminpb.RegisterModerationServer(grpcServer, &moderationService{core: s.core})
	adminpb.RegisterTokensServer(grpcServer, &tokensService{core: s.core})
	adminpb.RegisterEventsServer(grpcServer, &eventsService{core: s.core})
	registerRelayLink(grpcServer, s.core)
}

// Interceptors returns the authentication interceptors to install.
func (s *Server) Interceptors() (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	authenticator := Authenticator{Accounts: s.core.accounts}
	return authenticator.Unary, authenticator.Stream
}

// effectiveActor is the grants a call is evaluated against.
//
// The intersection of two things, and the intersection is the point. The service
// account's ceiling bounds what an API key can ever do, so a leaked key cannot
// exceed its purpose. The named human's own standing in the room bounds what this
// particular action may do, so a customer's console cannot turn a chat moderator
// into someone who bans by routing the request through a more privileged backend.
//
// A call with no `on_behalf_of` is the backend acting on its own account -- an
// automated safety verdict, a scheduled close -- and is bounded only by the
// account's ceiling. That is correct, and it is also why an integrator should set
// the field: it is the difference between an action attributable to a person and one
// attributable to a key.
func (c *core) effectiveActor(
	ctx context.Context,
	account ServiceAccount,
	tenant, room string,
	actor *adminpb.ActorContext,
) (control.Grants, error) {
	grants := account.Grants
	behalf := actor.GetOnBehalfOf()
	if behalf == "" {
		return grants, nil
	}

	username, err := control.NormaliseUsername(behalf)
	if err != nil {
		return control.Grants{}, err
	}
	human, err := c.grantsOf(ctx, tenant, room, username)
	if err != nil {
		return control.Grants{}, err
	}
	return control.Grants{
		Publish:    grants.Publish & human.Publish,
		Moderation: grants.Moderation & human.Moderation,
		IsOwner:    grants.IsOwner && human.IsOwner,
	}, nil
}

// grantsOf finds what a person currently holds in a room.
//
// Overrides first, then any of their connected devices, then the room's ownership
// record. Overrides win because they are what makes a demotion immediate; a token
// issued before it may still claim more.
func (c *core) grantsOf(ctx context.Context, tenant, room, username string) (control.Grants, error) {
	overrides, err := c.store.ListGrantOverrides(ctx, tenant, room)
	if err != nil {
		return control.Grants{}, err
	}
	for id, grants := range overrides {
		if id.Username == username {
			return grants, nil
		}
	}

	page, err := c.store.ListParticipants(ctx, store.ParticipantQuery{
		Tenant: tenant, Room: room, IncludeBackstage: true, IncludeDeparted: true,
		Paging: store.Paging{PageSize: store.MaxPageSize},
	})
	if err != nil {
		return control.Grants{}, err
	}
	// The most capable of their devices, because the powers belong to the person:
	// a moderator whose phone dropped is still a moderator on their laptop, and
	// picking the first device found would make authorisation depend on map order.
	var best control.Grants
	found := false
	for _, participant := range page.Participants {
		if participant.Identity.Username != username {
			continue
		}
		found = true
		best.Moderation |= participant.Grants.Moderation
		best.Publish |= participant.Grants.Publish
		best.IsOwner = best.IsOwner || participant.Grants.IsOwner
	}
	if found {
		return best, nil
	}

	// Not in the room. An owner may still act on their own room while absent --
	// that is what makes closing it from a backend possible -- so ownership is
	// checked against the record rather than the roster.
	found2, err := c.store.GetRoom(ctx, tenant, room)
	if err != nil {
		return control.Grants{}, err
	}
	if found2.Owner == username {
		return control.Grants{IsOwner: true}, nil
	}
	return control.Grants{}, fmt.Errorf("%w: %s holds nothing in %s", control.ErrPermission, username, room)
}

// targetGrants is what the person being acted on currently holds.
//
// Returns nil when they hold nothing that anyone needs standing over -- an ordinary
// viewer -- so the superset rule does not have to special-case absence.
func (c *core) targetGrants(ctx context.Context, tenant, room string, target control.Identity) *control.Grants {
	grants, err := c.grantsOf(ctx, tenant, room, target.Username)
	if err != nil {
		return &control.Grants{}
	}
	return &grants
}

// counts summarises who is in a room, for the fields on Room.
func (c *core) counts(ctx context.Context, tenant, room string) (roomCounts, error) {
	page, err := c.store.ListParticipants(ctx, store.ParticipantQuery{
		Tenant: tenant, Room: room, IncludeBackstage: true,
		Paging: store.Paging{PageSize: store.MaxPageSize},
	})
	if err != nil {
		return roomCounts{}, err
	}
	people := map[string]struct{}{}
	for _, participant := range page.Participants {
		people[participant.Identity.Username] = struct{}{}
	}

	watchdogs, err := c.store.ListWatchdogs(ctx, tenant, room)
	if err != nil {
		return roomCounts{}, err
	}
	recording := false
	for _, watchdog := range watchdogs {
		if watchdog.Kind == control.WatchdogRecording && watchdog.Health != control.WatchdogFailed {
			recording = true
			break
		}
	}

	return roomCounts{
		devices:   uint32(len(page.Participants)),
		people:    uint32(len(people)),
		recording: recording,
	}, nil
}

func randomIDs() func() string {
	var mu sync.Mutex
	var counter uint64
	start := time.Now().UnixNano()
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("%x-%d", start, counter)
	}
}
