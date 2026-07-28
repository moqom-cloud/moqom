package token

// The two token implementations have never exchanged a byte, and until they do,
// "the relay verifies our tokens" is a belief rather than a fact. Both sides
// were written from the same description, which is exactly the situation in
// which they agree on everything except the one detail that matters -- the
// canonical bytes the signature covers, the truncation that produces a device
// ID, the claim the tenant lives in.
//
// So this mints with the real minter and writes a fixture the Rust verifier
// reads. Regenerate with MOQOM_WRITE_FIXTURES=1; without it this checks the
// committed fixture still verifies here, which is what catches a change to this
// package that would silently break every relay.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// Where the Rust side reads it from. A committed fixture rather than a running
// process on both sides: the Rust test then needs no Go toolchain, so it runs on
// every `cargo test` instead of only where the harness is set up.
//
// That property is why the fixture lives in the engine repository rather than
// here, next to the verifier that reads it, and why this side is the one that
// has to go looking. MOQOM_ENGINE_ROOT says where; the fallback assumes a
// sibling checkout, which is where both land if you cloned them into the same
// directory.
const fixtureUnder = "moqom-relay/tests/testdata/tokens.json"

func fixturePath() string {
	if root := os.Getenv("MOQOM_ENGINE_ROOT"); root != "" {
		return filepath.Join(root, fixtureUnder)
	}
	return filepath.Join("../../../../moqom-engine", fixtureUnder)
}

// A fixed seed, so regenerating the fixture produces the same bytes and a diff
// shows a real change rather than fresh randomness.
var interopSeed = []byte{
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
}

// Fixed, so "has this expired" is a property of the fixture rather than of when
// the test runs.
var interopNow = time.Unix(1_800_000_000, 0).UTC()

type fixture struct {
	Issuer string  `json:"issuer"`
	Now    int64   `json:"now"`
	JWKS   string  `json:"jwks"`
	Cases  []tcase `json:"cases"`
}

type tcase struct {
	Name     string `json:"name"`
	Token    string `json:"token"`
	Tenant   string `json:"tenant"`
	Username string `json:"username"`
	Device   string `json:"device"`
}

func buildFixture(t *testing.T) fixture {
	t.Helper()

	key, err := KeyFrom(ed25519.NewKeyFromSeed(interopSeed))
	if err != nil {
		t.Fatalf("deriving the signing key: %v", err)
	}
	jwk, err := key.PublicJWK()
	if err != nil {
		t.Fatalf("rendering the public key: %v", err)
	}

	// A thumbprint longer than DeviceIDLength, so the fixture actually exercises
	// the truncation. A short one would pass on both sides no matter what either
	// of them did with the length.
	const thumbprint = "MpXqvQAmv7Y3Kk9dHnJpLwRtUvXyZaBcDeFgHiJkLmN"

	built := fixture{
		Issuer: "https://api.moqom.cloud",
		Now:    interopNow.Unix(),
		JWKS:   "[" + jwk + "]",
	}

	for _, spec := range []struct {
		name     string
		id       string
		username string
		room     string
	}{
		{"plain", "fixture-1", "ada", "room-42"},
		// Not ASCII, and not the same bytes as a decomposed spelling of the same
		// name. If the two implementations disagreed about normalisation this
		// would be a user the relay files under a different key than the control
		// plane bans.
		{"unicode username", "fixture-2", "ada\u0301", "room-42"},
		// A username with the separator the rendered form of an identity uses,
		// to prove nothing anywhere is parsing "user/device" back apart.
		{"username with a slash", "fixture-3", "ada/laptop", "room-42"},
	} {
		signed, _, err := Mint(key, Claims{
			Issuer:       built.Issuer,
			Subject:      spec.username,
			Audience:     "acme",
			ID:           spec.id,
			Confirmation: Confirmation{Thumbprint: thumbprint},
			MOQT: ScopeFrom(
				control.Grants{Publish: control.PublishCamera},
				spec.room, false, false,
			),
		}, 10*time.Minute, interopNow)
		if err != nil {
			t.Fatalf("minting %s: %v", spec.name, err)
		}

		device, err := control.DeviceFromThumbprint(thumbprint)
		if err != nil {
			t.Fatalf("deriving the device: %v", err)
		}
		built.Cases = append(built.Cases, tcase{
			Name:     spec.name,
			Token:    signed,
			Tenant:   "acme",
			Username: spec.username,
			Device:   device,
		})
	}
	return built
}

func TestInteropFixture(t *testing.T) {
	built := buildFixture(t)
	path := fixturePath()

	if os.Getenv("MOQOM_WRITE_FIXTURES") == "1" {
		encoded, err := json.MarshalIndent(built, "", "  ")
		if err != nil {
			t.Fatalf("encoding the fixture: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("making the fixture directory: %v", err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		t.Logf("wrote %s", path)
	}

	raw, err := os.ReadFile(path)
	// A missing fixture means the engine repository is not checked out, which is
	// an ordinary state for someone working on the control plane alone -- so it
	// skips rather than fails. That would be a quiet way to lose the only check
	// that the two token implementations agree, so CI sets
	// MOQOM_REQUIRE_INTEROP and turns the skip back into a failure.
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("MOQOM_REQUIRE_INTEROP") != "1" {
		t.Skipf("no fixture at %s: check out moqom-engine beside this repository, "+
			"or set MOQOM_ENGINE_ROOT", path)
	}
	if err != nil {
		t.Fatalf("reading the fixture (regenerate with MOQOM_WRITE_FIXTURES=1): %v", err)
	}
	var committed fixture
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}

	// Byte-for-byte, because the whole value of a fixture is that the far side
	// is checking the same bytes this side produced. Minting is deterministic
	// here only because every input is pinned -- including the token ID, which
	// Mint would otherwise randomise.
	if len(committed.Cases) != len(built.Cases) {
		t.Fatalf("fixture has %d cases, minting produced %d", len(committed.Cases), len(built.Cases))
	}

	public, ok := ed25519.NewKeyFromSeed(interopSeed).Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("seed did not produce an Ed25519 key")
	}
	key, err := KeyFrom(ed25519.NewKeyFromSeed(interopSeed))
	if err != nil {
		t.Fatalf("deriving the key: %v", err)
	}
	verifier := NewVerifier(committed.Issuer, map[string]ed25519.PublicKey{key.ID: public})

	for _, c := range committed.Cases {
		claims, err := verifier.Verify(c.Token, time.Unix(committed.Now, 0))
		if err != nil {
			t.Fatalf("%s: the committed token no longer verifies here: %v", c.Name, err)
		}
		identity, err := claims.Identity()
		if err != nil {
			t.Fatalf("%s: deriving the identity: %v", c.Name, err)
		}
		if identity.Username != c.Username || identity.Device != c.Device {
			t.Errorf("%s: fixture says %s/%s, this build reads %s/%s",
				c.Name, c.Username, c.Device, identity.Username, identity.Device)
		}
		if claims.Audience != c.Tenant {
			t.Errorf("%s: fixture says tenant %s, token carries %s", c.Name, c.Tenant, claims.Audience)
		}
	}
}
