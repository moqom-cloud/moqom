package token

// Three implementations derive the same thumbprint, and this is the Go end of
// checking that they do.
//
// This side is the one that matters most and is easiest to get wrong quietly. It
// does not verify a proof -- the relay does that -- it decides what `cnf.jkt`
// says, from a JWK a device sent. So a disagreement here does not produce a
// refused proof with a clear reason: it produces tokens naming a device that
// does not exist, minted successfully, refused later by a relay that is not the
// component with the bug.
//
// The fixture is written by moqom-swift and read by moqom-relay; see
// `possession.rs`. This reads the `keys` half of it. The proofs are not checked
// here, because nothing in the control plane verifies one.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

const possessionFixtureUnder = "moqom-relay/tests/testdata/possession.json"

func possessionFixturePath() string {
	if root := os.Getenv("MOQOM_ENGINE_ROOT"); root != "" {
		return filepath.Join(root, possessionFixtureUnder)
	}
	return filepath.Join("../../../../moqom-engine", possessionFixtureUnder)
}

type possessionFixture struct {
	Keys []struct {
		Name       string    `json:"name"`
		JWK        PublicKey `json:"jwk"`
		Thumbprint string    `json:"thumbprint"`
	} `json:"keys"`
}

func TestThumbprintsAgreeWithTheOtherImplementations(t *testing.T) {
	path := possessionFixturePath()
	raw, err := os.ReadFile(path)

	// A missing fixture means the engine repository is not checked out, which is
	// ordinary for somebody working on the control plane alone. CI sets
	// MOQOM_REQUIRE_INTEROP and turns the skip back into a failure, so this
	// cannot become a check that quietly stopped running.
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("MOQOM_REQUIRE_INTEROP") != "1" {
		t.Skipf("no fixture at %s: check out moqom-engine beside this repository, "+
			"or set MOQOM_ENGINE_ROOT", path)
	}
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	var fixture possessionFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	if len(fixture.Keys) == 0 {
		t.Fatal("the fixture carries no thumbprint vectors")
	}

	for _, vector := range fixture.Keys {
		got, err := vector.JWK.Thumbprint()
		if err != nil {
			t.Errorf("%s: %v", vector.Name, err)
			continue
		}
		if got != vector.Thumbprint {
			t.Errorf("%s: the client and the relay say %s, this build says %s",
				vector.Name, vector.Thumbprint, got)
		}
	}
}
