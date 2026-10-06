package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/moqom-cloud/moqom/go/internal/mls"
)

// KeyDeliveryHandler serves the MLS delivery service under mls.PathPrefix,
// for devices, on the listener that serves JoinPath.
//
// Authenticated by the participant token the device already holds for the
// room: the tenant is its audience, the room its scope, and the device the
// identity its thumbprint implies. A token for no room is refused, since a
// group belongs to one.
//
// Account and device bans are checked on every request, not only at join. A
// banned device's token stays valid until it expires, and without this it
// could keep claiming places in the group; refused here, it stops being heard
// from, and the room's next commit removes it, which rotates the room's keys.
// Address bans are left to the relay, which sees the address media comes from.
func (s *Server) KeyDeliveryHandler(service *mls.Service) http.Handler {
	return mls.Handler(service, func(r *http.Request) (mls.Caller, error) {
		header := r.Header.Get("Authorization")
		credential, found := strings.CutPrefix(header, "Bearer ")
		if !found {
			return mls.Caller{}, errors.New("no bearer credential")
		}
		now := s.core.now()
		claims, err := s.core.verifier.Verify(strings.TrimSpace(credential), now)
		if err != nil {
			return mls.Caller{}, err
		}
		if claims.MOQT.Room == "" {
			return mls.Caller{}, errors.New("token names no room")
		}
		identity, err := claims.Identity()
		if err != nil {
			return mls.Caller{}, err
		}
		bans, err := s.core.store.ListBans(r.Context(), claims.Audience, claims.MOQT.Room)
		if err != nil {
			return mls.Caller{}, err
		}
		for _, ban := range bans {
			if ban.Matches(identity, "", now) {
				return mls.Caller{}, errors.New("banned")
			}
		}
		return mls.Caller{
			Room:   mls.Room{Tenant: claims.Audience, Room: claims.MOQT.Room},
			Device: identity.String(),
		}, nil
	})
}
