package billing

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Operators are MOQOM staff, and they are not tenants. A tenant's API key is a
// bearer secret a customer pastes into a browser; letting one of those reach
// every other customer's ledger would make each leaked key a breach of all of
// them. So the operator API takes a different credential entirely: a Google ID
// token for an allow-listed, verified address, minted for MOQOM's own OAuth
// client. A tenant key, a relay key and a control-plane service account all
// fail here before anything is looked up, because none of them is a JWT signed
// by Google.
//
// An ID token lives an hour, is bound to one OAuth client by its audience, and
// is issued only after Google's own sign-in, including whatever second factor
// the Workspace enforces. Nothing long-lived exists for an attacker to copy.

// ErrNotOperator is every way an operator credential can fail, as one error.
var ErrNotOperator = errors.New("not an operator credential")

// Google's ID token issuers. Both spellings are in use.
var googleIssuers = map[string]bool{"https://accounts.google.com": true, "accounts.google.com": true}

// GoogleCertsURL is where Google publishes the keys that sign ID tokens.
const GoogleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

// largestToken bounds what is parsed before the signature is checked.
const largestToken = 8 << 10

// clockSkew is how far a token's times may disagree with ours.
const clockSkew = time.Minute

// KeySource returns the RSA key a token names.
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// GoogleIdentity verifies Google ID tokens for the operator API.
type GoogleIdentity struct {
	// ClientID is MOQOM's OAuth client, the only audience accepted.
	ClientID string
	// Allowed are the operator addresses, lower case. Anybody else with a
	// Google account is refused, including others in the same Workspace.
	Allowed map[string]bool
	// HostedDomain, if set, must be the token's hd claim: the account is a
	// Workspace account of that domain, not a consumer account with a
	// look-alike address.
	HostedDomain string
	Keys         KeySource
	Now          func() time.Time
}

// Operator is who an operator request is from.
type Operator struct {
	Email string
	// Subject is Google's stable ID for the account, which survives an
	// address change.
	Subject string
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type googleClaims struct {
	Issuer        string          `json:"iss"`
	Audience      json.RawMessage `json:"aud"`
	Subject       string          `json:"sub"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	HostedDomain  string          `json:"hd"`
	IssuedAt      int64           `json:"iat"`
	Expires       int64           `json:"exp"`
}

// Verify checks a presented ID token and returns the operator it names.
func (g *GoogleIdentity) Verify(ctx context.Context, raw string) (Operator, error) {
	if g == nil || g.ClientID == "" || len(g.Allowed) == 0 || g.Keys == nil {
		return Operator{}, ErrNotOperator
	}
	if len(raw) > largestToken {
		return Operator{}, ErrNotOperator
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Operator{}, ErrNotOperator
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Operator{}, ErrNotOperator
	}
	var header jwtHeader
	if json.Unmarshal(headerJSON, &header) != nil {
		return Operator{}, ErrNotOperator
	}
	// One algorithm, named here rather than taken from the token: "none" and
	// an HMAC keyed with the public key are the two classic ways a verifier is
	// talked into accepting a token nobody signed.
	if header.Alg != "RS256" || header.Kid == "" {
		return Operator{}, ErrNotOperator
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Operator{}, ErrNotOperator
	}
	key, err := g.Keys.Key(ctx, header.Kid)
	if err != nil {
		return Operator{}, ErrNotOperator
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return Operator{}, ErrNotOperator
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Operator{}, ErrNotOperator
	}
	var claims googleClaims
	if json.Unmarshal(payload, &claims) != nil {
		return Operator{}, ErrNotOperator
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	at := now()
	switch {
	case !googleIssuers[claims.Issuer]:
		return Operator{}, ErrNotOperator
	case !audienceIs(claims.Audience, g.ClientID):
		return Operator{}, ErrNotOperator
	case claims.Expires == 0 || !at.Before(time.Unix(claims.Expires, 0).Add(clockSkew)):
		return Operator{}, ErrNotOperator
	case claims.IssuedAt == 0 || time.Unix(claims.IssuedAt, 0).After(at.Add(clockSkew)):
		return Operator{}, ErrNotOperator
	case !isTrue(claims.EmailVerified):
		return Operator{}, ErrNotOperator
	case g.HostedDomain != "" && claims.HostedDomain != g.HostedDomain:
		return Operator{}, ErrNotOperator
	case claims.Subject == "":
		return Operator{}, ErrNotOperator
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if !g.Allowed[email] {
		return Operator{}, ErrNotOperator
	}
	return Operator{Email: email, Subject: claims.Subject}, nil
}

func audienceIs(raw json.RawMessage, want string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == want
	}
	// A token for several audiences is a token another client can also
	// present; only one minted for this client alone is accepted.
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return len(many) == 1 && many[0] == want
	}
	return false
}

func isTrue(raw json.RawMessage) bool {
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var text string
	return json.Unmarshal(raw, &text) == nil && text == "true"
}

// ParseAllowList reads a comma-separated list of operator addresses.
func ParseAllowList(raw string) map[string]bool {
	allowed := map[string]bool{}
	for _, email := range strings.Split(raw, ",") {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			allowed[email] = true
		}
	}
	return allowed
}

// RemoteKeys fetches a JSON Web Key Set and caches it for as long as its
// Cache-Control allows, between five minutes and a day. An unknown key ID
// refetches, at most once a minute, so Google's rotation is picked up without
// a token naming a made-up kid turning into a request to Google every time.
type RemoteKeys struct {
	URL    string
	Client *http.Client
	Now    func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
	fetched time.Time
}

func (r *RemoteKeys) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *RemoteKeys) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if key, ok := r.keys[kid]; ok && now.Before(r.expires) {
		return key, nil
	}
	if r.keys == nil || !now.Before(r.expires) || now.Sub(r.fetched) >= time.Minute {
		if err := r.refresh(ctx, now); err != nil && r.keys == nil {
			return nil, err
		}
	}
	if key, ok := r.keys[kid]; ok {
		return key, nil
	}
	return nil, ErrNotOperator
}

func (r *RemoteKeys) refresh(ctx context.Context, now time.Time) error {
	r.fetched = now
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: %s", r.URL, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	keys, err := ParseJWKS(body)
	if err != nil {
		return err
	}
	ttl := 5 * time.Minute
	for _, directive := range strings.Split(response.Header.Get("Cache-Control"), ",") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(directive), "max-age="); ok {
			if seconds, err := strconv.Atoi(value); err == nil {
				ttl = min(max(time.Duration(seconds)*time.Second, 5*time.Minute), 24*time.Hour)
			}
		}
	}
	r.keys, r.expires = keys, now.Add(ttl)
	return nil
}

// ParseJWKS reads the RSA signing keys from a JSON Web Key Set.
func ParseJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Alg != "" && k.Alg != "RS256") || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: modulus, E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("the key set holds no RSA signing keys")
	}
	return keys, nil
}
