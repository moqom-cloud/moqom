package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/token"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The routes on the public listener that this package serves itself.
//
// There is no route here that turns a credential into a participant token. One
// used to exist, for deployments without a token server of their own, and it
// had to go: a credential a phone carries is a credential anybody with the app
// binary has, and the most it could ever say was "somebody has this app". A
// participant token is minted by the customer's backend, which knows who signed
// in, or by the Apple sign-in route, which asks Apple; see appleSignIn.
//
// What is left is for relays: a child relay's credential for its parent, and a
// managed relay VM's overlay identity. Both refuse every account that is not a
// relay's.

// TokenResponse is a minted credential and enough about it to use and display.
//
// The expiry is included because a token is opaque to whoever holds it: it is
// sealed for MOQOM's servers, and a client has nothing to decode it with.
type TokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Tenant    string    `json:"tenant,omitempty"`
	Room      string    `json:"room"`
	Username  string    `json:"username"`
	KeyID     string    `json:"keyId"`

	// Device is the thumbprint this token was bound to, untruncated, so a
	// caller can see which key the control plane decided the token belongs to.
	Device string `json:"device"`

	// Publish names what the token may publish, because the holder cannot read
	// it out of the token.
	Publish []string `json:"publish,omitempty"`

	// Direct is what the room allows of direct (device-to-device) paths.
	// Absent for a token that names no room.
	Direct *TokenDirect `json:"direct,omitempty"`

	// Relays are the addresses a token confined to a relay pool must dial.
	// Absent for the shared fleet, which is reached by its usual name.
	Relays []string `json:"relays,omitempty"`
}

// TokenDirect is control.DirectGrant as JSON.
type TokenDirect struct {
	Allowed         bool   `json:"allowed"`
	MaxParticipants uint32 `json:"maxParticipants"`

	// Stun is the relays' STUN responder. Absent when direct paths are not
	// allowed or the deployment names none.
	Stun *TokenStun `json:"stun,omitempty"`
}

// TokenStun is control.StunEndpoint as JSON.
type TokenStun struct {
	Host string `json:"host"`
	Port uint32 `json:"port"`
}

func tokenDirect(grant *adminpb.DirectGrant) *TokenDirect {
	if grant == nil {
		return nil
	}
	direct := &TokenDirect{Allowed: grant.GetAllowed(), MaxParticipants: grant.GetMaxParticipants()}
	if stun := grant.GetStun(); stun != nil {
		direct.Stun = &TokenStun{Host: stun.GetHost(), Port: stun.GetPort()}
	}
	return direct
}

// howBigAPublicRequestMayBe caps a body on the public listener.
//
// A few names and a device key. An ML-DSA-87 JWK is about 3.5 KB on its own,
// and each name may be up to control.MaxFieldBytes, so this leaves room for
// both; an Apple sign-in adds an identity token and an attestation, which have
// their own, larger cap.
const howBigAPublicRequestMayBe = 16 << 10

// howManyEnrollmentsPerMinute bounds one relay account's enrollments.
const howManyEnrollmentsPerMinute = 30

// PublicHandler serves the relay routes on the public listener.
//
// Returned separately from “HealthHandler“ because the two belong on different
// ports: readiness is for the orchestrator and stays closed to everything else.
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(RelayLinkPath, s.relayLink(
		&publicLimiter{per: howManyLinksPerMinute, window: time.Minute, now: s.core.now}))
	mux.HandleFunc(RelayEnrollPath, s.relayEnroll(
		&publicLimiter{per: howManyEnrollmentsPerMinute, window: time.Minute, now: s.core.now}))
	if s.core.apple != nil {
		mux.HandleFunc(AppleSignInPath, s.appleSignIn)
		mux.HandleFunc(AppleTokenPath, s.appleToken)
	}
	return mux
}

// boundDevice decides which key a token will name.
//
// Derived from the JWK, because the relay demands a signature from whatever
// `cnf.jkt` names, so binding a token to a key the caller can prove it holds is
// the difference between a credential and a label.
//
// device is optional and only ever checked against the key. A request that
// carries both and disagrees is refused rather than resolved: "this client is
// confused about its own key" and "this client is trying to have a token
// minted for somebody else's" are not distinguishable from here.
func boundDevice(key *token.PublicKey, device string) (string, error) {
	if key == nil {
		return "", errors.New("key is required: relays demand a proof from the key a token names")
	}
	thumbprint, err := key.Thumbprint()
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	// Compared against the prefix, because `DeviceID` truncates the
	// thumbprint to sixteen characters and that is the form a device tends to
	// know itself by.
	if device != "" && !strings.HasPrefix(thumbprint, device) {
		return "", fmt.Errorf(
			"device %q is not the key sent with it, whose thumbprint is %q",
			device, thumbprint)
	}
	return thumbprint, nil
}

// bearer reads the credential off an Authorization header.
func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	credential := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if credential == header {
		credential = strings.TrimSpace(strings.TrimPrefix(header, "bearer "))
	}
	if credential == "" || credential == header {
		return "", false
	}
	return credential, true
}

func tokensFor(c *core) *tokensService { return &tokensService{core: c} }

// statusToHTTP carries the gRPC code the mint path produced out to a status line.
//
// Mapped rather than flattened to 400, because the client does different things
// with each: told 403 it should stop; told 404, that the room is not there;
// told 429 or 503, that trying again shortly is worth doing.
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

// publicLimiter is a fixed window per name.
//
// A window rather than a bucket because the thing being prevented is a flood,
// not a burst.
type publicLimiter struct {
	per    int
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	seen  map[string]int
	since time.Time
}

func (l *publicLimiter) permit(name string) bool {
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
