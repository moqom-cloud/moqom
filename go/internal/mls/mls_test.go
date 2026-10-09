package mls_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/mls"
	"github.com/moqom-cloud/moqom/go/internal/mls/mlstest"
)

func TestMemoryStore(t *testing.T) {
	mlstest.Run(t, func(*testing.T) mls.Store { return mls.NewMemory() })
}

func vector(buffer *bytes.Buffer, data []byte) {
	switch n := len(data); {
	case n < 1<<6:
		buffer.WriteByte(byte(n))
	case n < 1<<14:
		buffer.Write([]byte{byte(n>>8) | 0x40, byte(n)})
	default:
		buffer.Write([]byte{byte(n>>24) | 0x80, byte(n >> 16), byte(n >> 8), byte(n)})
	}
	buffer.Write(data)
}

// keyPackage is a key package in the suite MoqomKit founds every group in.
func keyPackage(t *testing.T, identity string) []byte {
	return keyPackageIn(t, mls.FoundingSuite, identity)
}

// The suites MoqomKit no longer implements, for packages and groups that must
// be refused or replaced: the AES-128 suites, and MOQOM's earlier ML-KEM-768
// and ML-KEM-1024 with ML-DSA-65 suites that SuiteMLDSA (0xF0AC) replaced.
const (
	suiteX25519   = mls.CipherSuite(0x0001)
	suiteXWing    = mls.CipherSuite(0xF0A1)
	suiteMLKEM768 = mls.CipherSuite(0xF0AA)
	suiteMLDSA65  = mls.CipherSuite(0xF0AB)
)

// kemKey is a public key the size and shape of suite's: X25519's 32 bytes,
// X-Wing's ML-KEM-768 encapsulation key followed by 32, ML-KEM-768's alone, or
// ML-KEM-1024's alone for 0xF0AB and the suite there is.
func kemKey(t *testing.T, suite mls.CipherSuite) []byte {
	t.Helper()
	x25519 := make([]byte, 32)
	_, _ = rand.Read(x25519)
	switch suite {
	case suiteX25519:
		return x25519
	case mls.SuiteMLDSA, suiteMLDSA65:
		key, err := mlkem.GenerateKey1024()
		if err != nil {
			t.Fatal(err)
		}
		return key.EncapsulationKey().Bytes()
	}
	key, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	if suite == suiteMLKEM768 {
		return key.EncapsulationKey().Bytes()
	}
	return append(key.EncapsulationKey().Bytes(), x25519...)
}

