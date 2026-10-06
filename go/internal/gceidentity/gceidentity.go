// Package gceidentity verifies a Compute Engine instance's identity token.
//
// A VM can ask its metadata server for a token, signed by Google, saying which
// instance it is, in which project and zone, running as which service account
// -- and for which audience, so a token minted for one recipient cannot be
// replayed to another. The control plane takes one from each managed relay
// before minting it an overlay identity: a stolen relay credential alone does
// not mint anything, because only the VM can produce the token.
//
// The token must be the "full" format, which carries the instance claims.
package gceidentity

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrNotTrusted is every way a token can fail, as one error.
var ErrNotTrusted = errors.New("not a trusted instance identity")

// Instance is what a verified token says.
type Instance struct {
	ID      string
	Name    string
	Zone    string
	Project string
	// ServiceAccount is the address the instance runs as.
	ServiceAccount string
}

// KeySource returns the RSA key a token names. billing.RemoteKeys over
// Google's certificate endpoint is one.
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Policy is which instances are trusted.
type Policy struct {
	// Audience is the only audience accepted.
	Audience string
	// Project is the only project accepted, by ID.
	Project string
	// NamePrefix, if set, must begin the instance's name.
	NamePrefix string
	// ServiceAccounts are the addresses an instance may run as, lower case.
	ServiceAccounts map[string]bool
	Keys            KeySource
	Now             func() time.Time
}

const (
	largestToken = 8 << 10
	clockSkew    = time.Minute
)

var issuers = map[string]bool{"https://accounts.google.com": true, "accounts.google.com": true}

type claims struct {
	Issuer        string          `json:"iss"`
	Audience      json.RawMessage `json:"aud"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
	IssuedAt      int64           `json:"iat"`
	Expires       int64           `json:"exp"`
	Google        struct {
		ComputeEngine struct {
			InstanceID   string `json:"instance_id"`
			InstanceName string `json:"instance_name"`
			ProjectID    string `json:"project_id"`
			Zone         string `json:"zone"`
		} `json:"compute_engine"`
	} `json:"google"`
}

// Verify checks a token against the policy.
func (p Policy) Verify(ctx context.Context, raw string) (Instance, error) {
	if p.Audience == "" || p.Project == "" || len(p.ServiceAccounts) == 0 || p.Keys == nil {
		return Instance{}, ErrNotTrusted
	}
	if len(raw) > largestToken {
		return Instance{}, ErrNotTrusted
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Instance{}, ErrNotTrusted
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Instance{}, ErrNotTrusted
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(headerJSON, &header) != nil || header.Alg != "RS256" || header.Kid == "" {
		return Instance{}, ErrNotTrusted
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Instance{}, ErrNotTrusted
	}
	key, err := p.Keys.Key(ctx, header.Kid)
	if err != nil {
		return Instance{}, ErrNotTrusted
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return Instance{}, ErrNotTrusted
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Instance{}, ErrNotTrusted
	}
	var c claims
	if json.Unmarshal(payload, &c) != nil {
		return Instance{}, ErrNotTrusted
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	at := now()
	engine := c.Google.ComputeEngine
	email := strings.ToLower(c.Email)
	switch {
	case !issuers[c.Issuer]:
	case !audienceIs(c.Audience, p.Audience):
	case c.Expires == 0 || !at.Before(time.Unix(c.Expires, 0).Add(clockSkew)):
	case c.IssuedAt == 0 || time.Unix(c.IssuedAt, 0).After(at.Add(clockSkew)):
	case engine.InstanceID == "" || engine.InstanceName == "":
	case engine.ProjectID != p.Project:
	case !strings.HasPrefix(engine.InstanceName, p.NamePrefix):
	case !c.EmailVerified || !p.ServiceAccounts[email]:
	default:
		return Instance{
			ID: engine.InstanceID, Name: engine.InstanceName, Zone: engine.Zone,
			Project: engine.ProjectID, ServiceAccount: email,
		}, nil
	}
	return Instance{}, ErrNotTrusted
}

func audienceIs(raw json.RawMessage, want string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == want
	}
	var many []string
	return json.Unmarshal(raw, &many) == nil && len(many) == 1 && many[0] == want
}
