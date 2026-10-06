package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

type roomsHarness struct {
	server   *server.Server
	rooms    adminpb.RoomsClient
	accounts *revocable
}

func newRoomsHarness(t *testing.T) *roomsHarness {
	t.Helper()
	signing, err := token.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	accounts := &revocable{accounts: map[string]server.ServiceAccount{
		acmeKey:     {Name: "acme", Tenant: "acme", Grants: control.Grants{IsOwner: true}},
		globexKey:   {Name: "globex", Tenant: "globex", Grants: control.Grants{IsOwner: true}},
		acmeJoinKey: {Name: "acme-join", Tenant: "acme", Grants: control.Grants{Publish: control.PublishCamera}},
		operatorKey: {Name: "operator", Grants: control.Grants{IsOwner: true}},
		liveRelay:   {Name: "relay", IsRelay: true},
	}}
	instance, err := server.New(server.Config{
		Store: memory.New(time.Now, nil), Accounts: accounts, Issuer: "moqom-test", Keys: []token.Key{signing},
	})
	if err != nil {
		t.Fatal(err)
	}
	unary, streaming := instance.Interceptors()
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(streaming))
	instance.Register(grpcServer)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = grpcServer.Serve(listener) }()
	connection, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close()
		grpcServer.Stop()
		_ = listener.Close()
	})
	return &roomsHarness{server: instance, rooms: adminpb.NewRoomsClient(connection), accounts: accounts}
}

func (h *roomsHarness) create(t *testing.T, credential, tenant, room string) {
	t.Helper()
	if _, err := h.rooms.CreateRoom(as(credential), &adminpb.CreateRoomRequest{Tenant: tenant, Room: room, OwnerUsername: "host"}); err != nil {
		t.Fatalf("creating %s/%s: %v", tenant, room, err)
	}
}

func (h *roomsHarness) close(t *testing.T, credential, tenant, room string) {
	t.Helper()
	if _, err := h.rooms.CloseRoom(as(credential), &adminpb.CloseRoomRequest{Tenant: tenant, Room: room}); err != nil {
		t.Fatalf("closing %s/%s: %v", tenant, room, err)
	}
}

func getRooms(t *testing.T, handler http.Handler, credential, query string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, server.RoomsPath+query, nil)
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestRoomsBridgeRefusesAnyoneButATenantsOwnerKey(t *testing.T) {
	h := newRoomsHarness(t)
	handler := h.server.RoomsHandler(server.RoomsOptions{})
	for _, path := range []string{server.RoomsPath, server.RoomEventsStreamPath} {
		for _, tc := range []struct {
			name, credential, query string
			want                    int
		}{
			{"no credential", "", "", http.StatusUnauthorized},
			{"an unknown credential", "guess", "", http.StatusUnauthorized},
			{"a key in the query string", "", "?access_token=" + acmeKey, http.StatusUnauthorized},
			{"a relay credential", liveRelay, "", http.StatusForbidden},
			{"a key without owner standing", acmeJoinKey, "", http.StatusForbidden},
			{"a tenant key naming another tenant", acmeKey, "?tenant=globex", http.StatusForbidden},
			{"a platform operator account", operatorKey, "?tenant=acme", http.StatusForbidden},
		} {
			request := httptest.NewRequest(http.MethodGet, path+tc.query, nil)
			if tc.credential != "" {
				request.Header.Set("Authorization", "Bearer "+tc.credential)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Errorf("%s %s: got %d (%s), want %d", path, tc.name, recorder.Code, recorder.Body.String(), tc.want)
			}
		}
	}
	// Read-only: nothing but GET is routed.
	request := httptest.NewRequest(http.MethodPost, server.RoomsPath, strings.NewReader(`{"room":"x"}`))
	request.Header.Set("Authorization", "Bearer "+acmeKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST %s: %d", server.RoomsPath, recorder.Code)
	}
}

