package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func seedEntry(kind string, fill byte) string {
	return kind + ":" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

// sync-secrets.sh rotates by prepending an mldsa87 key and leaving what was
// there, so the entries from before the move have to be passed over rather
// than stop the control plane.
func TestSigningKeysSkipWhatWasThereBeforeMLDSA87(t *testing.T) {
	t.Setenv("MOQOM_CONTROL_SIGNING_KEYS", strings.Join([]string{
		seedEntry("mldsa87", 1), seedEntry("mldsa65", 2), seedEntry("ed25519", 3), seedEntry("mldsa87", 4),
	}, ","))
	keys, err := signingKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want the two mldsa87 entries", len(keys))
	}
	for _, key := range keys {
		if key.Algorithm != "ML-DSA-87" || !key.CanSign() {
			t.Errorf("key %s is %s, signs %v", key.ID, key.Algorithm, key.CanSign())
		}
	}
}

func TestSigningKeysRefuseASetWithNothingToSignWith(t *testing.T) {
	for name, value := range map[string]string{
		"only mldsa65": seedEntry("mldsa65", 2),
		"unknown kind": seedEntry("mldsa44", 2),
		"short seed":   "mldsa87:AAAA",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MOQOM_CONTROL_SIGNING_KEYS", value)
			if _, err := signingKeys(); err == nil {
				t.Error("accepted")
			}
		})
	}
}
