package mls_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/mls"
)

func TestAudience(t *testing.T) {
	ctx := context.Background()
	service := mls.NewService(mls.NewMemory(), nil)
	alice, viewer := caller("alice/a"), caller("viewer/v")
	groupID := []byte("group-1")
	if err := service.CreateGroup(ctx, alice, groupID, mls.SuiteMLDSA); err != nil {
		t.Fatal(err)
	}
	key := kemKey(t, mls.SuiteMLDSA)

	// A viewer is never a member: it may not found the group or be added.
	asViewer := viewer
	asViewer.Viewer = true
	if err := service.PublishKeyPackages(ctx, asViewer, nil, keyPackage(t, "viewer/v")); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("a viewer published key packages: %v", err)
	}
	if err := service.CreateGroup(ctx, asViewer, []byte("group-2"), mls.SuiteMLDSA); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("a viewer founded a group: %v", err)
	}

	if _, err := service.JoinAudience(ctx, viewer, key[:32], nil, 0); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("a key that is not ML-KEM-1024: %v", err)
	}
	envelope, err := service.JoinAudience(ctx, viewer, key, nil, 0)
	if err != nil || envelope != nil {
		t.Fatalf("nothing sealed yet: %+v, %v", envelope, err)
	}

	// A member's sync counts the viewer as waiting, which is work.
	snapshot, err := service.Sync(ctx, alice, 0, 0)
	if err != nil || snapshot.Audience != 1 || snapshot.AudienceWaiting != 1 {
		t.Fatalf("snapshot %+v, %v", snapshot, err)
	}
	if snapshot, _ := service.Sync(ctx, viewer, 0, 0); snapshot.Audience != 0 {
		t.Fatalf("a viewer is told about the audience: %+v", snapshot)
	}

	// Only a member is given the work or may seal.
	if _, err := service.AudienceWork(ctx, viewer); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("a viewer asked for the work: %v", err)
	}
	work, err := service.AudienceWork(ctx, alice)
	if err != nil || !bytes.Equal(work.GroupID, groupID) || work.Epoch != 0 || len(work.Seats) != 1 ||
		work.Seats[0].Device != "viewer/v" || !bytes.Equal(work.Seats[0].Key, key) {
		t.Fatalf("work %+v, %v", work, err)
	}
	if err := service.SealAudience(ctx, viewer, work.EpochRef, map[string][]byte{"viewer/v": {1}}); !errors.Is(err, mls.ErrForbidden) {
		t.Fatalf("a viewer sealed: %v", err)
	}
	stale := mls.EpochRef{GroupID: groupID, Epoch: 7}
	if err := service.SealAudience(ctx, alice, stale, map[string][]byte{"viewer/v": {1}}); !errors.Is(err, mls.ErrStale) {
		t.Fatalf("sealed for an epoch the group is not at: %v", err)
	}
	if err := service.SealAudience(ctx, alice, work.EpochRef, map[string][]byte{"viewer/v": make([]byte, 4096)}); !errors.Is(err, mls.ErrInvalid) {
		t.Fatalf("an oversized envelope: %v", err)
	}

	// A held viewer is woken by the seal.
	got := make(chan *mls.Envelope, 1)
	go func() {
		envelope, _ := service.JoinAudience(ctx, viewer, key, nil, 5*time.Second)
		got <- envelope
	}()
	time.Sleep(50 * time.Millisecond)
	if err := service.SealAudience(ctx, alice, work.EpochRef, map[string][]byte{"viewer/v": []byte("sealed")}); err != nil {
		t.Fatal(err)
	}
	select {
	case envelope := <-got:
		if envelope == nil || string(envelope.Data) != "sealed" || !envelope.EpochRef.Equal(work.EpochRef) {
			t.Fatalf("envelope %+v", envelope)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held viewer was not woken by the seal")
	}

	// Holding the current epoch, it is not handed the same envelope again,
	// and nobody is waiting.
	if envelope, _ := service.JoinAudience(ctx, viewer, key, &work.EpochRef, 0); envelope != nil {
		t.Fatalf("handed the epoch it has: %+v", envelope)
	}
	if snapshot, _ := service.Sync(ctx, alice, 0, 0); snapshot.AudienceWaiting != 0 {
		t.Fatalf("still waiting once sealed: %+v", snapshot)
	}
	if work, _ := service.AudienceWork(ctx, alice); len(work.Seats) != 0 {
		t.Fatalf("work once sealed: %+v", work)
	}

	if err := service.LeaveAudience(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := service.Sync(ctx, alice, 0, 0); snapshot.Audience != 0 {
		t.Fatalf("a viewer that left is counted: %+v", snapshot)
	}
}

func TestAudienceHTTP(t *testing.T) {
	service := mls.NewService(mls.NewMemory(), nil)
	server := httptest.NewServer(mls.Handler(service, func(r *http.Request) (mls.Caller, error) {
		return caller(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), nil
	}))
	defer server.Close()
	call := func(method, path, device string, body any) (int, map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		request, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(encoded))
		request.Header.Set("Authorization", "Bearer "+device)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		return response.StatusCode, decoded
	}

	groupID := []byte("group-1")
	if code, _ := call("POST", "/mls/v1/group", "alice/a", map[string]any{"groupId": groupID, "cipherSuite": 0xf0ac}); code != http.StatusCreated {
		t.Fatalf("found: %d", code)
	}
	key := kemKey(t, mls.SuiteMLDSA)
	if code, _ := call("POST", "/mls/v1/audience", "viewer/v", map[string]any{"publicKey": key, "wait": 0}); code != http.StatusNoContent {
		t.Fatalf("join: %d", code)
	}
	code, body := call("GET", "/mls/v1/sync?after=0&wait=0", "alice/a", nil)
	if code != http.StatusOK || body["audience"] != float64(1) || body["audienceWaiting"] != float64(1) {
		t.Fatalf("sync: %d %v", code, body)
	}
	code, body = call("GET", "/mls/v1/audience/work", "alice/a", nil)
	seats, _ := body["seats"].([]any)
	if code != http.StatusOK || len(seats) != 1 || body["epoch"] != float64(0) {
		t.Fatalf("work: %d %v", code, body)
	}
	if code, _ := call("GET", "/mls/v1/audience/work", "viewer/v", nil); code != http.StatusForbidden {
		t.Fatalf("a viewer's work: %d", code)
	}
	seal := map[string]any{"groupId": groupID, "epoch": 0, "envelopes": map[string][]byte{"viewer/v": []byte("sealed")}}
	if code, _ := call("POST", "/mls/v1/audience/envelopes", "alice/a", seal); code != http.StatusNoContent {
		t.Fatalf("seal: %d", code)
	}
	code, body = call("POST", "/mls/v1/audience", "viewer/v", map[string]any{"publicKey": key, "wait": 0})
	if code != http.StatusOK || body["epoch"] != float64(0) || body["data"] != "c2VhbGVk" {
		t.Fatalf("envelope: %d %v", code, body)
	}
	have := map[string]any{"publicKey": key, "haveGroupId": groupID, "haveEpoch": 0, "wait": 0}
	if code, _ := call("POST", "/mls/v1/audience", "viewer/v", have); code != http.StatusNoContent {
		t.Fatalf("handed the epoch it has: %d", code)
	}
	if code, _ := call("DELETE", "/mls/v1/audience", "viewer/v", nil); code != http.StatusNoContent {
		t.Fatalf("leave: %d", code)
	}
}
