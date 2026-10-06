// Package listen opens the sockets the control plane serves on.
//
// Two kinds. TCP is the ordinary one and what every deployment has used so
// far. OpenZiti is a zero-trust overlay: the control plane binds a named
// service on it, opens no port at all, and is reachable only by identities the
// overlay's policy allows to dial that service. It is for the management plane
// -- RelayLink, the admin API -- and never for media.
//
// A connection that arrives over the overlay carries the dialing identity,
// authenticated by the overlay, as its remote address (see Peer). That is how a
// handler further up learns which enrolled relay is on the other end without
// trusting anything the relay says about itself.
package listen

import (
	"context"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc/peer"
)

// Kinds of listener.
const (
	KindTCP  = "tcp"
	KindZiti = "ziti"
)

// Spec says where to listen.
type Spec struct {
	Kind string

	// Address is the TCP address, for KindTCP.
	Address string

	// ZitiIdentity is the path to an enrolled OpenZiti identity file, and
	// ZitiService the service to bind, for KindZiti.
	ZitiIdentity string
	ZitiService  string
}

// String describes the spec for a log line, without anything secret in it.
func (s Spec) String() string {
	switch s.Kind {
	case KindZiti:
		return "ziti service " + s.ZitiService
	default:
		return "tcp " + s.Address
	}
}

// Environment variables Overlay reads.
const (
	ZitiIdentityVariable = "MOQOM_CONTROL_ZITI_IDENTITY"
	ZitiServiceVariable  = "MOQOM_CONTROL_ZITI_SERVICE"
)

// Overlay returns the OpenZiti listener a deployment asked for, or nil for none.
//
// Both variables or neither: an identity with no service, or a service with no
// identity, is a half-finished enrolment rather than a choice, and serving
// without the overlay because of it would leave an operator believing the
// management plane is dark when it is not.
func Overlay(getenv func(string) string) (*Spec, error) {
	identity := getenv(ZitiIdentityVariable)
	service := getenv(ZitiServiceVariable)
	if identity == "" && service == "" {
		return nil, nil
	}
	if identity == "" || service == "" {
		return nil, fmt.Errorf("%s and %s go together", ZitiIdentityVariable, ZitiServiceVariable)
	}
	return &Spec{Kind: KindZiti, ZitiIdentity: identity, ZitiService: service}, nil
}

// Open binds a listener for spec.
func Open(ctx context.Context, spec Spec) (net.Listener, error) {
	switch spec.Kind {
	case KindTCP, "":
		var config net.ListenConfig
		return config.Listen(ctx, "tcp", spec.Address)
	case KindZiti:
		if spec.ZitiIdentity == "" || spec.ZitiService == "" {
			return nil, errors.New("a ziti listener needs an identity file and a service name")
		}
		inner, err := openZiti(spec.ZitiIdentity, spec.ZitiService)
		if err != nil {
			return nil, fmt.Errorf("binding ziti service %s: %w", spec.ZitiService, err)
		}
		return Identifying(inner), nil
	default:
		return nil, fmt.Errorf("unknown listener kind %q, want tcp or ziti", spec.Kind)
	}
}

// Peer is the remote address of a connection that arrived over the overlay.
//
// The identity is the overlay's statement about who dialed, made by its
// controller when the circuit was authorised, so it is as trustworthy as the
// overlay's own enrolment.
type Peer struct {
	// ID is the overlay's immutable identifier for the dialing identity.
	ID string
	// Name is the operator-assigned name of that identity, which is what
	// licences are keyed on.
	Name string
}

// Network implements net.Addr.
func (Peer) Network() string { return KindZiti }

// String implements net.Addr.
func (p Peer) String() string { return "ziti:" + p.Name }

// PeerOf returns the overlay identity behind a gRPC call, if the call arrived
// over the overlay.
func PeerOf(ctx context.Context) (Peer, bool) {
	caller, ok := peer.FromContext(ctx)
	if !ok || caller.Addr == nil {
		return Peer{}, false
	}
	overlay, ok := caller.Addr.(Peer)
	if !ok || overlay.Name == "" {
		return Peer{}, false
	}
	return overlay, true
}

// dialerIdentified is the part of an OpenZiti connection this package reads.
//
// Named as an interface rather than taken from the SDK so the wrapping can be
// tested with a fake, and so nothing above this package depends on the SDK's
// types.
type dialerIdentified interface {
	GetDialerIdentityId() string
	GetDialerIdentityName() string
}

// Identifying wraps a listener so that each accepted connection that knows its
// dialer reports it as a Peer from RemoteAddr.
//
// A connection that does not know its dialer is refused, closed and skipped
// rather than passed through anonymous: everything that reaches this listener
// is supposed to have come through the overlay, and one that cannot say who it
// is did not.
func Identifying(inner net.Listener) net.Listener {
	return &identifying{Listener: inner}
}

type identifying struct {
	net.Listener
}

func (l *identifying) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		known, ok := conn.(dialerIdentified)
		if !ok || known.GetDialerIdentityName() == "" {
			_ = conn.Close()
			continue
		}
		return &identifiedConn{
			Conn: conn,
			peer: Peer{ID: known.GetDialerIdentityId(), Name: known.GetDialerIdentityName()},
		}, nil
	}
}

type identifiedConn struct {
	net.Conn
	peer Peer
}

func (c *identifiedConn) RemoteAddr() net.Addr { return c.peer }
