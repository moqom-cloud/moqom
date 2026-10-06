package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/token"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The one request a client device is allowed to make of the control plane.
//
// # Why this exists
//
// A token is capped at fifteen minutes, because it is a bearer credential and
// its lifetime is the window a leaked one is useful for. That cap is right and
// it makes a token useless as a thing to configure a device with: every path
// that put one on a phone by hand -- a launch argument, a pasted string, a QR
// code -- produced a phone that worked for a quarter of an hour and then sat
// there being refused, with the reason inside a credential nobody can read.
//
// What a real application does is ask its own server, which knows who signed in
// and asks the control plane on their behalf. This is that server, for
// deployments that do not have one yet: one route, reachable from the internet,
// that turns a long-lived device credential into a short-lived participant
// token. The phone then fetches one immediately before it dials, every time,
// and the expiry stops being something anybody has to think about.
//
// # Why it is safe to expose when the admin API is not
//
// The admin port demands a client certificate and offers rooms, moderation,
// events and minting with whatever the calling account may do. This offers
// exactly one thing and narrows it twice.
//
// First, the credential is an ordinary service account, so it is bounded by the
// ceiling that already exists: an account configured to let people join rooms
// cannot mint a moderator however the request is phrased. Second, and
// regardless of the account, this endpoint requests no moderation and no
// ownership -- so pointing it at an owner's credential by mistake still mints
// plain participants. The blast radius of the key a phone carries is "can join
// rooms in one tenant as any name", which is what a device credential is for.
//
// What it is not is authentication. Nothing here knows who is holding the
// phone, and the username is whatever was asked for. A deployment that needs
// those to be the same person needs its own server in front of this, which is
// the thing this stands in for.

// JoinRequest is what a device asks for.
//
// Deliberately not the mint request. A device does not get to name grants, a
// lifetime, an owner flag or an attribution -- all of which the admin API
// accepts and all of which this decides for itself.
type JoinRequest struct {
	Tenant   string `json:"tenant"`
	Username string `json:"username"`
	Room     string `json:"room"`

	// Device is the thumbprint the token is bound to by its `cnf` claim.
	//
	// Superseded by Key and kept for the clients that predate it. A thumbprint
	// on its own is an assertion -- the device saying which key it holds --
	// and the relay now demands a signature from whatever `cnf.jkt` names, so a
	// token minted from a bare thumbprint is one whose holder may or may not be
	// able to use it. That is legal and it is worse than it sounds: the failure
	// arrives later, at a relay, as an authentication error with nothing there
	// to explain it.
	Device string `json:"device"`

	// Key is the device's public signing key, as a JWK.
	//
	// The thumbprint is derived from this rather than taken from Device, which
	// is the whole difference between binding a token to a key and labelling it
	// with a name. Sending it costs a client four short strings it already has;
	// see internal/token.PublicKey.
	Key *token.PublicKey `json:"key,omitempty"`
}

// JoinResponse is the credential and enough about it to display.
//
// The expiry is included even though it is inside the token, because a client
// that has to base64-decode a JWS to find out when to refresh is a client that
// will get it wrong once and then cache it forever.
type JoinResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Room      string    `json:"room"`
	Username  string    `json:"username"`
	KeyID     string    `json:"keyId"`

	// Device is the thumbprint this token was bound to, untruncated.
	//
	// Echoed so the caller can see which key the control plane decided the
	// token belongs to, rather than assuming it was the one they meant. A
	// mismatch here is the cheapest place to notice a token that will be
	// refused by the relay: the alternative is a session that fails to
	// authenticate with the reason buried in a credential nobody reads.
	Device string `json:"device"`
}

// JoinPath is the only route this handler serves.
const JoinPath = "/join"

// howManyJoinsPerMinute bounds one credential.
//
// A public endpoint that mints signatures needs a ceiling, and this one can be
// low: a device asks for a token when somebody presses Join, so a handful a
// minute is generous for a person and cheap to serve. It is a brake rather than
// a security control -- a leaked credential is a leaked credential -- and what
// it buys is that the leak cannot also be a way to spend the control plane's CPU
// or fill its logs.
const howManyJoinsPerMinute = 30

// howBigAJoinRequestMayBe caps the body.
//
// Four fields of a few dozen characters. Anything larger is either a mistake or
// somebody seeing what happens, and reading it into memory to find out which is
// the mistake.
const howBigAJoinRequestMayBe = 4 << 10

