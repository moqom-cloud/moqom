package listen

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
)

// fakeConn is an overlay connection as the SDK presents one: a net.Conn that
// also knows who dialed it.
type fakeConn struct {
	net.Conn
	id, name string
}

func (c fakeConn) GetDialerIdentityId() string   { return c.id }
func (c fakeConn) GetDialerIdentityName() string { return c.name }

// fakeListener hands out whatever connections it is given, standing in for the
// overlay so the wrapping can be tested without a controller.
type fakeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newFakeListener() *fakeListener {
	return &fakeListener{conns: make(chan net.Conn, 4), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return Peer{Name: "control"} }

// dial makes a connected pair and queues the server half, wrapped as the
// overlay would present it.
func (l *fakeListener) dial(id, name string) net.Conn {
	server, client := net.Pipe()
	if name == "" && id == "" {
		l.conns <- server
	} else {
		l.conns <- fakeConn{Conn: server, id: id, name: name}
	}
	return client
}

func TestAnOverlayConnectionReportsItsDialer(t *testing.T) {
	inner := newFakeListener()
	listener := Identifying(inner)
	defer listener.Close()

	client := inner.dial("id-1", "byoc-acme-1")
	defer client.Close()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, ok := conn.RemoteAddr().(Peer)
	if !ok {
		t.Fatalf("remote address is %T, want Peer", conn.RemoteAddr())
	}
	if got.ID != "id-1" || got.Name != "byoc-acme-1" {
		t.Fatalf("peer = %+v", got)
	}
	if got.Network() != KindZiti || got.String() != "ziti:byoc-acme-1" {
		t.Fatalf("addr = %s/%s", got.Network(), got.String())
	}
}

func TestAConnectionThatCannotNameItsDialerIsDropped(t *testing.T) {
	inner := newFakeListener()
	listener := Identifying(inner)
	defer listener.Close()

	anonymous := inner.dial("", "")
	defer anonymous.Close()
	unnamed := inner.dial("id-only", "")
	defer unnamed.Close()
	named := inner.dial("id-2", "relay-2")
	defer named.Close()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().(Peer).Name; got != "relay-2" {
		t.Fatalf("accepted %q; the two without a name should have been skipped", got)
	}

	// The skipped ones were closed rather than left hanging.
	_ = anonymous.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := anonymous.Read(make([]byte, 1)); err == nil {
		t.Fatal("an anonymous connection was left open")
	}
}

func TestClosingTheListenerEndsAccept(t *testing.T) {
	inner := newFakeListener()
	listener := Identifying(inner)
	_ = listener.Close()
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenRefusesAnIncompleteZitiSpec(t *testing.T) {
	_, err := Open(context.Background(), Spec{Kind: KindZiti, ZitiService: "moqom-control"})
	if err == nil || !strings.Contains(err.Error(), "identity file") {
		t.Fatalf("err = %v", err)
	}
	_, err = Open(context.Background(), Spec{Kind: "udp"})
	if err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}

func TestOpenReportsAnUnreadableIdentity(t *testing.T) {
	_, err := Open(context.Background(), Spec{
		Kind: KindZiti, ZitiIdentity: t.TempDir() + "/missing.json", ZitiService: "moqom-control",
	})
	if err == nil || !strings.Contains(err.Error(), "moqom-control") {
		t.Fatalf("err = %v", err)
	}
}

func TestOverlayIsBothVariablesOrNeither(t *testing.T) {
	environment := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	none, err := Overlay(environment(nil))
	if err != nil || none != nil {
		t.Fatalf("no variables gave %+v, %v", none, err)
	}

	both, err := Overlay(environment(map[string]string{
		ZitiIdentityVariable: "/run/ziti/control.json",
		ZitiServiceVariable:  "moqom-control",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if both.Kind != KindZiti || both.ZitiIdentity != "/run/ziti/control.json" || both.ZitiService != "moqom-control" {
		t.Fatalf("spec = %+v", both)
	}

	for _, half := range []map[string]string{
		{ZitiIdentityVariable: "/run/ziti/control.json"},
		{ZitiServiceVariable: "moqom-control"},
	} {
		if spec, err := Overlay(environment(half)); err == nil {
			t.Fatalf("half an enrolment %v was accepted as %+v", half, spec)
		}
	}
}

func TestOpenTCP(t *testing.T) {
	listener, err := Open(context.Background(), Spec{Kind: KindTCP, Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, ok := listener.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("addr = %T", listener.Addr())
	}
	if got := (Spec{Kind: KindTCP, Address: ":8443"}).String(); got != "tcp :8443" {
		t.Fatal(got)
	}
	if got := (Spec{Kind: KindZiti, ZitiService: "s", ZitiIdentity: "/secret"}).String(); strings.Contains(got, "secret") {
		t.Fatalf("the identity path leaked into %q", got)
	}
}

// identityReporter is a health server that records who called it, standing
// in for a real handler reading PeerOf.
type identityReporter struct {
	healthpb.UnimplementedHealthServer
	seen chan Peer
}

func (r *identityReporter) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if overlay, ok := PeerOf(ctx); ok {
		r.seen <- overlay
	} else {
		r.seen <- Peer{}
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

// The identity has to survive gRPC: a handler reads it from the call's peer,
// which is the connection's remote address after whatever the transport did.
func TestAGRPCHandlerSeesTheOverlayIdentity(t *testing.T) {
	inner := newFakeListener()
	reporter := &identityReporter{seen: make(chan Peer, 1)}
	server := grpc.NewServer()
	healthpb.RegisterHealthServer(server, reporter)
	go func() { _ = server.Serve(Identifying(inner)) }()
	defer server.Stop()

	conn, err := grpc.NewClient("passthrough:///overlay",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return inner.dial("id-9", "byoc-acme-9"), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := <-reporter.seen; got.Name != "byoc-acme-9" || got.ID != "id-9" {
		t.Fatalf("handler saw %+v", got)
	}
}

func TestPeerOfIgnoresOrdinaryAddresses(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1}})
	if _, ok := PeerOf(ctx); ok {
		t.Fatal("a TCP peer was taken for an overlay identity")
	}
	if _, ok := PeerOf(context.Background()); ok {
		t.Fatal("no peer at all was taken for an overlay identity")
	}
}

// TLS runs on top of the overlay, and the identity must still be visible
// through it: tls.Conn reports the underlying connection's remote address.
func TestTheIdentitySurvivesTLS(t *testing.T) {
	var conn net.Conn = &identifiedConn{Conn: nil, peer: Peer{Name: "relay"}}
	wrapped := tls.Server(conn, &tls.Config{})
	if got, ok := wrapped.RemoteAddr().(Peer); !ok || got.Name != "relay" {
		t.Fatalf("through TLS the address is %v", wrapped.RemoteAddr())
	}
}
