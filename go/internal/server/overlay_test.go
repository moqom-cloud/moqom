package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/byoc"
	"github.com/moqom-cloud/moqom/go/internal/byocpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/gceidentity"
	"github.com/moqom-cloud/moqom/go/internal/listen"
	"github.com/moqom-cloud/moqom/go/internal/overlay"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// fakeIdentities is the overlay's management API, in memory.
type fakeIdentities struct {
	mu     sync.Mutex
	byID   map[string]overlay.Identity
	nextID int
}

func newFakeIdentities() *fakeIdentities {
	return &fakeIdentities{byID: map[string]overlay.Identity{}}
}

func (f *fakeIdentities) Create(_ context.Context, asked overlay.NewIdentity) (overlay.Enrolment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if existing.Name == asked.Name {
			return overlay.Enrolment{}, errors.New("name in use")
		}
	}
	f.nextID++
	id := fmt.Sprintf("zid%d", f.nextID)
	f.byID[id] = overlay.Identity{ID: id, Name: asked.Name, RoleAttributes: asked.RoleAttributes, Tags: asked.Tags}
	return overlay.Enrolment{ID: id, Name: asked.Name, JWT: "jwt-" + id, ExpiresAt: asked.TokenExpires}, nil
}

func (f *fakeIdentities) Find(_ context.Context, name string) (overlay.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, identity := range f.byID {
		if identity.Name == name {
			return identity, nil
		}
	}
	return overlay.Identity{}, overlay.ErrNotFound
}

func (f *fakeIdentities) WithRole(_ context.Context, role string) ([]overlay.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []overlay.Identity
	for _, identity := range f.byID {
		for _, r := range identity.RoleAttributes {
			if r == role {
				out = append(out, identity)
			}
		}
	}
	return out, nil
}

func (f *fakeIdentities) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byID, id)
	return nil
}

func (f *fakeIdentities) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

type fakeInstances map[string]gceidentity.Instance

func (f fakeInstances) Verify(_ context.Context, raw string) (gceidentity.Instance, error) {
	if instance, ok := f[raw]; ok {
		return instance, nil
	}
	return gceidentity.Instance{}, gceidentity.ErrNotTrusted
}

// overlayConn is a connection the way the OpenZiti SDK hands one over: it
// knows which identity dialed.
type overlayConn struct {
	net.Conn
	name string
}

func (c overlayConn) GetDialerIdentityId() string   { return "id-" + c.name }
func (c overlayConn) GetDialerIdentityName() string { return c.name }

type overlayListener struct {
	*bufconn.Listener
	name string
}

func (l overlayListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return overlayConn{Conn: conn, name: l.name}, nil
}

type overlayHarness struct {
	server      *server.Server
	grpc        *grpc.Server
	identities  *fakeIdentities
	enrollments *byoc.Memory
	plain       *grpc.ClientConn
}

// newOverlayHarness serves the overlay API service as the control plane
// does, with a plain listener beside it for calls that did not come over the
// overlay.
func newOverlayHarness(t *testing.T, overlayOnly bool) *overlayHarness {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	signing, err := token.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	h := &overlayHarness{identities: newFakeIdentities(), enrollments: byoc.NewMemory()}
	h.server, err = server.New(server.Config{
		Store: memory.New(now, nil),
		Accounts: server.NewStaticAccounts(map[string]server.ServiceAccount{
			ownerKey: {Name: "owner-backend", Tenant: tenant, Grants: control.Grants{IsOwner: true}},
			modKey:   {Name: "mod-backend", Tenant: tenant, Grants: control.Grants{Moderation: control.CapKick}},
			relayKey: {Name: "relay-1", IsRelay: true},
		}),
		Issuer: issuerFor,
		Keys:   []token.Key{signing},
		Now:    now,
		Overlay: &server.OverlayConfig{
			Identities:        h.identities,
			Enrollments:       h.enrollments,
			Instances:         fakeInstances{"good-token": {ID: "77", Name: "moqom-dev-relay-us-west1-abcd", Zone: "us-west1-b", Project: "p"}},
			CustomerPrefix:    "moqom-test-byoc-",
			RelaysOverlayOnly: overlayOnly,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	unary, streaming := h.server.OverlayInterceptors()
	h.grpc = grpc.NewServer(grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(streaming))
	h.server.Register(h.grpc)
	h.server.RegisterOverlay(h.grpc)
	t.Cleanup(h.grpc.Stop)
	h.plain = h.dial(t, nil)
	return h
}

// dial opens a client connection; with a name, it arrives over the overlay
// as that identity.
func (h *overlayHarness) dial(t *testing.T, name *string) *grpc.ClientConn {
	t.Helper()
	inner := bufconn.Listen(1 << 20)
	var listener net.Listener = inner
	if name != nil {
		listener = listen.Identifying(overlayListener{Listener: inner, name: *name})
	}
	go func() { _ = h.grpc.Serve(listener) }()
	connection, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return inner.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(); _ = inner.Close() })
	return connection
}

func (h *overlayHarness) enroll(t *testing.T) *byocpb.CreateResponse {
	t.Helper()
	created, err := byocpb.NewRelayEnrollmentsClient(h.plain).Create(as(ownerKey),
		&byocpb.CreateRequest{Tenant: tenant, Name: "on-prem 1"})
	if err != nil {
		t.Fatalf("creating an enrolment: %v", err)
	}
	return created
}

type attached struct {
	stream    relaypb.RelayLink_AttachClient
	snapshots chan *relaypb.RoomSnapshot
	ended     chan error
}

func attach(t *testing.T, ctx context.Context, connection *grpc.ClientConn, relayID string) (*attached, error) {
	t.Helper()
	stream, err := relaypb.NewRelayLinkClient(connection).Attach(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_Hello{
		Hello: &relaypb.Hello{RelayId: relayID, Region: "on-prem"},
	}}); err != nil {
		return nil, err
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if first.GetWelcome() == nil {
		t.Fatalf("the first message was %T, want a Welcome", first.GetBody())
	}
	a := &attached{stream: stream, snapshots: make(chan *relaypb.RoomSnapshot, 8), ended: make(chan error, 1)}
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				a.ended <- err
				return
			}
			if snapshot := message.GetRoomSnapshot(); snapshot != nil {
				a.snapshots <- snapshot
			}
		}
	}()
	return a, nil
}

