package token

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The seed MoqomKit's tests use for their room authority, so the fixture below
// verifies there against a key the Swift side derives on its own.
var roomLogSeed = bytes.Repeat([]byte{0xA7}, 32)

const roomLogEntry = `{"sequence":1,"snapshot":{"participants":[]},"version":1}`

func TestARoomLogEntryVerifiesOnlyWhereItWasSigned(t *testing.T) {
	key, err := KeyFromSeed(roomLogSeed)
	if err != nil {
		t.Fatal(err)
	}
	entry := []byte(roomLogEntry)
	property, err := key.SignRoomLog("acme", "room-42", entry)
	if err != nil {
		t.Fatal(err)
	}
	keys := []Key{key}
	if !VerifyRoomLog(keys, "acme", "room-42", entry, property) {
		t.Fatal("an entry did not verify in the room it was signed for")
	}
	if VerifyRoomLog(keys, "acme", "room-7", entry, property) {
		t.Error("an entry verified in another room")
	}
	if VerifyRoomLog(keys, "globex", "room-42", entry, property) {
		t.Error("an entry verified for another tenant")
	}
	altered := bytes.Replace(entry, []byte(`"sequence":1`), []byte(`"sequence":2`), 1)
	if VerifyRoomLog(keys, "acme", "room-42", altered, property) {
		t.Error("an altered entry verified")
	}
	other, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if VerifyRoomLog([]Key{other}, "acme", "room-42", entry, property) {
		t.Error("an entry verified against a key that did not sign it")
	}
}

func TestALegacyKeyNeverSignsOrVerifiesTheLog(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := LegacyEd25519(public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.SignRoomLog("acme", "room-42", []byte(roomLogEntry)); err != ErrRoomLogUnsignable {
		t.Fatalf("a verify-only key signed: %v", err)
	}
	forged := append([]byte{byte(len(legacy.ID))}, legacy.ID...)
	forged = append(forged, make([]byte, ed25519.SignatureSize)...)
	if VerifyRoomLog([]Key{legacy}, "acme", "room-42", []byte(roomLogEntry), forged) {
		t.Error("an Ed25519 key verified a log entry")
	}
}

// The cross-language half: an entry signed here, which MoqomKit's
// RoomLogSignatureInteropTests verifies. Regenerate with MOQOM_WRITE_FIXTURES=1.
func TestRoomLogFixture(t *testing.T) {
	type fixture struct {
		Tenant   string `json:"tenant"`
		Room     string `json:"room"`
		Entry    string `json:"entry"`
		KeyID    string `json:"kid"`
		Property string `json:"property"`
	}
	path := filepath.Join("testdata", "roomlog.json")
	key, err := KeyFromSeed(roomLogSeed)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MOQOM_WRITE_FIXTURES") == "1" {
		property, err := key.SignRoomLog("acme", "room-42", []byte(roomLogEntry))
		if err != nil {
			t.Fatal(err)
		}
		written, err := json.MarshalIndent(fixture{
			Tenant:   "acme",
			Room:     "room-42",
			Entry:    roomLogEntry,
			KeyID:    key.ID,
			Property: base64.StdEncoding.EncodeToString(property),
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(written, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture (regenerate with MOQOM_WRITE_FIXTURES=1): %v", err)
	}
	var read fixture
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	property, err := base64.StdEncoding.DecodeString(read.Property)
	if err != nil {
		t.Fatal(err)
	}
	if read.KeyID != key.ID {
		t.Fatalf("fixture names key %q, the seed derives %q", read.KeyID, key.ID)
	}
	if !VerifyRoomLog([]Key{key}, read.Tenant, read.Room, []byte(read.Entry), property) {
		t.Fatal("the fixture does not verify")
	}
}