// signer is a fresh signature key in suite's algorithm: its public key, and a
// function that signs with it.
func signer(t *testing.T, suite mls.CipherSuite) ([]byte, func([]byte) []byte) {
	t.Helper()
	if suite == mls.SuiteMLDSA {
		key, err := mldsa.GenerateKey(mldsa.MLDSA87())
		if err != nil {
			t.Fatal(err)
		}
		return key.PublicKey().Bytes(), func(message []byte) []byte {
			signature, err := key.Sign(rand.Reader, message, &mldsa.Options{})
			if err != nil {
				t.Fatal(err)
			}
			return signature
		}
	}
	if suite == suiteMLKEM768 || suite == suiteMLDSA65 {
		key, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		return key.PublicKey().Bytes(), func(message []byte) []byte {
			signature, err := key.Sign(rand.Reader, message, &mldsa.Options{})
			if err != nil {
				t.Fatal(err)
			}
			return signature
		}
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, func(message []byte) []byte { return ed25519.Sign(private, message) }
}

// keyPackageIn encodes a key package the way MLSGroup.swift does, signed.
func keyPackageIn(t *testing.T, suite mls.CipherSuite, identity string) []byte {
	t.Helper()
	return signedKeyPackage(t, suite, identity, kemKey(t, suite), kemKey(t, suite))
}

func signedKeyPackage(t *testing.T, suite mls.CipherSuite, identity string, initKey, encryptionKey []byte) []byte {
	t.Helper()
	public, sign := signer(t, suite)
	var tbs bytes.Buffer
	tbs.Write([]byte{0, 1})
	_ = binary.Write(&tbs, binary.BigEndian, uint16(suite))
	vector(&tbs, []byte(identity))
	vector(&tbs, public)
	vector(&tbs, initKey)
	vector(&tbs, encryptionKey)
	var content bytes.Buffer
	vector(&content, []byte("MLS 1.0 KeyPackageTBS"))
	vector(&content, tbs.Bytes())
	out := bytes.NewBuffer(tbs.Bytes())
	vector(out, sign(content.Bytes()))
	return out.Bytes()
}

// commit encodes a commit message's framing the way MLSGroup.swift does. The
// body, signature and tag are opaque to the service and filled with noise.
func commit(groupID []byte, epoch uint64) []byte {
	var out bytes.Buffer
	vector(&out, groupID)
	_ = binary.Write(&out, binary.BigEndian, epoch)
	_ = binary.Write(&out, binary.BigEndian, uint32(0))
	vector(&out, []byte("body"))
	vector(&out, make([]byte, 64))
	vector(&out, make([]byte, 32))
	return out.Bytes()
}

func caller(device string) mls.Caller {
	return mls.Caller{Room: mls.Room{Tenant: "acme", Room: "room-42"}, Device: device}
}

func TestKeyPackages(t *testing.T) {
	data := keyPackage(t, "bob/b")
	parsed, err := mls.ParseKeyPackage(data)
	if err != nil || parsed.Identity != "bob/b" || parsed.CipherSuite != mls.SuiteMLDSA {
		t.Fatalf("parsed %+v, %v", parsed, err)
	}
	for _, suite := range []mls.CipherSuite{suiteX25519, suiteXWing, suiteMLKEM768, suiteMLDSA65} {
		if _, err := mls.ParseKeyPackage(keyPackageIn(t, suite, "bob/b")); !errors.Is(err, mls.ErrInvalid) {
			t.Fatalf("retired suite %#04x accepted: %v", uint16(suite), err)
		}
	}
	// The package relabelled to an earlier ML-KEM suite, with its keys and a
	// signature that no longer covers it.
	for _, old := range []byte{0xAA, 0xAB} {
		relabelled := bytes.Clone(data)
		relabelled[2], relabelled[3] = 0xF0, old
		if _, err := mls.ParseKeyPackage(relabelled); !errors.Is(err, mls.ErrInvalid) {
			t.Fatalf("relabelled to 0xF0%X accepted: %v", old, err)
		}
	}
	if len(data) < 10350 || len(data) > 10450 {
		t.Fatalf("an ML-DSA key package is %d bytes", len(data))
	}

	tampered := bytes.Clone(data)
	tampered[10] ^= 1
	if _, err := mls.ParseKeyPackage(tampered); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("tampered package accepted: %v", err)
	}
	if _, err := mls.ParseKeyPackage(append(bytes.Clone(data), 0)); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("trailing byte accepted: %v", err)
	}

	// A suite MoqomKit does not implement, keys the wrong size, and an ML-KEM
	// key that fails FIPS 203's check.
	unknown := bytes.Clone(data)
	unknown[2], unknown[3] = 0x00, 0x02
	if _, err := mls.ParseKeyPackage(unknown); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("unknown suite accepted: %v", err)
	}
	short := signedKeyPackage(t, mls.SuiteMLDSA, "bob/b", kemKey(t, suiteX25519), kemKey(t, suiteX25519))
	if _, err := mls.ParseKeyPackage(short); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("X25519 keys under the ML-DSA suite accepted: %v", err)
	}
	notMLKEM := bytes.Repeat([]byte{0xff}, 1568)
	if _, err := mls.ParseKeyPackage(signedKeyPackage(t, mls.SuiteMLDSA, "bob/b", notMLKEM, kemKey(t, mls.SuiteMLDSA))); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("invalid ML-KEM-1024 key accepted: %v", err)
	}
	// Ed25519-signed, but naming the ML-DSA suite: the key is the wrong size.
	edSigned := signedKeyPackage(t, suiteXWing, "bob/b", kemKey(t, mls.SuiteMLDSA), kemKey(t, mls.SuiteMLDSA))
	edSigned[2], edSigned[3] = 0xF0, 0xAB
	if _, err := mls.ParseKeyPackage(edSigned); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("Ed25519 key under the ML-DSA suite accepted: %v", err)
	}

	service := mls.NewService(mls.NewMemory(), nil)
	ctx := context.Background()
	// A device may publish only packages naming itself, which is what binds a
	// leaf in the group to the token the control plane minted.
	if err := service.PublishKeyPackages(ctx, caller("mallory/m"), nil, data); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("published under another's identity: %v", err)
	}
	if err := service.PublishKeyPackages(ctx, caller("bob/b"), [][]byte{data}, data); err != nil {
		t.Fatal(err)
	}
	for _, suite := range []mls.CipherSuite{suiteX25519, suiteMLKEM768, suiteMLDSA65} {
		retired := keyPackageIn(t, suite, "bob/b")
		if err := service.PublishKeyPackages(ctx, caller("bob/b"), [][]byte{retired}, retired); !errors.Is(err, mls.ErrInvalid) {
			t.Fatalf("a package in suite %#04x published: %v", uint16(suite), err)
		}
	}
}

