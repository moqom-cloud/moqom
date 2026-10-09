package server

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

type tokensService struct {
	adminpb.UnimplementedTokensServer
	core *core
}

func (s *tokensService) MintClientToken(
	ctx context.Context,
	request *adminpb.MintClientTokenRequest,
) (*adminpb.MintClientTokenResponse, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	if request.GetDeviceThumbprint() == "" {
		return nil, status.Error(codes.InvalidArgument, "device_thumbprint is required")
	}
	if s.core.credits != nil {
		if err := s.core.credits.Allow(ctx, request.GetTenant()); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
	}

	username, err := control.NormaliseUsername(request.GetUsername())
	if err != nil {
		return nil, statusFor(err)
	}
	if username == RecorderSubject ||
		(s.core.recording != nil && request.GetDeviceThumbprint() == s.core.recording.thumbprint) {
		return nil, status.Error(codes.InvalidArgument, "that identity is reserved for the hosted recorder")
	}
	if username == RelaySubject {
		return nil, status.Error(codes.InvalidArgument, "that identity is reserved for relay links")
	}

	requested, err := grantsFromProto(request.GetGrants(), request.GetPreset())
	if err != nil {
		return nil, statusFor(err)
	}

	// A recorder or classifier the tenant runs, joining the room's encrypted
	// group so it can decrypt. Its username is the one thing every member's
	// device sees of it — each holds back its media until the room discloses the
	// watchdog the name points at — so the name is minted only for a watchdog
	// that is attached to this room, and the token lets it watch and nothing else.
	if watchdog, ok := strings.CutPrefix(username, control.AgentUsernamePrefix); ok {
		if err := s.agentMayJoin(ctx, request.GetTenant(), request.GetRoom(), watchdog); err != nil {
			return nil, err
		}
		requested = control.Grants{}
	}

	// The ceiling, and the reason it exists.
	//
	// Without this, any key that can mint can mint an owner, and a leaked
	// integration key is a takeover of every room in the tenant. With it, a key
	// issued so that users can join rooms cannot produce a moderator, whatever the
	// request body says.
	if !account.Grants.CanGrant(requested.Moderation) {
		return nil, status.Errorf(codes.PermissionDenied,
			"account %s holds %s and cannot mint %s",
			account.Name, account.Grants.Moderation, requested.Moderation)
	}
	if requested.IsOwner && !account.Grants.IsOwner {
		return nil, status.Errorf(codes.PermissionDenied, "account %s cannot mint an owner token", account.Name)
	}
	if !account.Grants.IsOwner && !account.Grants.Publish.Has(requested.Publish) {
		return nil, status.Errorf(codes.PermissionDenied,
			"account %s cannot mint publish rights it does not hold", account.Name)
	}

	// A room that exists, is open, and whose policy this token satisfies. Checked at
	// minting because it is the last moment a backend can be told why: a token that
	// fails at the relay produces a client-side error nobody can explain.
	var direct *adminpb.DirectGrant
	if request.GetRoom() != "" {
		room, err := s.core.store.GetRoom(ctx, request.GetTenant(), request.GetRoom())
		if err != nil {
			return nil, statusFor(err)
		}
		direct = directGrantToProto(room.Policy.Direct())
		if direct.GetAllowed() && s.core.stun != nil {
			direct.Stun = stunToProto(*s.core.stun)
		}
		if !room.IsOpen() {
			return nil, statusFor(fmt.Errorf("%w: %s has ended", control.ErrRoomClosed, room.Room))
		}
		if room.Policy.AgeGate.RequireVerification && !request.GetAgeVerified() {
			return nil, status.Error(codes.FailedPrecondition,
				"this room requires age verification, which the request does not assert")
		}

		// A moderator's word outranks the backend's request. A demoted guest's
		// app renewing its token as usual gets one for what they now hold, so the
		// token it presents agrees with what the relay enforces; and a promotion
		// set through SetGrants is the most a token minted afterwards can carry.
		// Narrowed rather than refused, because the backend asking is usually
		// doing nothing wrong: it is renewing a role it has not heard changed.
		device, err := control.DeviceFromThumbprint(request.GetDeviceThumbprint())
		if err != nil {
			return nil, statusFor(err)
		}
		ceiling, overridden, err := s.core.grantCeiling(ctx, request.GetTenant(), request.GetRoom(),
			control.Identity{Username: username, Device: device})
		if err != nil {
			return nil, statusFor(err)
		}
		if overridden {
			requested.Publish &= ceiling.Publish
			requested.Moderation &= ceiling.Moderation
		}
	}

	// Normalised the same way the subject is, because it is compared against
	// usernames: attribution to "Bea" that never matches the participant named
	// "bea" is a cohort that silently reads as organic.
	arrivedVia := request.GetArrivedVia()
	if arrivedVia != "" {
		arrivedVia, err = control.NormaliseUsername(arrivedVia)
		if err != nil {
			return nil, statusFor(fmt.Errorf("arrived_via: %w", err))
		}
	}

	claims := token.Claims{
		Issuer:       s.core.issuer,
		Subject:      username,
		Tenant:       request.GetTenant(),
		Confirmation: token.Confirmation{Thumbprint: request.GetDeviceThumbprint()},
		Grant: token.ScopeFrom(
			requested,
			request.GetRoom(),
			request.GetBackstage(),
			request.GetAgeVerified(),
		),
		Via: arrivedVia,
	}

	relays, err := s.core.confine(ctx, &claims)
	if err != nil {
		return nil, statusFor(err)
	}

	signing := s.core.keys[0]
	minted, expires, err := s.core.mint(claims, request.GetLifetime().AsDuration())
	if err != nil {
		return nil, statusFor(err)
	}

	identity, err := token.Identity(claims)
	if err != nil {
		return nil, statusFor(err)
	}

	return &adminpb.MintClientTokenResponse{
		Token:     minted,
		ExpiresAt: timeToProto(expires),
		Identity:  identityToProto(identity),
		KeyId:     signing.ID,
		Direct:    direct,
		Relays:    relays,
	}, nil
}

func (s *tokensService) ListVerificationKeys(
	ctx context.Context,
	request *adminpb.ListVerificationKeysRequest,
) (*adminpb.ListVerificationKeysResponse, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}

	response := &adminpb.ListVerificationKeysResponse{
		// Every relay requires proof of possession and no token is minted
		// without a device key to prove.
		Binding: adminpb.TokenBinding_TOKEN_BINDING_DPOP,
	}
	for _, key := range s.core.keys {
		jwk, err := key.PublicJWK()
		if err != nil {
			return nil, statusFor(err)
		}
		response.Keys = append(response.Keys, &adminpb.VerificationKey{
			KeyId:     key.ID,
			Algorithm: key.Algorithm,
			Jwk:       jwk,
		})
	}
	return response, nil
}

// agentMayJoin refuses an agent token unless its watchdog is attached to the
// room and has not failed.
func (s *tokensService) agentMayJoin(ctx context.Context, tenant, room, watchdog string) error {
	if room == "" {
		return status.Error(codes.InvalidArgument, "an agent token names the room its watchdog is attached to")
	}
	attached, err := s.core.store.ListWatchdogs(ctx, tenant, room)
	if err != nil {
		return statusFor(err)
	}
	for _, candidate := range attached {
		if candidate.ID == watchdog {
			if candidate.Health == control.WatchdogFailed {
				return status.Errorf(codes.FailedPrecondition, "watchdog %s has failed; reattach it", watchdog)
			}
			return nil
		}
	}
	return status.Errorf(codes.FailedPrecondition,
		"no watchdog %s is attached to %s; attach it first, so the room is told", watchdog, room)
}