// JoinHandler serves “JoinPath“, minting participant tokens for devices.
//
// Returned separately from “HealthHandler“ because the two belong on different
// ports: readiness is for the orchestrator and stays closed to everything else,
// and this one is the only thing in the process that faces the internet.
//
// createsRooms decides what happens when the room asked for does not exist.
// Creating it is what makes a demo work -- somebody types a room name and joins
// it -- and it is also an unbounded write available to whoever holds the
// credential, so it is a deployment's decision rather than a default.
func (s *Server) JoinHandler(createsRooms bool) http.Handler {
	mux := http.NewServeMux()
	limiter := &joinLimiter{per: howManyJoinsPerMinute, window: time.Minute, now: s.core.now}

	mux.HandleFunc(JoinPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a JSON body", http.StatusMethodNotAllowed)
			return
		}

		account, err := s.accountFor(r)
		if err != nil {
			// Uninformative on purpose, as the gRPC interceptor is: which
			// credential was wrong is not something an unauthenticated caller
			// learns.
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if !limiter.permit(account.Name) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		var asked JoinRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, howBigAJoinRequestMayBe)).
			Decode(&asked); err != nil {
			http.Error(w, "could not read the request", http.StatusBadRequest)
			return
		}
		if asked.Tenant == "" {
			asked.Tenant = account.Tenant
		}

		response, code, err := s.join(r.Context(), account, asked, createsRooms)
		if err != nil {
			// The reason, because every one of them is something the caller can
			// act on: a room that has ended, a name that will not normalise, a
			// credential that may not publish. This is a device's only route in
			// and a bare 400 here is the failure that has to be debugged from
			// the other side of a network.
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Never stored, anywhere, by anything in between. A token in a proxy
		// cache is a token issued to whoever asks next.
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc(RelayLinkPath, s.relayLink(limiter))
	mux.HandleFunc(RelayEnrollPath, s.relayEnroll(limiter))

	return mux
}

// join does the work, and is where the narrowing lives.
func (s *Server) join(
	ctx context.Context,
	account ServiceAccount,
	asked JoinRequest,
	createsRooms bool,
) (JoinResponse, int, error) {
	device, err := boundDevice(asked)
	if err != nil {
		return JoinResponse{}, http.StatusBadRequest, err
	}
	// Asked here as well as at minting so a device hears 402, which says what
	// to do about it, rather than the 409 the gRPC refusal would map to.
	if s.core.credits != nil {
		if err := s.core.credits.Allow(ctx, asked.Tenant); err != nil {
			return JoinResponse{}, http.StatusPaymentRequired, err
		}
	}

	if asked.Room != "" && createsRooms {
		if err := s.ensureRoom(ctx, asked.Tenant, asked.Room); err != nil {
			return JoinResponse{}, http.StatusBadRequest, err
		}
	}

	// The narrowing, and the reason this port can be open while the admin one is
	// not: no moderation, no ownership, and publish rights that are the
	// account's own rather than anything the caller named. A device asks who and
	// where; what it may do there was decided when the credential was issued.
	granted := control.Grants{Publish: account.Grants.Publish}

	// An account with no publish rights is refused here rather than served a
	// token that grants none. Both are safe; only one is legible. Minting it
	// produces a phone that connects, joins, appears to work, and carries no
	// media -- which is the failure this endpoint was built to remove, arriving
	// by a new route. An owner's credential does this: `is_owner` grants
	// everything through the admin API and leaves this field empty, so pointing
	// the endpoint at the wrong secret would otherwise fail silently and look
	// like a broken relay.
	if granted.Publish == 0 {
		return JoinResponse{}, http.StatusForbidden, fmt.Errorf(
			"account %s holds no publish rights, so a token minted from it could"+
				" publish nothing: give it publish scopes, or point this endpoint at"+
				" an account that has them", account.Name)
	}

	minted, err := tokensFor(s.core).MintClientToken(
		WithPrincipal(ctx, account),
		&adminpb.MintClientTokenRequest{
			Tenant:           asked.Tenant,
			Username:         asked.Username,
			DeviceThumbprint: device,
			Room:             asked.Room,
			Grants:           grantsToProto(granted),
			// Asked for explicitly rather than left to the default, so the
			// window a device holds is a decision here and not one inherited
			// from whatever the admin API happens to default to.
			Lifetime: durationpb.New(howLongADeviceTokenLasts),
		},
	)
	if err != nil {
		return JoinResponse{}, statusToHTTP(err), fmt.Errorf("%w", err)
	}

	return JoinResponse{
		Token:     minted.GetToken(),
		ExpiresAt: minted.GetExpiresAt().AsTime(),
		Room:      asked.Room,
		Username:  minted.GetIdentity().GetUsername(),
		KeyID:     minted.GetKeyId(),
		Device:    device,
	}, http.StatusOK, nil
}