func TestRoomsBridgeListsOnlyTheCallersRooms(t *testing.T) {
	h := newRoomsHarness(t)
	handler := h.server.RoomsHandler(server.RoomsOptions{})
	h.create(t, acmeKey, "acme", "lobby")
	h.create(t, acmeKey, "acme", "stage")
	h.create(t, globexKey, "globex", "secret-room")
	h.close(t, acmeKey, "acme", "stage")

	code, body := getRooms(t, handler, acmeKey, "")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var page struct {
		Rooms []struct {
			Room      string `json:"room"`
			Lifecycle string `json:"lifecycle"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Rooms) != 2 || strings.Contains(body, "globex") || strings.Contains(body, "secret-room") {
		t.Fatalf("acme's rooms: %s", body)
	}
	if code, body := getRooms(t, handler, acmeKey, "?lifecycle=ended"); code != http.StatusOK || !strings.Contains(body, `"stage"`) || strings.Contains(body, `"lobby"`) {
		t.Errorf("ended rooms: %d %s", code, body)
	}
	if code, _ := getRooms(t, handler, acmeKey, "?lifecycle=bogus"); code != http.StatusBadRequest {
		t.Errorf("unknown lifecycle: %d", code)
	}
	if code, body := getRooms(t, handler, globexKey, ""); code != http.StatusOK || strings.Contains(body, "lobby") || !strings.Contains(body, "secret-room") {
		t.Errorf("globex's rooms: %d %s", code, body)
	}
}

func openRoomStream(t *testing.T, url, credential string) (*http.Response, *bufio.Reader) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if event, _ := readEvent(t, reader); event != "ready" {
		t.Fatalf("first event %q", event)
	}
	return response, reader
}

func TestRoomEventStreamCarriesOnlyTheCallersEventsUntilRevoked(t *testing.T) {
	h := newRoomsHarness(t)
	handler := h.server.RoomsHandler(server.RoomsOptions{Recheck: 50 * time.Millisecond, Heartbeat: time.Hour})
	web := httptest.NewServer(handler)
	defer web.Close()
	h.create(t, acmeKey, "acme", "lobby")
	h.create(t, globexKey, "globex", "secret-room")

	response, reader := openRoomStream(t, web.URL+server.RoomEventsStreamPath, acmeKey)
	defer response.Body.Close()

	// Globex's event is published first; acme's stream must skip it.
	h.close(t, globexKey, "globex", "secret-room")
	h.close(t, acmeKey, "acme", "lobby")
	event, data := readEvent(t, reader)
	if event != "room_event" || !strings.Contains(data, `"room":"lobby"`) || !strings.Contains(data, "lifecycle_changed") {
		t.Fatalf("event %q %s", event, data)
	}
	if strings.Contains(data, "globex") || strings.Contains(data, "secret-room") {
		t.Fatalf("acme saw globex: %s", data)
	}

	h.accounts.revoke(acmeKey)
	for i := 0; i < 20; i++ {
		if event, _ := readEvent(t, reader); event == "revoked" {
			return
		}
	}
	t.Fatal("a revoked key kept its stream")
}

func TestRoomEventStreamResumesAfterASequence(t *testing.T) {
	h := newRoomsHarness(t)
	handler := h.server.RoomsHandler(server.RoomsOptions{Heartbeat: time.Hour})
	web := httptest.NewServer(handler)
	defer web.Close()
	h.create(t, acmeKey, "acme", "lobby")
	h.close(t, acmeKey, "acme", "lobby")

	// after=0 replays the room from its first event.
	response, reader := openRoomStream(t, web.URL+server.RoomEventsStreamPath+"?room=lobby&after=0", acmeKey)
	event, data := readEvent(t, reader)
	response.Body.Close()
	if event != "room_event" || !strings.Contains(data, `"sequence":"1"`) || !strings.Contains(data, "lifecycle_changed") {
		t.Fatalf("history: %q %s", event, data)
	}

	// Another tenant cannot replay acme's room by naming it.
	h.create(t, globexKey, "globex", "lobby")
	response, reader = openRoomStream(t, web.URL+server.RoomEventsStreamPath+"?room=lobby&after=0", globexKey)
	h.close(t, globexKey, "globex", "lobby")
	event, data = readEvent(t, reader)
	response.Body.Close()
	if event != "room_event" || !strings.Contains(data, `"tenant":"globex"`) {
		t.Fatalf("globex replay: %q %s", event, data)
	}

	request := httptest.NewRequest(http.MethodGet, server.RoomEventsStreamPath+"?after=1", nil)
	request.Header.Set("Authorization", "Bearer "+acmeKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("after without a room: %d", recorder.Code)
	}
}

func TestRoomEventStreamsPerTenantAreLimited(t *testing.T) {
	h := newRoomsHarness(t)
	handler := h.server.RoomsHandler(server.RoomsOptions{MaxStreams: 1, Heartbeat: time.Hour})
	web := httptest.NewServer(handler)
	defer web.Close()
	first, _ := openRoomStream(t, web.URL+server.RoomEventsStreamPath, acmeKey)
	defer first.Body.Close()

	request, _ := http.NewRequest(http.MethodGet, web.URL+server.RoomEventsStreamPath, nil)
	request.Header.Set("Authorization", "Bearer "+acmeKey)
	second, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second stream: %d", second.StatusCode)
	}
	// Another tenant has its own allowance.
	other, _ := openRoomStream(t, web.URL+server.RoomEventsStreamPath, globexKey)
	other.Body.Close()
}
