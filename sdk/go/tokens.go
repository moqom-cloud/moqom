package moqom

import (
	"context"
	"errors"
	"time"

	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// TokenRequest asks for a token a client can join with.
//
// This is the call a backend makes most, and the one that decides who anybody is. The
// customer authenticates its user however it already does, then asks for a token naming
// that user, that device, that room, and exactly the rights they should have. MOQOM
// never sees the customer's password database.
type TokenRequest struct {
	// Username is the account this token speaks for -- the customer's own
	// identifier. MOQOM has no opinion about its shape beyond it being a string.
	Username string

	// DeviceThumbprint is the device public key's JWK thumbprint, base64url of the
	// SHA-256.
	//
	// The client generates the key in its secure element and never exports it, so
	// this is the only part of the device identity a backend handles. The device
	// field of every Identity for this session is derived from it, which is what
	// makes a device ban and this value the same fact.
	DeviceThumbprint string

	// Room is which room. Empty mints a token good for the tenant's discovery
	// surface but no room, which is what a client needs before it has chosen one.
	Room string

	// Publish is what they may send. A grant is permission, never an action.
	Publish PublishScope

	// Moderation is what they may moderate. Subject to the service account's own
	// ceiling: a key cannot mint a token more powerful than itself, which is what
	// keeps a leaked integration key from becoming an owner of every room.
	Moderation Capability

	// Preset replaces Moderation with the preset's expansion.
	Preset Preset

	// Owner makes this token's holder the room's owner: immune to moderation, and
	// not demotable. Still bounded by the account's ceiling.
	Owner bool

	// Lifetime is how long the token is good for. Zero takes the server's default.
	// Short on purpose -- a token is the one credential that leaves the building,
	// and until proof-of-possession lands a stolen one is replayable.
	Lifetime time.Duration

	// Backstage joins backstage rather than the room proper. The token names the
	// backstage namespace, and going live later is a namespace change rather than a
	// new token.
	Backstage bool

	// AgeVerified attests that this user's age has been verified, for rooms whose
	// policy demands it. The tenant performs the verification; this asserts it
	// happened.
	AgeVerified bool
}

// MintToken issues a client token.
//
// Not retried on an ambiguous failure, unlike everything else here, and that is
// deliberate: minting is cheap, has no idempotency key, and a second token is harmless
// where a second ban is not. Ask again.
func (c *Client) MintToken(ctx context.Context, request TokenRequest) (Token, error) {
	if request.Username == "" {
		return Token{}, errors.New("moqom: MintToken needs a username")
	}
	if request.DeviceThumbprint == "" {
		return Token{}, errors.New("moqom: MintToken needs a device thumbprint")
	}
	preset, err := presetToProto(request.Preset)
	if err != nil {
		return Token{}, err
	}

	wire := &adminpb.MintClientTokenRequest{
		Tenant:           c.tenant,
		Username:         request.Username,
		DeviceThumbprint: request.DeviceThumbprint,
		Room:             request.Room,
		Grants: grantsToProto(Grants{
			Publish:    request.Publish,
			Moderation: request.Moderation,
			IsOwner:    request.Owner,
		}),
		Preset:      preset,
		Lifetime:    durationToProto(request.Lifetime),
		Backstage:   request.Backstage,
		AgeVerified: request.AgeVerified,
	}

	response, err := c.tokens.MintClientToken(ctx, wire)
	if err != nil {
		return Token{}, wrap("MintToken", err)
	}
	return Token{
		Token:     response.GetToken(),
		ExpiresAt: timeFromProto(response.GetExpiresAt()),
		Identity:  identityFromProto(response.GetIdentity()),
		KeyID:     response.GetKeyId(),
	}, nil
}

// VerificationKeys returns the public keys tokens can be verified with.
//
// Served rather than configured so a key can be rotated without a deploy: a new key is
// published, tokens start being signed with it, and the old one is withdrawn once every
// token it signed has expired. A caller that verifies tokens itself should refetch
// periodically rather than caching forever.
func (c *Client) VerificationKeys(ctx context.Context) (VerificationKeys, error) {
	response, err := call(ctx, c, "VerificationKeys", func(ctx context.Context) (*adminpb.ListVerificationKeysResponse, error) {
		return c.tokens.ListVerificationKeys(ctx, &adminpb.ListVerificationKeysRequest{Tenant: c.tenant})
	})
	if err != nil {
		return VerificationKeys{}, err
	}

	keys := VerificationKeys{Binding: bindingFromProto(response.GetBinding())}
	for _, key := range response.GetKeys() {
		keys.Keys = append(keys.Keys, VerificationKey{
			KeyID:     key.GetKeyId(),
			Algorithm: key.GetAlgorithm(),
			JWK:       key.GetJwk(),
			NotBefore: timeFromProto(key.GetNotBefore()),
			RetiresAt: timeFromProto(key.GetRetiresAt()),
		})
	}
	return keys, nil
}

func bindingFromProto(wire adminpb.TokenBinding) TokenBinding {
	switch wire {
	case adminpb.TokenBinding_TOKEN_BINDING_BEARER:
		return BindingBearer
	case adminpb.TokenBinding_TOKEN_BINDING_DPOP:
		return BindingDPoP
	default:
		return ""
	}
}
