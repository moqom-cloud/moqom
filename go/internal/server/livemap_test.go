package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

const (
	acmeKey     = "acme-backend-key"
	globexKey   = "globex-backend-key"
	acmeJoinKey = "acme-join-only-key"
	operatorKey = "platform-operator-key"
	liveRelay   = "live-relay-key"
)

// revocable is an account set a test can take a key out of.
type revocable struct {
	mu       sync.Mutex
	accounts map[string]server.ServiceAccount
}

func (r *revocable) Lookup(_ context.Context, credential string) (server.ServiceAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account, ok := r.accounts[credential]
	if !ok {
		return server.ServiceAccount{}, server.ErrNoSuchAccount
	}
	return account, nil
}

func (r *revocable) revoke(credential string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.accounts, credential)
}

type liveHarness struct {
	server   *server.Server
	relay    relaypb.RelayLinkClient
	accounts *revocable
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	signing, err := token.GenerateKey()
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	accounts := &revocable{accounts: map[string]server.ServiceAccount{
		acmeKey:     {Name: "acme", Tenant: "acme", Grants: control.Grants{IsOwner: true}},
		globexKey:   {Name: "globex", Tenant: "globex", Grants: control.Grants{IsOwner: true}},
		acmeJoinKey: {Name: "acme-join", Tenant: "acme", Grants: control.Grants{Publish: control.PublishCamera}},
		operatorKey: {Name: "operator"},
		liveRelay:   {Name: "relay", IsRelay: true},
	}}
	instance, err := server.New(server.Config{
		Store: memory.New(now, nil), Accounts: accounts, Issuer: "moqom-test",
		Keys: []token.Key{signing}, Now: now,
	})
	if err != nil {
		t.Fatalf("building the control plane: %v", err)
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
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() {
		_ = connection.Close()
		grpcServer.Stop()
		_ = listener.Close()
	})
	return &liveHarness{server: instance, relay: relaypb.NewRelayLinkClient(connection), accounts: accounts}
}

type liveRelayStream struct {
	stream relaypb.RelayLink_AttachClient
	cancel context.CancelFunc
}

// attach brings up a relay and waits for the control plane to welcome it.
func (h *liveHarness) attach(t *testing.T, id, region string) *liveRelayStream {
	t.Helper()
	ctx, cancel := context.WithCancel(as(liveRelay))
	stream, err := h.relay.Attach(ctx)
	if err != nil {
		cancel()
		t.Fatalf("attaching: %v", err)
	}
	if err := stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_Hello{
		Hello: &relaypb.Hello{RelayId: id, Region: region, Version: "0"},
	}}); err != nil {
		cancel()
		t.Fatalf("hello: %v", err)
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			cancel()
			t.Fatalf("waiting for the welcome: %v", err)
		}
		if message.GetWelcome() != nil {
			break
		}
	}
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	t.Cleanup(cancel)
	return &liveRelayStream{stream: stream, cancel: cancel}
}

func (r *liveRelayStream) census(t *testing.T, tenants ...*relaypb.TenantCensus) {
	t.Helper()
	if err := r.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_Census{
		Census: &relaypb.Census{At: timestamppb.Now(), Tenants: tenants},
	}}); err != nil {
		t.Fatalf("sending a census: %v", err)
	}
}

func (r *liveRelayStream) open(t *testing.T, id, tenant, room string) {
	t.Helper()
	if err := r.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SessionOpened{
		SessionOpened: &relaypb.SessionOpened{Session: &relaypb.SessionState{
			SessionId: id, Tenant: tenant, Room: room,
			Identity: &adminpb.Identity{Username: "user-" + id, Device: "d"},
			OpenedAt: timestamppb.Now(),
		}},
	}}); err != nil {
		t.Fatalf("opening a session: %v", err)
	}
}

func place(country, region, city string, lat, lon int32, sessions uint32) *relaypb.CensusPlace {
	return &relaypb.CensusPlace{Country: country, Region: region, City: city, Latitude: lat, Longitude: lon, Sessions: sessions}
}

func london(n uint32) *relaypb.CensusPlace { return place("GB", "ENG", "London", 52, 0, n) }

// fetch asks for one snapshot and returns the status, the raw body, and the
// snapshot when there is one.
func fetch(t *testing.T, handler http.Handler, credential, query string) (int, string, server.LiveSnapshot) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, server.LivePath+query, nil)
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var snapshot server.LiveSnapshot
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("decoding %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, recorder.Body.String(), snapshot
}

