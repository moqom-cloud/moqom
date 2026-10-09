package apple

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/apple/appletest"
)

const testAppID = "ABCDE12345.cloud.moqom.room"

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func TestAGenuineAttestationVerifiesAndItsAssertionsFollow(t *testing.T) {
	authority := appletest.NewAuthority(t, testNow)
	device := appletest.NewDevice(t)
	challenge := sha256.Sum256([]byte("an identity token"))

	object := authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{})
	attested, err := VerifyAttestation(object, device.KeyID, challenge[:], []string{"OTHER.app", testAppID},
		authority.Roots, testNow, false)
	if err != nil {
		t.Fatalf("a genuine attestation: %v", err)
	}
	if attested.AppID != testAppID || attested.Development {
		t.Errorf("attested %+v", attested)
	}

	body := sha256.Sum256([]byte(`{"room":"lobby"}`))
	counter, err := VerifyAssertion(device.Assert(t, body[:], testAppID, 1), body[:], attested.PublicKey, testAppID, 0)
	if err != nil || counter != 1 {
		t.Fatalf("the first assertion: %d, %v", counter, err)
	}
	if _, err := VerifyAssertion(device.Assert(t, body[:], testAppID, 1), body[:], attested.PublicKey, testAppID, 1); !errors.Is(err, ErrAttestation) {
		t.Errorf("a replayed counter gave %v", err)
	}
	other := sha256.Sum256([]byte(`{"room":"stage"}`))
	if _, err := VerifyAssertion(device.Assert(t, body[:], testAppID, 2), other[:], attested.PublicKey, testAppID, 1); !errors.Is(err, ErrAttestation) {
		t.Errorf("an assertion over another body gave %v", err)
	}
	if _, err := VerifyAssertion(appletest.NewDevice(t).Assert(t, body[:], testAppID, 2), body[:], attested.PublicKey, testAppID, 1); !errors.Is(err, ErrAttestation) {
		t.Errorf("an assertion by another key gave %v", err)
	}
}

func TestAnAttestationIsRefusedWhenAnyPartIsWrong(t *testing.T) {
	authority := appletest.NewAuthority(t, testNow)
	device := appletest.NewDevice(t)
	challenge := sha256.Sum256([]byte("an identity token"))
	elsewhere := sha256.Sum256([]byte("another identity token"))

	for name, test := range map[string]struct {
		object func() []byte
		keyID  string
		roots  *x509.CertPool
		dev    bool
	}{
		"for another app": {object: func() []byte {
			return authority.Attest(t, device, challenge[:], "OTHER.app", appletest.Spoil{})
		}},
		"over another challenge": {object: func() []byte {
			return authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{NonceFor: elsewhere[:]})
		}},
		"a used key": {object: func() []byte {
			return authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{Counter: 1})
		}},
		"from development": {object: func() []byte {
			return authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{AAGUID: aaguidDevelopment})
		}},
		"naming another key": {object: func() []byte {
			return authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{})
		}, keyID: appletest.NewDevice(t).KeyID},
		"not chaining to Apple": {object: func() []byte {
			return authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{})
		}, roots: AppAttestRoots()},
		"not CBOR": {object: func() []byte { return []byte{0xff, 0x00} }},
	} {
		t.Run(name, func(t *testing.T) {
			keyID := device.KeyID
			if test.keyID != "" {
				keyID = test.keyID
			}
			roots := authority.Roots
			if test.roots != nil {
				roots = test.roots
			}
			_, err := VerifyAttestation(test.object(), keyID, challenge[:], []string{testAppID}, roots, testNow, test.dev)
			if !errors.Is(err, ErrAttestation) {
				t.Errorf("got %v, want ErrAttestation", err)
			}
		})
	}

	development := authority.Attest(t, device, challenge[:], testAppID, appletest.Spoil{AAGUID: aaguidDevelopment})
	attested, err := VerifyAttestation(development, device.KeyID, challenge[:], []string{testAppID}, authority.Roots, testNow, true)
	if err != nil || !attested.Development {
		t.Errorf("a development attestation where allowed: %+v, %v", attested, err)
	}
}

func TestTheDecoderRefusesWhatAppAttestNeverSends(t *testing.T) {
	for name, data := range map[string][]byte{
		"trailing bytes":     {0x01, 0x02},
		"a negative number":  {0x20},
		"a tag":              {0xc0, 0x01},
		"indefinite length":  {0x5f, 0x41, 0x00, 0xff},
		"an integer map key": {0xa1, 0x01, 0x01},
		"a duplicate key":    {0xa2, 0x61, 'a', 0x01, 0x61, 'a', 0x02},
		"truncated bytes":    {0x45, 0x01},
		"a huge array":       {0x9a, 0xff, 0xff, 0xff, 0xff},
		"nested too deeply":  {0x81, 0x81, 0x81, 0x81, 0x81, 0x81, 0x01},
	} {
		if _, err := decodeCBOR(data); !errors.Is(err, errCBOR) {
			t.Errorf("%s: got %v, want errCBOR", name, err)
		}
	}
}

func TestTheEmbeddedRootIsApples(t *testing.T) {
	pool := AppAttestRoots()
	//nolint:staticcheck // Subjects is the only way to read a pool back.
	subjects := pool.Subjects()
	if len(subjects) != 1 || !bytes.Contains(subjects[0], []byte("Apple App Attestation Root CA")) {
		t.Errorf("the embedded root is not Apple's App Attestation Root CA")
	}
}
