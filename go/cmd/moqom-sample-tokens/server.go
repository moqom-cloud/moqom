package main

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	moqom "github.com/moqom-cloud/moqom-go"
	"github.com/moqom-cloud/moqom-security/go/proof"
)

const (
	// tokenLifetime is the control plane's ceiling, asked for explicitly.
	tokenLifetime = 15 * time.Minute
	// enrollmentLifetime is how long a phone stays signed in without signing in
	// with Apple again.
	enrollmentLifetime = 30 * 24 * time.Hour
	// mintsPerWindow per person per window is several times what renewing a
	// token in one room needs, and far short of what scripting it would want.
	mintsPerWindow = 30
	mintWindow     = 10 * time.Minute
	maxBody        = 16 << 10
)

// tokenMinter is the part of the moqom client this needs.
type tokenMinter interface {
	MintToken(context.Context, moqom.TokenRequest) (moqom.Token, error)
}

type server struct {
	minter tokenMinter
	apple  *appleVerifier
	sealer *enrollmentSealer
	rooms  []string
	tenant string
	now    func() time.Time
	limits *limiter
}

func newServer(minter tokenMinter, apple *appleVerifier, sealer *enrollmentSealer, tenant string, rooms []string) *server {
	return &server{
		minter: minter,
		tenant: tenant,
		apple:  apple,
		sealer: sealer,
		rooms:  rooms,
		now:    time.Now,
		limits: newLimiter(mintsPerWindow, mintWindow),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sign-in", s.signIn)
	mux.HandleFunc("POST /v1/token", s.token)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

type signInRequest struct {
	IdentityToken string    `json:"identityToken"`
	Key           proof.JWK `json:"key"`
	Room          string    `json:"room"`
}

type tokenRequest struct {
	Key  proof.JWK `json:"key"`
	Room string    `json:"room"`
}

type direct struct {
	Allowed         bool   `json:"allowed"`
	MaxParticipants uint32 `json:"maxParticipants"`
	Stun            *stun  `json:"stun,omitempty"`
}

type stun struct {
	Host string `json:"host"`
	Port uint16 `json:"port"`
}

// tokenResponse is what the SDK's TokenService reads, the same whichever
// backend sends it. Everything a client needs to know about the token is here,
// because the token itself is sealed for MOQOM's servers and cannot be read.
type tokenResponse struct {
	Token               string     `json:"token"`
	ExpiresAt           time.Time  `json:"expiresAt"`
	Tenant              string     `json:"tenant"`
	Room                string     `json:"room"`
	Username            string     `json:"username"`
	Device              string     `json:"device"`
	KeyID               string     `json:"keyId"`
	Publish             []string   `json:"publish"`
	Direct              *direct    `json:"direct,omitempty"`
	Enrollment          string     `json:"enrollment,omitempty"`
	EnrollmentExpiresAt *time.Time `json:"enrollmentExpiresAt,omitempty"`
}

func (s *server) signIn(w http.ResponseWriter, r *http.Request) {
	var asked signInRequest
	if !decode(w, r, &asked) {
		return
	}
	thumbprint, ok := s.thumbprint(w, asked.Key)
	if !ok {
		return
	}
	now := s.now()
	identity, err := s.apple.verify(r.Context(), asked.IdentityToken, now)
	if err != nil {
		slog.Info("sign-in refused", "error", err)
		fail(w, http.StatusUnauthorized, "sign in with Apple again")
		return
	}
	if identity.Nonce != thumbprint {
		fail(w, http.StatusUnauthorized, "the identity token's nonce is not this device key's thumbprint")
		return
	}
	expires := now.Add(enrollmentLifetime).Truncate(time.Second)
	sealed, err := s.sealer.seal(enrollment{Subject: identity.Subject, Thumbprint: thumbprint, Expires: expires.Unix()})
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not enroll")
		return
	}
	response, ok := s.mint(w, r.Context(), identity.Subject, thumbprint, asked.Room)
	if !ok {
		return
	}
	response.Enrollment, response.EnrollmentExpiresAt = sealed, &expires
	reply(w, response)
}

func (s *server) token(w http.ResponseWriter, r *http.Request) {
	bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || bearer == "" {
		fail(w, http.StatusUnauthorized, "sign in with Apple first")
		return
	}
	enrolled, err := s.sealer.open(bearer, s.now())
	if err != nil {
		fail(w, http.StatusUnauthorized, "sign in with Apple again")
		return
	}
	var asked tokenRequest
	if !decode(w, r, &asked) {
		return
	}
	thumbprint, ok := s.thumbprint(w, asked.Key)
	if !ok {
		return
	}
	if thumbprint != enrolled.Thumbprint {
		fail(w, http.StatusUnauthorized, "this enrollment belongs to a different device key")
		return
	}
	response, ok := s.mint(w, r.Context(), enrolled.Subject, thumbprint, asked.Room)
	if !ok {
		return
	}
	reply(w, response)
}

func (s *server) thumbprint(w http.ResponseWriter, key proof.JWK) (string, bool) {
	if _, err := key.Algorithm(); err != nil {
		fail(w, http.StatusBadRequest, "key must be an ML-DSA-87 device key")
		return "", false
	}
	thumbprint, err := key.Thumbprint()
	if err != nil {
		fail(w, http.StatusBadRequest, "key is not a valid ML-DSA-87 public key")
		return "", false
	}
	return thumbprint, true
}

func (s *server) mint(w http.ResponseWriter, ctx context.Context, subject, thumbprint, room string) (tokenResponse, bool) {
	if !slices.Contains(s.rooms, room) {
		fail(w, http.StatusNotFound, "no such room")
		return tokenResponse{}, false
	}
	if !s.limits.allow(subject, s.now()) {
		w.Header().Set("Retry-After", fmt.Sprint(int(mintWindow.Seconds())))
		fail(w, http.StatusTooManyRequests, "too many tokens; try again later")
		return tokenResponse{}, false
	}
	minted, err := s.minter.MintToken(ctx, moqom.TokenRequest{
		Username:         usernameFor(subject),
		DeviceThumbprint: thumbprint,
		Room:             room,
		Publish:          moqom.PublishFull,
		Lifetime:         tokenLifetime,
	})
	switch {
	case errors.Is(err, moqom.ErrNotFound), errors.Is(err, moqom.ErrClosed):
		fail(w, http.StatusNotFound, "no such room")
		return tokenResponse{}, false
	case err != nil:
		slog.Error("minting", "error", err)
		fail(w, http.StatusBadGateway, "could not mint a token")
		return tokenResponse{}, false
	}
	response := tokenResponse{
		Token:     minted.Token,
		ExpiresAt: minted.ExpiresAt,
		Tenant:    s.tenant,
		Room:      room,
		Username:  minted.Identity.Username,
		Device:    thumbprint,
		KeyID:     minted.KeyID,
		Publish:   []string{"audio", "video", "screen_share", "data"},
	}
	if grant := minted.Direct; grant != nil {
		response.Direct = &direct{Allowed: grant.Allowed, MaxParticipants: grant.MaxParticipants}
		if grant.Stun != nil {
			response.Direct.Stun = &stun{Host: grant.Stun.Host, Port: grant.Stun.Port}
		}
	}
	return response, true
}

// usernameFor is the username a person signs in as: derived from Apple's
// subject rather than equal to it, so the subject itself never reaches the
// control plane, the relays or the other people in a room.
func usernameFor(subject string) string {
	digest := sha256.Sum256([]byte("moqom-sample-tokens username\x00" + subject))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10])
	return "apple-" + strings.ToLower(encoded)
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := decoder.Decode(into); err != nil {
		fail(w, http.StatusBadRequest, "the body is not the JSON this expects")
		return false
	}
	return true
}

func reply(w http.ResponseWriter, response tokenResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

func fail(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, reason, code)
}

// limiter counts mints per person in fixed windows.
type limiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	counts  map[string]int
	started time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, counts: map[string]int{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.started) >= l.window {
		clear(l.counts)
		l.started = now
	}
	if l.counts[key] >= l.limit {
		return false
	}
	l.counts[key]++
	return true
}