// waitForSnapshot polls until the relay's report has been handled.
func waitForSnapshot(t *testing.T, handler http.Handler, credential, query string, ready func(server.LiveSnapshot) bool) server.LiveSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body, snapshot := fetch(t, handler, credential, query)
		if code == http.StatusOK && ready(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching snapshot: %d %s", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveMapRefusesAnyoneWithoutATenantsBackendKey(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{})

	for _, tc := range []struct {
		name, credential, query string
		want                    int
	}{
		{"no credential", "", "", http.StatusUnauthorized},
		{"an unknown credential", "guess", "", http.StatusUnauthorized},
		{"a relay credential", liveRelay, "", http.StatusForbidden},
		{"a key without owner standing", acmeJoinKey, "", http.StatusForbidden},
		{"a tenant key naming another tenant", acmeKey, "?tenant=globex", http.StatusForbidden},
		{"an operator naming no tenant", operatorKey, "", http.StatusBadRequest},
	} {
		code, body, _ := fetch(t, handler, tc.credential, tc.query)
		if code != tc.want {
			t.Errorf("%s: got %d (%s), want %d", tc.name, code, body, tc.want)
		}
	}

	// The credential is a header and nothing else. A key in the URL would be in
	// every access log between the browser and here.
	request := httptest.NewRequest(http.MethodGet, server.LivePath+"?access_token="+acmeKey, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("a key in the query string was accepted: %d", recorder.Code)
	}
}

func TestLiveMapShowsATenantOnlyItsOwnConnections(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{})
	relay := h.attach(t, "relay-a", "us-west1")
	relay.open(t, "1", "acme", "lobby")
	relay.open(t, "2", "acme", "stage")
	relay.open(t, "3", "globex", "secret-room")
	relay.census(t,
		&relaypb.TenantCensus{Tenant: "acme", Publishers: 1, Viewers: 5, Places: []*relaypb.CensusPlace{london(4), place("FR", "IDF", "Paris", 49, 2, 1)}, Unlocated: 1},
		&relaypb.TenantCensus{Tenant: "globex", Viewers: 7, Places: []*relaypb.CensusPlace{place("JP", "13", "Tokyo", 36, 140, 7)}},
	)

	acme := waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions > 0 })
	if acme.Tenant != "acme" || acme.Sessions != 6 || acme.Publishers != 1 || acme.Viewers != 5 {
		t.Errorf("acme totals: %+v", acme)
	}
	if acme.Rooms != 2 {
		t.Errorf("acme has %d rooms, want 2", acme.Rooms)
	}
	if len(acme.Places) != 1 || acme.Places[0].City != "London" || acme.Places[0].Sessions != 4 {
		t.Errorf("acme places: %+v", acme.Places)
	}
	// Paris holds one person and the unlocated session is nowhere: both are
	// counted and neither is placed.
	if acme.Withheld != 2 {
		t.Errorf("acme withheld %d, want 2", acme.Withheld)
	}

	_, body, _ := fetch(t, handler, acmeKey, "")
	for _, leaked := range []string{"globex", "Tokyo", "JP", "secret-room", "lobby", "user-"} {
		if strings.Contains(body, leaked) {
			t.Errorf("acme's snapshot contains %q: %s", leaked, body)
		}
	}

	globex := waitForSnapshot(t, handler, globexKey, "", func(s server.LiveSnapshot) bool { return s.Sessions > 0 })
	if globex.Sessions != 7 || len(globex.Places) != 1 || globex.Places[0].City != "Tokyo" {
		t.Errorf("globex: %+v", globex)
	}

	operator := waitForSnapshot(t, handler, operatorKey, "?tenant=globex", func(s server.LiveSnapshot) bool { return s.Sessions > 0 })
	if operator.Tenant != "globex" || operator.Sessions != 7 {
		t.Errorf("operator view of globex: %+v", operator)
	}
}

// Two relays with two people each in one city is a city of four, which is
// shown. Withholding per relay would have hidden it.
func TestLiveMapMergesRelaysBeforeWithholding(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{})
	h.attach(t, "relay-a", "us-west1").census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 2, Places: []*relaypb.CensusPlace{london(2)}})
	h.attach(t, "relay-b", "us-west2").census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 2, Places: []*relaypb.CensusPlace{london(2)}})

	snapshot := waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions == 4 })
	if len(snapshot.Places) != 1 || snapshot.Places[0].Sessions != 4 {
		t.Errorf("places: %+v", snapshot.Places)
	}
	// Each region holds two, which is below the threshold too.
	if len(snapshot.Regions) != 0 || snapshot.RegionsWithheld != 4 {
		t.Errorf("regions: %+v withheld %d", snapshot.Regions, snapshot.RegionsWithheld)
	}
}

