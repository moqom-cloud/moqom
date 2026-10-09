package token

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"

	sectoken "github.com/moqom-cloud/moqom-security/go/token"
)

// Sealing a token for MOQOM's servers.
//
// A signed token is sealed with AES-256-GCM before it leaves the control plane,
// so whoever holds it -- the phone, a log line, a crash report -- holds bytes
// that say nothing about who it names or what it grants. A relay opens it with a
// seal key and then verifies the signature as before.
//
// A seal key that opens every tenant's tokens belongs only on relays MOQOM runs
// for every tenant. A tenant with relays of its own -- a customer's, or a
// private pool -- has its tokens sealed with a key derived from the deployment's
// key and its name, and those relays are given that key alone: holding it opens
// that tenant's tokens and nobody else's, and nothing about it leads back to the
// key it came from.

// TenantSealKey derives the seal key for one tenant's tokens from a deployment
// seal key.
//
// The identifier is a MAC of the tenant under the deployment key, so it is
// stable for the tenant and names nobody to whoever reads a sealed token; the
// secret is HKDF over the same inputs. Both change when the deployment key does,
// so rotating that rotates every tenant's.
func TenantSealKey(master sectoken.SealKey, tenant string) (sectoken.SealKey, error) {
	mac := hmac.New(sha256.New, master.Secret())
	mac.Write([]byte("moqom tenant seal key id\x00"))
	mac.Write([]byte(tenant))
	id := mac.Sum(nil)[:sectoken.SealKeyIDSize]

	secret, err := hkdf.Key(sha256.New, master.Secret(), id, "moqom tenant seal key\x00"+tenant, 32)
	if err != nil {
		return sectoken.SealKey{}, err
	}
	return sectoken.NewSealKey(id, secret)
}
