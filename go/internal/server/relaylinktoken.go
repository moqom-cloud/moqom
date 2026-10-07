package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// The credential a relay presents to its parent.
//
// An edge in a tier reads from a parent relay with a session of its own, and
// that session is not a person: the people are the edge's viewers, metered
// where they are connected. Presented with an ordinary viewer token the parent
// would meter the link as one more viewer and every one of them would be billed
// twice. So the link carries a grant saying what it is -- `moqt.rly` -- and the
// parent does not meter a session that holds it.
//
// The grant is worth not paying for, which decides where it can come from: a
// signed claim, minted here and nowhere else, for relay service accounts only.
// Not on the admin API, where a customer's backend could ask for it; not on
// /join, which refuses relay credentials outright. And it is read-only: no
// publish rights and no powers, which the relay enforces as well by refusing a
// link claim beside either, so holding one buys reading a room and nothing more.

// RelayLinkPath is where a relay asks for its link credential.
const RelayLinkPath = "/relay/link"

// RelaySubject is the username every relay-link token carries.
//
// Reserved like RecorderSubject, so that a link on a parent's roster is
// recognisable as one and a backend cannot mint a person who looks like it.
const RelaySubject = "moqom-relay"

// RelayLinkRequest is what a relay asks for: a link into one room of one
// tenant, bound to a key it holds.
//
// One room per credential, and the room is required. A parent refuses any
// request on the link outside the room the token names, so an edge holding the
// credential for one room cannot read another however it is compromised; a
// credential naming no room would be a read of the whole tenant.
//
// The key, not a bare thumbprint. A parent requires a proof of possession from
// the key the token names, and deriving the thumbprint here from the key itself
// is what makes the binding a statement about a key the edge holds.
type RelayLinkRequest struct {
	Tenant string           `json:"tenant"`
	Room   string           `json:"room"`
	Device string           `json:"device"`
	Key    *token.PublicKey `json:"key,omitempty"`
}

// relayLinkLifetime is a third of the cap, because it is also how long a
// revocation takes. Nothing tells a parent that a room has ended or a relay
// account has been withdrawn: it learns when the edge cannot renew, and the
// credential it holds lapses. An edge renews well inside this, so the cost of a
// short lifetime is a request every few minutes per room it serves.
const relayLinkLifetime = 5 * time.Minute

// howManyLinksPerMinute bounds one relay account, which asks once per room it
// serves every few minutes rather than once per person, and so needs a higher
// ceiling than a join backend.
const howManyLinksPerMinute = 600

func (s *Server) relayLink(limiter *joinLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a JSON body", http.StatusMethodNotAllowed)
			return
		}
		account, err := s.relayAccountFor(r)
		if err != nil {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if !limiter.permit("relay-link:" + account.Name) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		var asked RelayLinkRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, howBigAJoinRequestMayBe)).
			Decode(&asked); err != nil {
			http.Error(w, "could not read the request", http.StatusBadRequest)
			return
		}
		if asked.Tenant == "" {
			http.Error(w, "tenant is required", http.StatusBadRequest)
			return
		}
		if asked.Room == "" {
			http.Error(w, "room is required: a link credential names one room", http.StatusBadRequest)
			return
		}
		if asked.Key == nil {
			http.Error(w, "key is required: a link credential is bound to the relay's key", http.StatusBadRequest)
			return
		}
		if account.Tenant != "" && account.Tenant != asked.Tenant {
			http.Error(w, "this relay account is scoped to another tenant", http.StatusForbidden)
			return
		}
		device, err := boundDevice(JoinRequest{Device: asked.Device, Key: asked.Key})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Asked at every renewal, which is what makes ending a room reach the
		// edges: the next renewal is refused, and the link lapses with the
		// credential it holds.
		switch room, err := s.core.store.GetRoom(r.Context(), asked.Tenant, asked.Room); {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such room", http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, "could not read the room", http.StatusServiceUnavailable)
			return
		case !room.IsOpen():
			http.Error(w, control.ErrRoomClosed.Error(), http.StatusGone)
			return
		}

		claims := token.Claims{
			Issuer:       s.core.issuer,
			Subject:      RelaySubject,
			Audience:     asked.Tenant,
			Confirmation: token.Confirmation{Thumbprint: device},
			MOQT:         token.Scope{Room: asked.Room, Relay: true},
		}
		minted, expires, err := token.Mint(s.core.keys[0], claims, relayLinkLifetime, s.core.now())
		if err != nil {
			http.Error(w, "could not mint the link credential", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(JoinResponse{
			Token:     minted,
			ExpiresAt: expires,
			Room:      asked.Room,
			Username:  RelaySubject,
			KeyID:     s.core.keys[0].ID,
			Device:    device,
		})
	}
}

// relayAccountFor reads the bearer credential off a request and accepts only a
// relay service account.
func (s *Server) relayAccountFor(r *http.Request) (ServiceAccount, error) {
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
	if !account.IsRelay {
		return ServiceAccount{}, errors.New("not a relay account")
	}
	return account, nil
}