func (a *attached) open(t *testing.T, session, tenantName string) {
	t.Helper()
	if err := a.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SessionOpened{
		SessionOpened: &relaypb.SessionOpened{Session: &relaypb.SessionState{
			SessionId: session, Tenant: tenantName, Room: roomName,
			Identity: &adminpb.Identity{Username: "dave", Device: "d1"},
			OpenedAt: timestamppb.Now(),
		}},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestACustomerRelayIsItsEnrolmentAndServesOnlyItsTenant(t *testing.T) {
	h := newOverlayHarness(t, false)
	created := h.enroll(t)
	if created.GetEnrollmentToken() == "" || created.GetEnrollment().GetIdentityName() == "" {
		t.Fatalf("created %v", created)
	}
	identity := created.GetEnrollment().GetIdentityName()

	// No credential at all: the overlay identity is the credential.
	relay, err := attach(t, context.Background(), h.dial(t, &identity), "whatever-it-likes")
	if err != nil {
		t.Fatalf("an enrolled relay could not attach: %v", err)
	}

	// Another tenant's session is ignored, so no snapshot is sent for it.
	relay.open(t, "s-globex", "globex")
	relay.open(t, "s-acme", tenant)
	snapshot := <-relay.snapshots
	if snapshot.GetTenant() != tenant {
		t.Fatalf("the first snapshot was for %s, want %s", snapshot.GetTenant(), tenant)
	}
	if err := relay.stream.Send(&relaypb.RelayMessage{Body: &relaypb.RelayMessage_SnapshotRequest{
		SnapshotRequest: &relaypb.SnapshotRequest{Tenant: "globex", Room: roomName},
	}}); err != nil {
		t.Fatal(err)
	}
	select {
	case leaked := <-relay.snapshots:
		t.Fatalf("a customer relay was sent another tenant's room: %v", leaked)
	case <-time.After(200 * time.Millisecond):
	}

	// A customer relay's identity is not a way into the admin API.
	_, err = adminpb.NewRoomsClient(h.dial(t, &identity)).ListRooms(context.Background(), &adminpb.ListRoomsRequest{Tenant: tenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a customer relay reached the admin API: %v", err)
	}
}

func TestRevokingCutsTheStreamAndTheIdentity(t *testing.T) {
	h := newOverlayHarness(t, false)
	created := h.enroll(t)
	identity := created.GetEnrollment().GetIdentityName()
	relay, err := attach(t, context.Background(), h.dial(t, &identity), "")
	if err != nil {
		t.Fatal(err)
	}

	enrollments := byocpb.NewRelayEnrollmentsClient(h.plain)
	if _, err := enrollments.Revoke(as(ownerKey), &byocpb.RevokeRequest{Tenant: tenant, Id: created.GetEnrollment().GetId()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-relay.ended:
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("the stream ended with %v, want PermissionDenied", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked relay's stream was not cut")
	}
	if h.identities.count() != 0 {
		t.Error("the overlay identity outlived its revocation")
	}
	if _, err := attach(t, context.Background(), h.dial(t, &identity), ""); status.Code(err) != codes.Unauthenticated {
		t.Errorf("a revoked relay reattached: %v", err)
	}

	listed, err := enrollments.List(as(ownerKey), &byocpb.ListRequest{Tenant: tenant})
	if err != nil || len(listed.GetEnrollments()) != 1 || listed.GetEnrollments()[0].GetRevokedAt() == nil {
		t.Errorf("the list after revoking is %v (%v)", listed, err)
	}
}

func TestOnlyAnOwnerOfTheTenantEnrolsARelay(t *testing.T) {
	h := newOverlayHarness(t, false)
	enrollments := byocpb.NewRelayEnrollmentsClient(h.plain)
	cases := map[string]struct {
		credential string
		tenant     string
	}{
		"a moderator key":  {modKey, tenant},
		"another tenant":   {ownerKey, "globex"},
		"a relay key":      {relayKey, tenant},
		"no tenant at all": {ownerKey, ""},
	}
	for name, c := range cases {
		if _, err := enrollments.Create(as(c.credential), &byocpb.CreateRequest{Tenant: c.tenant, Name: "x"}); err == nil {
			t.Errorf("%s enrolled a relay", name)
		}
	}
	if h.identities.count() != 0 {
		t.Error("a refused enrolment left an identity behind")
	}
}

func TestAManagedRelayCannotPoseAsACustomerRelay(t *testing.T) {
	h := newOverlayHarness(t, false)
	_, err := attach(t, as(relayKey), h.plain, "byoc:moqom-test-byoc-acme-1")
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a managed relay attached under a customer's name: %v", err)
	}
	if _, err := attach(t, as(relayKey), h.plain, "relay-1"); err != nil {
		t.Errorf("a managed relay over TCP was refused with the overlay optional: %v", err)
	}
}

func TestOverlayOnlyRefusesTheTCPPath(t *testing.T) {
	h := newOverlayHarness(t, true)
	if _, err := attach(t, as(relayKey), h.plain, "relay-1"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a relay over TCP attached with the overlay required: %v", err)
	}
	name := "moqom-dev-relay-us-west1-abcd"
	if _, err := attach(t, as(relayKey), h.dial(t, &name), "relay-1"); err != nil {
		t.Errorf("a managed relay over the overlay was refused: %v", err)
	}
}

func enrollManaged(t *testing.T, h *overlayHarness, credential, instanceToken string) (int, server.RelayEnrollResponse) {
	t.Helper()
	body, _ := json.Marshal(server.RelayEnrollRequest{InstanceToken: instanceToken})
	request := httptest.NewRequest(http.MethodPost, server.RelayEnrollPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+credential)
	recorder := httptest.NewRecorder()
	h.server.PublicHandler().ServeHTTP(recorder, request)
	var response server.RelayEnrollResponse
	raw, _ := io.ReadAll(recorder.Body)
	_ = json.Unmarshal(raw, &response)
	return recorder.Code, response
}

func TestAManagedRelayVMIsMintedItsIdentity(t *testing.T) {
	h := newOverlayHarness(t, false)

	code, minted := enrollManaged(t, h, relayKey, "good-token")
	if code != http.StatusOK || minted.Identity != "moqom-dev-relay-us-west1-abcd" || minted.EnrollmentToken == "" {
		t.Fatalf("minting gave %d %+v", code, minted)
	}
	found, err := h.identities.Find(context.Background(), minted.Identity)
	if err != nil || found.Tags["instance_id"] != "77" {
		t.Errorf("the minted identity is %+v (%v)", found, err)
	}

	// Asking again replaces it rather than adding a second.
	if code, _ := enrollManaged(t, h, relayKey, "good-token"); code != http.StatusOK || h.identities.count() != 1 {
		t.Errorf("re-minting gave %d with %d identities", code, h.identities.count())
	}

	if code, _ := enrollManaged(t, h, relayKey, "forged"); code != http.StatusForbidden {
		t.Errorf("an untrusted instance token gave %d", code)
	}
	if code, _ := enrollManaged(t, h, ownerKey, "good-token"); code != http.StatusUnauthorized {
		t.Errorf("a tenant key gave %d", code)
	}
}

func TestReconcilingDeletesIdentitiesOfGoneVMs(t *testing.T) {
	h := newOverlayHarness(t, false)
	ctx := context.Background()
	mint := func(name, instanceID string) {
		_, err := h.identities.Create(ctx, overlay.NewIdentity{
			Name: name, RoleAttributes: []string{server.RoleManagedRelay, server.RoleMintedRelay},
			Tags: map[string]string{"instance_id": instanceID},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mint("relay-a", "1")
	mint("relay-b", "2")
	mint("relay-c", "3")
	// An operator's identity is not the reconciler's business.
	if _, err := h.identities.Create(ctx, overlay.NewIdentity{Name: "elliott", RoleAttributes: []string{"moqom-operator"}}); err != nil {
		t.Fatal(err)
	}

	if deleted, err := h.server.ReconcileRelayIdentities(ctx, map[string]string{}); err != nil || len(deleted) != 0 {
		t.Errorf("an empty listing deleted %v (%v)", deleted, err)
	}
	// relay-b is gone; relay-c was recreated under its name.
	deleted, err := h.server.ReconcileRelayIdentities(ctx, map[string]string{"relay-a": "1", "relay-c": "30"})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || h.identities.count() != 2 {
		t.Errorf("deleted %v, leaving %d", deleted, h.identities.count())
	}
	if _, err := h.identities.Find(ctx, "relay-a"); err != nil {
		t.Error("a live VM's identity was deleted")
	}
	if _, err := h.identities.Find(ctx, "elliott"); err != nil {
		t.Error("an operator identity was deleted")
	}
}