// boundDevice decides which key the token will name.
//
// Derived from the JWK when there is one, because that is the only version of
// this that means anything: the relay demands a signature from whatever
// `cnf.jkt` names, so binding a token to a key the caller can prove it holds is
// the difference between a credential and a label. A bare thumbprint is still
// accepted, for the clients that predate proofs, and gets a token that will only
// work against a relay not requiring one.
//
// A request that carries both and disagrees is refused rather than resolved. The
// two readings -- "this client is confused about its own key" and "this client
// is trying to have a token minted for somebody else's" -- are not
// distinguishable from here, and neither is a request worth serving.
func boundDevice(asked JoinRequest) (string, error) {
	if asked.Key == nil {
		if asked.Device == "" {
			return "", errors.New("device or key is required")
		}
		return asked.Device, nil
	}

	thumbprint, err := asked.Key.Thumbprint()
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}

	// Compared against the prefix rather than the whole, because that is what a
	// client has: `DeviceID` truncates the thumbprint to sixteen characters for
	// the namespace, and it is the truncated form that a device tends to know
	// itself by and send here.
	if asked.Device != "" && !strings.HasPrefix(thumbprint, asked.Device) {
		return "", fmt.Errorf(
			"device %q is not the key sent with it, whose thumbprint is %q",
			asked.Device, thumbprint)
	}
	return thumbprint, nil
}

// howLongADeviceTokenLasts is the full cap.
//
// The maximum rather than something shorter, and that is not laziness. Fifteen
// minutes covers a phone that is asked for a token, put in a pocket, and taken
// out again; anything tighter starts failing for people rather than for
// attackers.
//
// This used to say the credential was checked when a session started and never
// again, which was true of the relay then and is not now: it closes a session
// whose token has lapsed, and a client presents a fresh one mid-call rather than
// reconnecting. So this lifetime bounds a stolen token rather than only the act
// of joining -- which is what a short lifetime was always supposed to be for,
// since a token here is a bearer credential and nothing else limits one.
const howLongADeviceTokenLasts = 15 * time.Minute

// ensureRoom creates the room if it is not there.
//
// Through the store rather than through the rooms service, deliberately, and the
// difference is the capability check: creating a room needs
// `change_room_policy`, which is not a capability a device credential should
// carry -- it is also what lets an account rewrite the policy of a room that
// already exists. So this creates and never modifies, and an existing room is
// left exactly as it is.
func (s *Server) ensureRoom(ctx context.Context, tenant, name string) error {
	switch _, err := s.core.store.GetRoom(ctx, tenant, name); {
	case err == nil:
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return err
	}

	room := control.Room{
		Tenant:    tenant,
		Room:      name,
		Lifecycle: control.LifecycleLive,
		CreatedAt: s.core.now(),
	}
	if err := room.Namespace().Validate(); err != nil {
		return err
	}
	switch _, err := s.core.store.CreateRoom(ctx, room); {
	// Two devices joining a new room at once is the ordinary case, not a race to
	// report: whoever lost has the room they wanted.
	case err == nil, errors.Is(err, store.ErrConflict):
		return nil
	default:
		return err
	}
}

// accountFor reads the bearer credential off a request.
func (s *Server) accountFor(r *http.Request) (ServiceAccount, error) {
	header := r.Header.Get("Authorization")
	credential := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if credential == header {
		credential = strings.TrimSpace(strings.TrimPrefix(header, "bearer "))
	}
	if credential == "" || credential == header {
		return ServiceAccount{}, ErrNoSuchAccount
	}

	account, err := s.core.accounts.Lookup(r.Context(), credential)
	if err != nil {
		return ServiceAccount{}, err
	}
	// A relay's credential is not a device's. Refused here as well as in
	// `requireTenant`, so the refusal does not depend on a later check
	// happening to run.
	if account.IsRelay || account.IsRecorder {
		return ServiceAccount{}, ErrNoSuchAccount
	}
	return account, nil
}

func tokensFor(c *core) *tokensService { return &tokensService{core: c} }

// statusToHTTP carries the gRPC code the mint path produced out to a status line.
//
// Mapped rather than flattened to 400, because the client does different things
// with each and the distinctions are already made upstream. A phone told 403
// should stop and say the credential will not do; told 404, that the room is not
// there; told 429 or 503, that trying again shortly is worth doing. Collapsing
// them produces a retry loop against a permanent refusal, which is the failure
// this endpoint exists to remove rather than to add.
func statusToHTTP(err error) int {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted:
		return http.StatusConflict
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// joinLimiter is a fixed window per credential.
//
// A window rather than a bucket because the thing being prevented is a flood,
// not a burst: two tokens in the same second is a phone retrying, and the
// difference between the two algorithms only matters at a scale this endpoint
// does not operate at.
type joinLimiter struct {
	per    int
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	seen  map[string]int
	since time.Time
}

func (l *joinLimiter) permit(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.seen == nil || now.Sub(l.since) >= l.window {
		l.seen = map[string]int{}
		l.since = now
	}
	l.seen[name]++
	return l.seen[name] <= l.per
}
