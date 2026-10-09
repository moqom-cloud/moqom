// Package apple checks what Apple vouches for: a Sign in with Apple identity
// token, which names a person, and an App Attest attestation or assertion,
// which says a request comes from a genuine copy of an app on a real Apple
// device.
//
// Both are signed by Apple with classical algorithms (RS256 and ECDSA P-256).
// Neither is a MOQOM identity: a device's identity is its ML-DSA-87 key, which
// every token is bound to. What these decide is whether to mint for that key.
package apple

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
	"slices"
	"strings"
	"sync"
	"time"
)

// Issuer and KeysURL are Sign in with Apple's, as Apple documents them for
// verifying an identity token on a server.
const (
	Issuer  = "https://appleid.apple.com"
	KeysURL = "https://appleid.apple.com/auth/keys"
)

// ErrIdentity is an identity token that does not verify.
var ErrIdentity = errors.New("the Apple identity token is not valid")

// Identity is what a verified identity token says.
type Identity struct {
	// Subject is Apple's stable identifier for this person in one team's apps.
	Subject string
	// Audience is the bundle ID the token was issued to.
	Audience string
	// Nonce is what the app put in the request. MOQOM's clients set it to the
	// device key's thumbprint, which binds the sign-in to that key.
	Nonce string
}

// IdentityVerifier checks identity tokens: RS256 under one of Apple's
// published keys, issued by Apple, unexpired.
type IdentityVerifier struct {
	// KeysURL is where Apple's keys are fetched; tests point it elsewhere.
	KeysURL string
	Client  *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewIdentityVerifier fetches Apple's keys from KeysURL.
func NewIdentityVerifier() *IdentityVerifier {
	return &IdentityVerifier{KeysURL: KeysURL, Client: &http.Client{Timeout: 10 * time.Second}}
}

// keyRefetchInterval bounds how often an unknown key ID sends this to Apple,
// so a stream of tokens naming made-up keys cannot turn it into a proxy.
const keyRefetchInterval = time.Minute

// keyLifetime is how long a fetched key set is used before it is refreshed.
const keyLifetime = time.Hour

// maxIdentityTokenBytes bounds what is parsed before a signature is checked.
const maxIdentityTokenBytes = 8192

func (v *IdentityVerifier) key(ctx context.Context, id string, now time.Time) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[id]; ok && now.Sub(v.fetched) < keyLifetime {
		return key, nil
	}
	if now.Sub(v.fetched) < keyRefetchInterval {
		if key, ok := v.keys[id]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("%w: unknown key %q", ErrIdentity, id)
	}
	keys, err := v.fetch(ctx)
	if err != nil {
		if key, ok := v.keys[id]; ok {
			return key, nil
		}
		return nil, err
	}
	v.keys, v.fetched = keys, now
	key, ok := keys[id]
	if !ok {
		return nil, fmt.Errorf("%w: unknown key %q", ErrIdentity, id)
	}
	return key, nil
}

func (v *IdentityVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.KeysURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := v.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetching Apple's keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Apple's keys: %s", response.Status)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("reading Apple's keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) > 4 {
			continue
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if key.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("Apple published no usable keys")
	}
	return keys, nil
}

// Verify checks an identity token at now, issued to one of audiences.
func (v *IdentityVerifier) Verify(ctx context.Context, token string, audiences []string, now time.Time) (Identity, error) {
	if len(token) == 0 || len(token) > maxIdentityTokenBytes {
		return Identity{}, fmt.Errorf("%w: length", ErrIdentity)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, fmt.Errorf("%w: not a JWS", ErrIdentity)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "RS256" {
		return Identity{}, fmt.Errorf("%w: header", ErrIdentity)
	}
	key, err := v.key(ctx, header.Kid, now)
	if err != nil {
		return Identity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, fmt.Errorf("%w: signature encoding", ErrIdentity)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return Identity{}, fmt.Errorf("%w: signature", ErrIdentity)
	}
	var claims struct {
		Iss   string `json:"iss"`
		Aud   string `json:"aud"`
		Sub   string `json:"sub"`
		Exp   int64  `json:"exp"`
		Iat   int64  `json:"iat"`
		Nonce string `json:"nonce"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return Identity{}, fmt.Errorf("%w: claims", ErrIdentity)
	}
	const skew = 60
	switch {
	case claims.Iss != Issuer:
		return Identity{}, fmt.Errorf("%w: issuer", ErrIdentity)
	case !slices.Contains(audiences, claims.Aud):
		return Identity{}, fmt.Errorf("%w: audience %q", ErrIdentity, claims.Aud)
	case claims.Sub == "":
		return Identity{}, fmt.Errorf("%w: no subject", ErrIdentity)
	case now.Unix() > claims.Exp+skew:
		return Identity{}, fmt.Errorf("%w: expired", ErrIdentity)
	case claims.Iat > now.Unix()+skew:
		return Identity{}, fmt.Errorf("%w: issued in the future", ErrIdentity)
	}
	return Identity{Subject: claims.Sub, Audience: claims.Aud, Nonce: claims.Nonce}, nil
}

func decodeSegment(segment string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