// Swift-produced bytes, so the two encoders cannot drift apart unnoticed. From
// MLSKeyPackageBundle.generate(identity: "bob/iphone-b") in MoqomKit.
func TestSwiftKeyPackageFixture(t *testing.T) {
	if swiftKeyPackage == "" {
		t.Skip("no fixture")
	}
	// MoqomKit's packages in the AES-128 suites it no longer implements.
	for _, old := range []string{swiftKeyPackage, swiftHybridKeyPackage} {
		if _, err := mls.ParseKeyPackage(decodeHex(t, old)); !errors.Is(err, mls.ErrInvalid) {
			t.Fatalf("Swift AES-128 key package accepted: %v", err)
		}
	}
	header, err := mls.ParseCommitHeader(decodeHex(t, swiftCommit))
	if err != nil || header.Epoch != 0 || string(header.GroupID) != "acme/room-42" {
		t.Fatalf("Swift commit header: %+v, %v", header, err)
	}

	fixture, err := os.ReadFile("testdata/moqomkit-keypackage-f0ac.hex")
	if err != nil {
		t.Fatal(err)
	}
	signed, err := mls.ParseKeyPackage(decodeHex(t, strings.TrimSpace(string(fixture))))
	if err != nil || signed.Identity != "bob/b" || signed.CipherSuite != mls.SuiteMLDSA || len(signed.SignatureKey) != 2592 {
		t.Fatalf("Swift ML-DSA key package: %+v, %v", signed.Identity, err)
	}

	header, err = mls.ParseCommitHeader(decodeHex(t, swiftHybridCommit))
	if err != nil || header.Epoch != 0 || string(header.GroupID) != "acme/room-42" {
		t.Fatalf("Swift hybrid commit header: %+v, %v", header, err)
	}
}

