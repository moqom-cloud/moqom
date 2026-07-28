package server

import (
	"context"
	"fmt"

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

	username, err := control.NormaliseUsername(request.GetUsername())
	if err != nil {
		return nil, statusFor(err)
	}

	requested, err := grantsFromProto(request.GetGrants(), request.GetPreset())
	if err != nil {
		return nil, statusFor(err)
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
	if request.GetRoom() != "" {
		room, err := s.core.store.GetRoom(ctx, request.GetTenant(), request.GetRoom())
		if err != nil {
			return nil, statusFor(err)
		}
		if !room.IsOpen() {
			return nil, statusFor(fmt.Errorf("%w: %s has ended", control.ErrRoomClosed, room.Room))
		}
		if room.Policy.AgeGate.RequireVerification && !request.GetAgeVerified() {
			return nil, status.Error(codes.FailedPrecondition,
				"this room requires age verification, which the request does not assert")
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
		Audience:     request.GetTenant(),
		Confirmation: token.Confirmation{Thumbprint: request.GetDeviceThumbprint()},
		MOQT: token.ScopeFrom(
			requested,
			request.GetRoom(),
			request.GetBackstage(),
			request.GetAgeVerified(),
		),
		ArrivedVia: arrivedVia,
	}

	signing := s.core.keys[0]
	minted, expires, err := token.Mint(signing, claims, request.GetLifetime().AsDuration(), s.core.now())
	if err != nil {
		return nil, statusFor(err)
	}

	identity, err := claims.Identity()
	if err != nil {
		return nil, statusFor(err)
	}

	return &adminpb.MintClientTokenResponse{
		Token:     minted,
		ExpiresAt: timeToProto(expires),
		Identity:  identityToProto(identity),
		KeyId:     signing.ID,
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
		// Reported rather than assumed, so a client that wants to refuse bearer
		// tokens can find out without inspecting one. It will say DPOP when
		// proof-of-possession lands; until then saying BEARER is the honest answer.
		Binding: adminpb.TokenBinding_TOKEN_BINDING_BEARER,
	}
	for _, key := range s.core.keys {
		jwk, err := key.PublicJWK()
		if err != nil {
			return nil, statusFor(err)
		}
		response.Keys = append(response.Keys, &adminpb.VerificationKey{
			KeyId:     key.ID,
			Algorithm: "EdDSA",
			Jwk:       jwk,
		})
	}
	return response, nil
}
