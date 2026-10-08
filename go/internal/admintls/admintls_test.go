package admintls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// files writes a self-signed authority and a client pair to a temporary
// directory and returns their paths.
func files(t *testing.T) (authority, certificate, key string) {
	t.Helper()
	dir := t.TempDir()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "operator"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, kind string, bytes []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: bytes}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	certificate = write("client.crt", "CERTIFICATE", der)
	return certificate, certificate, write("client.key", "EC PRIVATE KEY", keyDER)
}

func set(t *testing.T, authority, certificate, key, name string) {
	t.Helper()
	t.Setenv(CAVariable, authority)
	t.Setenv(CertificateVariable, certificate)
	t.Setenv(KeyVariable, key)
	t.Setenv(ServerNameVariable, name)
}

func TestNoFilesIsPlaintext(t *testing.T) {
	set(t, "", "", "", "")
	creds, err := Credentials()
	if err != nil || creds != nil {
		t.Fatalf("Credentials() = %v, %v; want nil, nil", creds, err)
	}
}

func TestAPartialSetIsAMistake(t *testing.T) {
	authority, certificate, _ := files(t)
	set(t, authority, certificate, "", "")
	if _, err := Credentials(); err == nil || !strings.Contains(err.Error(), "go together") {
		t.Fatalf("Credentials() error = %v; want the three variables named together", err)
	}
}

func TestAFullSetNamesTheControlPlane(t *testing.T) {
	authority, certificate, key := files(t)

	set(t, authority, certificate, key, "")
	creds, err := Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().ServerName; got != DefaultServerName {
		t.Errorf("server name = %q; want %q", got, DefaultServerName)
	}

	set(t, authority, certificate, key, "control.example")
	creds, err = Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().ServerName; got != "control.example" {
		t.Errorf("server name = %q; want the override", got)
	}
}

func TestAnAuthorityWithoutCertificatesIsRefused(t *testing.T) {
	_, certificate, key := files(t)
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set(t, empty, certificate, key, "")
	if _, err := Credentials(); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("Credentials() error = %v; want the empty authority refused", err)
	}
}