func TestLiveMapPoolsSmallCitiesIntoTheirCountry(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{})
	h.attach(t, "relay-a", "us-west1").census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 3, Places: []*relaypb.CensusPlace{
		place("FR", "ARA", "Lyon", 46, 5, 2),
		place("FR", "HDF", "Lille", 51, 3, 1),
	}})

	snapshot := waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions == 3 })
	if len(snapshot.Places) != 1 {
		t.Fatalf("places: %+v", snapshot.Places)
	}
	france := snapshot.Places[0]
	if france.Country != "FR" || france.City != "" || france.Region != "" || france.Sessions != 3 {
		t.Errorf("pooled place: %+v", france)
	}
	if snapshot.Regions[0].Region != "us-west1" || snapshot.Regions[0].Sessions != 3 {
		t.Errorf("regions: %+v", snapshot.Regions)
	}
}

func TestLiveMapCannotBeConfiguredToShowOnePerson(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{Disclosure: server.Disclosure{MinCount: 1}})
	h.attach(t, "relay-a", "us-west1").census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 1, Places: []*relaypb.CensusPlace{london(1)}})

	snapshot := waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions == 1 })
	if len(snapshot.Places) != 0 || snapshot.Withheld != 1 || snapshot.Disclosure.MinCount != 2 {
		t.Errorf("a single session was placed: %+v", snapshot)
	}
}

func TestLiveMapRoundsWhenConfigured(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{Disclosure: server.Disclosure{MinCount: 3, RoundTo: 5}})
	h.attach(t, "relay-a", "us-west1").census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 11, Places: []*relaypb.CensusPlace{london(3), place("US", "WA", "Seattle", 48, -122, 8)}})

	snapshot := waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions == 11 })
	got := map[string]int{}
	for _, p := range snapshot.Places {
		got[p.City] = p.Sessions
	}
	if got["London"] != 5 || got["Seattle"] != 10 {
		t.Errorf("rounded places: %+v", snapshot.Places)
	}
}

// A relay that goes away takes its census with it: the map shows who is
// connected now, not who was.
func TestLiveMapForgetsADetachedRelay(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{})
	relay := h.attach(t, "relay-a", "us-west1")
	relay.census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 3, Places: []*relaypb.CensusPlace{london(3)}})
	waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions == 3 })

	relay.cancel()
	waitForSnapshot(t, handler, acmeKey, "", func(s server.LiveSnapshot) bool { return s.Sessions == 0 && len(s.Places) == 0 })
}

func readEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "" && (event != "" || data != ""):
			return event, data
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestLiveStreamSendsSnapshotsUntilTheKeyIsRevoked(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{Interval: 20 * time.Millisecond, Recheck: 50 * time.Millisecond})
	h.attach(t, "relay-a", "us-west1").census(t, &relaypb.TenantCensus{Tenant: "acme", Viewers: 3, Places: []*relaypb.CensusPlace{london(3)}})
	web := httptest.NewServer(handler)
	defer web.Close()

	request, _ := http.NewRequest(http.MethodGet, web.URL+server.LiveStreamPath, nil)
	request.Header.Set("Authorization", "Bearer "+acmeKey)
	response, err := web.Client().Do(request)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
	reader := bufio.NewReader(response.Body)

	sawLondon := false
	for i := 0; i < 50 && !sawLondon; i++ {
		event, data := readEvent(t, reader)
		if event != "snapshot" {
			t.Fatalf("event %q", event)
		}
		var snapshot server.LiveSnapshot
		if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
			t.Fatalf("decoding %q: %v", data, err)
		}
		sawLondon = len(snapshot.Places) == 1 && snapshot.Places[0].City == "London"
	}
	if !sawLondon {
		t.Fatal("the stream never showed the census")
	}

	h.accounts.revoke(acmeKey)
	for i := 0; i < 100; i++ {
		event, _ := readEvent(t, reader)
		if event == "revoked" {
			return
		}
	}
	t.Fatal("a revoked key kept its stream")
}

func TestLiveStreamsPerTenantAreLimited(t *testing.T) {
	h := newLiveHarness(t)
	handler := h.server.LiveHandler(server.LiveOptions{MaxStreams: 1, Interval: time.Hour})
	web := httptest.NewServer(handler)
	defer web.Close()

	open := func() *http.Response {
		request, _ := http.NewRequest(http.MethodGet, web.URL+server.LiveStreamPath, nil)
		request.Header.Set("Authorization", "Bearer "+acmeKey)
		response, err := web.Client().Do(request)
		if err != nil {
			t.Fatalf("opening a stream: %v", err)
		}
		return response
	}
	first := open()
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first stream: %d", first.StatusCode)
	}
	readEvent(t, bufio.NewReader(first.Body))

	second := open()
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second stream: %d, want 429", second.StatusCode)
	}
}
