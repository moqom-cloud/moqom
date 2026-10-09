package pqtls_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/pqtls"
)

func certificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "link"}, DNSNames: []string{"link"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func handshake(t *testing.T, server, client *tls.Config) (tls.CurveID, error) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() {
		conn := tls.Server(a, server)
		done <- conn.Handshake()
	}()
	conn := tls.Client(b, client)
	err := conn.Handshake()
	if err != nil {
		<-done
		return 0, err
	}
	if err := <-done; err != nil {
		return 0, err
	}
	return conn.ConnectionState().CurveID, nil
}

func TestLinksAgreeOnMLKEM1024AndRefuseLess(t *testing.T) {
	cert, pool := certificate(t)
	server := pqtls.Link(&tls.Config{Certificates: []tls.Certificate{cert}})

	for _, curves := range [][]tls.CurveID{{tls.MLKEM1024}, {tls.SecP384r1MLKEM1024}} {
		got, err := handshake(t, server, &tls.Config{RootCAs: pool, ServerName: "link", CurvePreferences: curves})
		if err != nil || got != curves[0] {
			t.Errorf("a client offering %v: %v, %v", curves, got, err)
		}
	}
	for _, curves := range [][]tls.CurveID{{tls.X25519MLKEM768}, {tls.X25519}, {tls.CurveP384}} {
		if _, err := handshake(t, server, &tls.Config{RootCAs: pool, ServerName: "link", CurvePreferences: curves}); err == nil {
			t.Errorf("a link accepted %v", curves)
		}
	}
}

func TestThePublicListenerTakesApplesHybridAndNothingClassical(t *testing.T) {
	cert, pool := certificate(t)
	server := pqtls.Public(&tls.Config{Certificates: []tls.Certificate{cert}})

	if got, err := handshake(t, server, &tls.Config{RootCAs: pool, ServerName: "link", CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}}); err != nil || got != tls.X25519MLKEM768 {
		t.Errorf("Apple's hybrid: %v, %v", got, err)
	}
	if _, err := handshake(t, server, &tls.Config{RootCAs: pool, ServerName: "link", CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256}}); err == nil {
		t.Error("the public listener accepted a classical key exchange")
	}
}