func decodeHex(t *testing.T, text string) []byte {
	t.Helper()
	out := make([]byte, len(text)/2)
	for i := range out {
		var b byte
		for _, c := range text[2*i : 2*i+2] {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				b |= byte(c - 'a' + 10)
			default:
				t.Fatalf("not hex: %q", c)
			}
		}
		out[i] = b
	}
	return out
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	service := mls.NewService(mls.NewMemory(), now)
	alice, bob, carol := caller("alice/a"), caller("bob/b"), caller("carol/c")

	// Nobody has a group: the first device is told so at once, and founds one.
	snapshot, err := service.Sync(ctx, alice, 0, 10*time.Second)
	if err != nil || snapshot.Group != nil {
		t.Fatalf("empty room: %+v, %v", snapshot, err)
	}
	groupID := []byte("group-1")
	// Only the ML-DSA suite founds a group: not one MoqomKit does not
	// implement, not the AES-128 suites or MOQOM's earlier ones, and not a
	// client that names none.
	for _, suite := range []mls.CipherSuite{0x0002, 0, suiteX25519, suiteXWing, suiteMLKEM768, suiteMLDSA65} {
		if err := service.CreateGroup(ctx, alice, groupID, suite); !errors.Is(err, mls.ErrInvalid) {
			t.Fatalf("founded in suite %#04x: %v", uint16(suite), err)
		}
	}
	if err := service.CreateGroup(ctx, alice, groupID, mls.SuiteMLDSA); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateGroup(ctx, bob, []byte("group-2"), mls.SuiteMLDSA); !errors.Is(err, mls.ErrConflict) {
		t.Fatalf("second founder: %v", err)
	}
	if snapshot, _ := service.Sync(ctx, alice, 0, 0); snapshot.Group == nil || snapshot.Group.CipherSuite != mls.SuiteMLDSA {
		t.Fatalf("the group's suite: %+v", snapshot.Group)
	}

	// Bob arrives with key packages and is pending; only a member may claim his.
	if err := service.PublishKeyPackages(ctx, bob, [][]byte{keyPackage(t, "bob/b")}, keyPackage(t, "bob/b")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClaimKeyPackage(ctx, carol, "bob/b"); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("a non-member claimed: %v", err)
	}
	snapshot, _ = service.Sync(ctx, alice, 0, 10*time.Second)
	if len(snapshot.Pending) != 1 || snapshot.Pending[0] != "bob/b" {
		t.Fatalf("alice should see bob pending: %+v", snapshot)
	}
	if _, err := service.ClaimKeyPackage(ctx, alice, "bob/b"); err != nil {
		t.Fatal(err)
	}

	// A commit that does not match the group or the epoch is refused before
	// anything is stored.
	if _, err := service.SubmitCommit(ctx, alice, mls.CommitRequest{Epoch: 0, Commit: commit([]byte("other"), 0)}); !errors.Is(err, mls.ErrStale) {
		t.Fatalf("another group's commit: %v", err)
	}
	if _, err := service.SubmitCommit(ctx, alice, mls.CommitRequest{Epoch: 1, Commit: commit(groupID, 0)}); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("mismatched epoch: %v", err)
	}
	if _, err := service.SubmitCommit(ctx, bob, mls.CommitRequest{Epoch: 0, Commit: commit(groupID, 0)}); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("a non-member committed: %v", err)
	}

	seq, err := service.SubmitCommit(ctx, alice, mls.CommitRequest{
		Epoch: 0, Commit: commit(groupID, 0), Welcome: []byte("welcome"), Joiners: []string{"bob/b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stale: the epoch has moved on.
	if _, err := service.SubmitCommit(ctx, alice, mls.CommitRequest{Epoch: 0, Commit: commit(groupID, 0)}); !errors.Is(err, mls.ErrStale) {
		t.Fatalf("stale commit: %v", err)
	}

	snapshot, _ = service.Sync(ctx, bob, 0, 0)
	if snapshot.Group == nil || snapshot.Group.Epoch != 1 || len(snapshot.Messages) != 2 ||
		snapshot.Messages[1].Kind != mls.KindWelcome || snapshot.Cursor != seq+1 {
		t.Fatalf("bob after the add: %+v", snapshot)
	}
	if strings.Join(snapshot.Members, ",") != "alice/a,bob/b" {
		t.Fatalf("members %v", snapshot.Members)
	}

	// A held sync is woken by a commit rather than waiting out its time.
	done := make(chan mls.Snapshot)
	go func() {
		held, _ := service.Sync(ctx, bob, snapshot.Cursor, 20*time.Second)
		done <- held
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := service.SubmitCommit(ctx, alice, mls.CommitRequest{Epoch: 1, Commit: commit(groupID, 1)}); err != nil {
		t.Fatal(err)
	}
	select {
	case held := <-done:
		if len(held.Messages) != 1 || held.Messages[0].Kind != mls.KindCommit {
			t.Fatalf("woken with %+v", held)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held sync was not woken")
	}

	// Bob goes quiet. Alice is told there is somebody to remove.
	advance(mls.OnlineWindow + time.Second)
	snapshot, _ = service.Sync(ctx, alice, 0, 10*time.Second)
	if strings.Join(snapshot.Members, ",") != "alice/a" {
		t.Fatalf("members after bob went quiet: %v", snapshot.Members)
	}

	// Everybody goes quiet: the group is dropped for the next arrival.
	advance(mls.OnlineWindow + time.Second)
	snapshot, _ = service.Sync(ctx, carol, 0, 10*time.Second)
	if snapshot.Group != nil || len(snapshot.Messages) != 0 {
		t.Fatalf("abandoned group survived: %+v", snapshot)
	}
	// The next arrival founds afresh, in the same suite.
	if err := service.CreateGroup(ctx, carol, []byte("group-3"), mls.SuiteMLDSA); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := service.Sync(ctx, carol, 0, 0); snapshot.Group == nil || snapshot.Group.CipherSuite != mls.SuiteMLDSA {
		t.Fatalf("a refounded group: %+v", snapshot.Group)
	}
}

// The routes, end to end through JSON, with the shape MoqomKit decodes.
func TestHTTP(t *testing.T) {
	service := mls.NewService(mls.NewMemory(), nil)
	handler := mls.Handler(service, func(r *http.Request) (mls.Caller, error) {
		device := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if device == "" {
			return mls.Caller{}, errors.New("no token")
		}
		return caller(device), nil
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	call := func(method, path, device string, body any) (int, map[string]any) {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		} else {
			reader = bytes.NewReader(nil)
		}
		request, _ := http.NewRequest(method, server.URL+path, reader)
		if device != "" {
			request.Header.Set("Authorization", "Bearer "+device)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		return response.StatusCode, decoded
	}

	if code, _ := call("GET", "/mls/v1/sync?after=0&wait=0", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sync: %d", code)
	}
	code, body := call("GET", "/mls/v1/sync?after=0&wait=0", "alice/a", nil)
	if code != http.StatusOK || body["group"] != nil || body["cursor"] != float64(0) {
		t.Fatalf("first sync: %d %v", code, body)
	}
	groupID := []byte("group-1")
	if code, _ := call("POST", "/mls/v1/group", "alice/a", map[string]any{"groupId": groupID, "cipherSuite": 0x7a7a}); code != http.StatusBadRequest {
		t.Fatalf("found in an unknown suite: %d", code)
	}
	if code, _ := call("POST", "/mls/v1/group", "alice/a", map[string]any{"groupId": groupID, "cipherSuite": 0xf0aa}); code != http.StatusBadRequest {
		t.Fatalf("found in the ML-KEM-768 suite: %d", code)
	}
	if code, _ := call("POST", "/mls/v1/group", "alice/a", map[string]any{"groupId": groupID, "cipherSuite": 0xf0ab}); code != http.StatusBadRequest {
		t.Fatalf("found in the ML-DSA-65 suite: %d", code)
	}
	if code, _ := call("POST", "/mls/v1/group", "alice/a", map[string]any{"groupId": groupID, "cipherSuite": 0xf0ac}); code != http.StatusCreated {
		t.Fatalf("found: %d", code)
	}
	if code, _ := call("POST", "/mls/v1/group", "bob/b", map[string]any{"groupId": []byte("x"), "cipherSuite": 0xf0ac}); code != http.StatusConflict {
		t.Fatalf("second found: %d", code)
	}
	kp := keyPackage(t, "bob/b")
	if code, _ := call("PUT", "/mls/v1/keypackages", "bob/b", map[string]any{"keyPackages": [][]byte{kp}, "lastResort": kp}); code != http.StatusNoContent {
		t.Fatalf("publish: %d", code)
	}
	code, body = call("POST", "/mls/v1/keypackages/claim", "alice/a", map[string]any{"device": "bob/b"})
	if code != http.StatusOK || body["keyPackage"] == nil {
		t.Fatalf("claim: %d %v", code, body)
	}
	code, body = call("POST", "/mls/v1/commits", "alice/a", map[string]any{
		"epoch": 0, "commit": commit(groupID, 0), "welcome": []byte("w"), "joiners": []string{"bob/b"}, "removed": []string{},
	})
	if code != http.StatusOK || body["seq"] != float64(1) {
		t.Fatalf("commit: %d %v", code, body)
	}
	if code, _ := call("POST", "/mls/v1/commits", "alice/a", map[string]any{"epoch": 0, "commit": commit(groupID, 0)}); code != http.StatusConflict {
		t.Fatalf("stale commit: %d", code)
	}
	code, body = call("GET", "/mls/v1/sync?after=0&wait=0", "bob/b", nil)
	messages, _ := body["messages"].([]any)
	if code != http.StatusOK || len(messages) != 2 || body["cursor"] != float64(2) {
		t.Fatalf("bob's sync: %d %v", code, body)
	}
	if group, _ := body["group"].(map[string]any); group["epoch"] != float64(1) || group["cipherSuite"] != float64(0xf0ac) {
		t.Fatalf("group %v", body["group"])
	}
	if code, _ := call("POST", "/mls/v1/rejoin", "bob/b", nil); code != http.StatusNoContent {
		t.Fatalf("rejoin: %d", code)
	}
	if code, _ := call("DELETE", "/mls/v1/presence", "bob/b", nil); code != http.StatusNoContent {
		t.Fatalf("leave: %d", code)
	}
}

// A group stored in an AES-128 suite, from before they were removed, is
// dropped when the room is next used, and the room is founded again.
func TestAES128GroupIsReplaced(t *testing.T) {
	ctx := context.Background()
	store := mls.NewMemory()
	alice := caller("alice/a")
	if _, err := store.CreateGroup(ctx, alice.Room, []byte("old"), suiteX25519, alice.Device, time.Now()); err != nil {
		t.Fatal(err)
	}
	service := mls.NewService(store, nil)
	if snapshot, err := service.Sync(ctx, alice, 0, 0); err != nil || snapshot.Group != nil {
		t.Fatalf("an AES-128 group was served: %+v, %v", snapshot.Group, err)
	}
	if err := service.CreateGroup(ctx, alice, []byte("new"), mls.SuiteMLDSA); err != nil {
		t.Fatal(err)
	}
	if group, err := store.Group(ctx, alice.Room); err != nil || string(group.ID) != "new" || group.CipherSuite != mls.SuiteMLDSA {
		t.Fatalf("refounded: %+v, %v", group, err)
	}
}
