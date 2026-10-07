package mls_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// keyPackage is a key package in the hybrid suite, which MoqomKit founds in.
func keyPackage(t *testing.T, identity string) []byte {
	return keyPackageIn(t, mls.SuiteXWing, identity)
}

// kemKey is a public key the size and shape of suite's: X25519's 32 bytes, or
// X-Wing's ML-KEM-768 encapsulation key followed by 32.
func kemKey(t *testing.T, suite mls.CipherSuite) []byte {
	t.Helper()
	x25519 := make([]byte, 32)
	_, _ = rand.Read(x25519)
	if suite != mls.SuiteXWing {
		return x25519
	}
	key, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	return append(key.EncapsulationKey().Bytes(), x25519...)
}

// keyPackageIn encodes a key package the way MLSGroup.swift does, signed.
func keyPackageIn(t *testing.T, suite mls.CipherSuite, identity string) []byte {
	t.Helper()
	return signedKeyPackage(t, suite, identity, kemKey(t, suite), kemKey(t, suite))
}

func signedKeyPackage(t *testing.T, suite mls.CipherSuite, identity string, initKey, encryptionKey []byte) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
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
	vector(out, ed25519.Sign(private, content.Bytes()))
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
	for _, suite := range []mls.CipherSuite{mls.SuiteX25519, mls.SuiteXWing} {
		parsed, err := mls.ParseKeyPackage(keyPackageIn(t, suite, "bob/b"))
		if err != nil || parsed.Identity != "bob/b" || parsed.CipherSuite != suite {
			t.Fatalf("suite %#04x: parsed %+v, %v", uint16(suite), parsed, err)
		}
	}
	data := keyPackage(t, "bob/b")
	if len(data) < 2400 || len(data) > 2700 {
		t.Fatalf("a hybrid key package is %d bytes", len(data))
	}

	tampered := bytes.Clone(data)
	tampered[10] ^= 1
	if _, err := mls.ParseKeyPackage(tampered); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("tampered package accepted: %v", err)
	}
	if _, err := mls.ParseKeyPackage(append(bytes.Clone(data), 0)); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("trailing byte accepted: %v", err)
	}

	// A suite MoqomKit does not implement, keys the wrong size for the suite
	// named, and an X-Wing key whose ML-KEM half fails FIPS 203's check.
	unknown := bytes.Clone(data)
	unknown[2], unknown[3] = 0x00, 0x02
	if _, err := mls.ParseKeyPackage(unknown); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("unknown suite accepted: %v", err)
	}
	short := signedKeyPackage(t, mls.SuiteXWing, "bob/b", kemKey(t, mls.SuiteX25519), kemKey(t, mls.SuiteX25519))
	if _, err := mls.ParseKeyPackage(short); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("X25519 keys under the X-Wing suite accepted: %v", err)
	}
	notMLKEM := bytes.Repeat([]byte{0xff}, 1216)
	if _, err := mls.ParseKeyPackage(signedKeyPackage(t, mls.SuiteXWing, "bob/b", notMLKEM, kemKey(t, mls.SuiteXWing))); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("invalid ML-KEM key accepted: %v", err)
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
	// One suite per device, so whichever package is claimed is in it.
	classical := keyPackageIn(t, mls.SuiteX25519, "bob/b")
	if err := service.PublishKeyPackages(ctx, caller("bob/b"), [][]byte{data}, classical); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("packages in two suites accepted: %v", err)
	}
	if err := service.PublishKeyPackages(ctx, caller("bob/b"), [][]byte{classical}, classical); err != nil {
		t.Fatal(err)
	}
}

// Swift-produced bytes, so the two encoders cannot drift apart unnoticed. From
// MLSKeyPackageBundle.generate(identity: "bob/iphone-b") in MoqomKit.
func TestSwiftKeyPackageFixture(t *testing.T) {
	if swiftKeyPackage == "" {
		t.Skip("no fixture")
	}
	data := decodeHex(t, swiftKeyPackage)
	parsed, err := mls.ParseKeyPackage(data)
	if err != nil || parsed.Identity != "bob/iphone-b" {
		t.Fatalf("Swift key package: %+v, %v", parsed, err)
	}
	if parsed.CipherSuite != mls.SuiteX25519 {
		t.Fatalf("Swift key package suite %#04x", uint16(parsed.CipherSuite))
	}
	header, err := mls.ParseCommitHeader(decodeHex(t, swiftCommit))
	if err != nil || header.Epoch != 0 || string(header.GroupID) != "acme/room-42" {
		t.Fatalf("Swift commit header: %+v, %v", header, err)
	}

	hybrid, err := mls.ParseKeyPackage(decodeHex(t, swiftHybridKeyPackage))
	if err != nil || hybrid.Identity != "bob/iphone-b" || hybrid.CipherSuite != mls.SuiteXWing || len(hybrid.InitKey) != 1216 {
		t.Fatalf("Swift hybrid key package: %+v, %v", hybrid.Identity, err)
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
	if err := service.CreateGroup(ctx, alice, groupID, mls.CipherSuite(0x0002)); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("founded in a suite MoqomKit does not implement: %v", err)
	}
	if err := service.CreateGroup(ctx, alice, groupID, mls.SuiteXWing); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateGroup(ctx, bob, []byte("group-2"), mls.SuiteXWing); !errors.Is(err, mls.ErrConflict) {
		t.Fatalf("second founder: %v", err)
	}
	if snapshot, _ := service.Sync(ctx, alice, 0, 0); snapshot.Group == nil || snapshot.Group.CipherSuite != mls.SuiteXWing {
		t.Fatalf("the group's suite: %+v", snapshot.Group)
	}

	// Dave is waiting with packages in the other suite. Claiming one is
	// refused, so a committer never adds him to a group he cannot be in.
	dave := caller("dave/d")
	if err := service.PublishKeyPackages(ctx, dave, nil, keyPackageIn(t, mls.SuiteX25519, "dave/d")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClaimKeyPackage(ctx, alice, "dave/d"); !errors.Is(err, mls.ErrConflict) {
		t.Fatalf("a package in another suite was handed out: %v", err)
	}
	if err := service.Leave(ctx, dave); err != nil {
		t.Fatal(err)
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
	// A client that names no suite founds in 0x0001, as every client did
	// before suites were declared.
	if err := service.CreateGroup(ctx, carol, []byte("group-3"), 0); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := service.Sync(ctx, carol, 0, 0); snapshot.Group == nil || snapshot.Group.CipherSuite != mls.SuiteX25519 {
		t.Fatalf("a group founded with no suite: %+v", snapshot.Group)
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
	if code, _ := call("POST", "/mls/v1/group", "alice/a", map[string]any{"groupId": groupID, "cipherSuite": 0xf0a1}); code != http.StatusCreated {
		t.Fatalf("found: %d", code)
	}
	if code, _ := call("POST", "/mls/v1/group", "bob/b", map[string]any{"groupId": []byte("x")}); code != http.StatusConflict {
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
	if group, _ := body["group"].(map[string]any); group["epoch"] != float64(1) || group["cipherSuite"] != float64(0xf0a1) {
		t.Fatalf("group %v", body["group"])
	}
	if code, _ := call("POST", "/mls/v1/rejoin", "bob/b", nil); code != http.StatusNoContent {
		t.Fatalf("rejoin: %d", code)
	}
	if code, _ := call("DELETE", "/mls/v1/presence", "bob/b", nil); code != http.StatusNoContent {
		t.Fatalf("leave: %d", code)
	}
}
