// Package admintls builds the transport credentials an operator's own tools
// present to a deployed control plane.
//
// The admin API and RelayLink share a port, and that port asks every caller for
// a certificate. It is the same requirement for both, for the same reason: the
// bearer credential says who a caller claims to be, and the certificate says
// they are something this deployment issued to. A stolen credential alone gets
// nowhere.
//
// Reachability is not the argument. These tools reach the control plane through
// a port-forward, which is already private and already requires cluster access;
// the certificate is what makes that access auditable as a deployment identity
// rather than as whoever holds a hex string.
package admintls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// Environment variables read here, set by deploy/tools/join.sh.
const (
	CAVariable          = "MOQOM_ADMIN_CA"
	CertificateVariable = "MOQOM_ADMIN_CERT"
	KeyVariable         = "MOQOM_ADMIN_KEY"
	ServerNameVariable  = "MOQOM_ADMIN_SERVER_NAME"
)

// DefaultServerName is what the control plane's certificate says it is.
//
// A name rather than the address it is reached at, because the address is a
// load balancer's current value and a port-forward's is 127.0.0.1 -- neither of
// which is an identity, and one of which changes.
const DefaultServerName = "moqom-control"

// Credentials returns what to dial with, or nil for plaintext.
//
// Nil is the local case: a control plane on a laptop, or a test suite's
// in-process server, neither of which has a certificate. All three files or
// none -- a partial set is a mistake rather than a mode, and one that would
// otherwise surface as a handshake failure several layers from its cause.
func Credentials() (credentials.TransportCredentials, error) {
	authority := os.Getenv(CAVariable)
	certificate := os.Getenv(CertificateVariable)
	key := os.Getenv(KeyVariable)

	if authority == "" && certificate == "" && key == "" {
		return nil, nil
	}
	if authority == "" || certificate == "" || key == "" {
		return nil, fmt.Errorf("%s, %s and %s go together", CAVariable, CertificateVariable, KeyVariable)
	}

	pair, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return nil, fmt.Errorf("reading the client certificate: %w", err)
	}

	pem, err := os.ReadFile(authority)
	if err != nil {
		return nil, fmt.Errorf("reading the authority: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("the authority holds no certificates")
	}

	name := os.Getenv(ServerNameVariable)
	if name == "" {
		name = DefaultServerName
	}

	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		// The name the certificate carries, which is not the host being dialled:
		// through a port-forward that host is the loopback, and a certificate
		// for 127.0.0.1 would be a certificate that says nothing.
		ServerName: name,
		MinVersion: tls.VersionTLS13,
	}), nil
}
