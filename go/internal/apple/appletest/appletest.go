// Package appletest fakes Apple for tests: an App Attest authority, attested
// devices, and a Sign in with Apple key server.
package appletest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// The AAGUIDs App Attest puts in authenticator data.
var (
	AAGUIDProduction  = append([]byte("appattest"), make([]byte, 7)...)
	AAGUIDDevelopment = []byte("appattestdevelop")
)

var nonceExtension = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

// CBOR writes the subset App Attest uses, with map keys sorted so the output is
// deterministic.
func CBOR(value any) []byte {
	var out bytes.Buffer
	head := func(major byte, n uint64) {
		switch {
		case n < 24:
			out.WriteByte(major<<5 | byte(n))
		case n <= 0xff:
			out.Write([]byte{major<<5 | 24, byte(n)})
		case n <= 0xffff:
			out.WriteByte(major<<5 | 25)
			_ = binary.Write(&out, binary.BigEndian, uint16(n))
		default:
			out.WriteByte(major<<5 | 26)
			_ = binary.Write(&out, binary.BigEndian, uint32(n))
		}
	}
	var write func(any)
	write = func(v any) {
		switch v := v.(type) {
		case uint64:
			head(0, v)
		case []byte:
			head(2, uint64(len(v)))
			out.Write(v)
		case string:
			head(3, uint64(len(v)))
			out.WriteString(v)
		case []any:
			head(4, uint64(len(v)))
			for _, item := range v {
				write(item)
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			head(5, uint64(len(keys)))
			for _, key := range keys {
				write(key)
				write(v[key])
			}
		default:
			panic("appletest.CBOR: unsupported type")
		}
	}
	write(value)
	return out.Bytes()
}

// Authority stands in for Apple's App Attest CA.
type Authority struct {
	Roots        *x509.CertPool
	intermediate *x509.Certificate
	key          *ecdsa.PrivateKey
	now          time.Time
}

// NewAuthority makes a root and an intermediate valid around now.
func NewAuthority(t testing.TB, now time.Time) Authority {
	t.Helper()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test App Attestation Root CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	intermediateTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test App Attestation CA 1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	intermediateDER, err := x509.CreateCertificate(rand.Reader, intermediateTemplate, root, &intermediateKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, _ := x509.ParseCertificate(intermediateDER)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return Authority{Roots: roots, intermediate: intermediate, key: intermediateKey, now: now}
}

// Device is an App Attest key on a device.
type Device struct {
	Key   *ecdsa.PrivateKey
	KeyID string
}

// NewDevice makes a key and the ID App Attest gives it.
func NewDevice(t testing.TB) Device {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point, _ := key.PublicKey.ECDH()
	digest := sha256.Sum256(point.Bytes())
	return Device{Key: key, KeyID: base64.StdEncoding.EncodeToString(digest[:])}
}

// Spoil names the parts of an attestation a test may get wrong.
type Spoil struct {
	AAGUID   []byte
	Counter  uint32
	NonceFor []byte
}

// Attest makes what DCAppAttestService.attestKey returns.
func (a Authority) Attest(t testing.TB, device Device, clientDataHash []byte, appID string, spoil Spoil) []byte {
	t.Helper()
	aaguid := AAGUIDProduction
	if spoil.AAGUID != nil {
		aaguid = spoil.AAGUID
	}
	keyID, _ := base64.StdEncoding.DecodeString(device.KeyID)
	rpID := sha256.Sum256([]byte(appID))
	authData := append([]byte{}, rpID[:]...)
	authData = append(authData, 0x41)
	authData = binary.BigEndian.AppendUint32(authData, spoil.Counter)
	authData = append(authData, aaguid...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(keyID)))
	authData = append(authData, keyID...)

	hashed := clientDataHash
	if spoil.NonceFor != nil {
		hashed = spoil.NonceFor
	}
	nonce := sha256.Sum256(append(append([]byte{}, authData...), hashed...))
	extension, err := asn1.Marshal(struct {
		Nonce []byte `asn1:"tag:1,explicit"`
	}{nonce[:]})
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: device.KeyID},
		NotBefore: a.now.Add(-time.Hour), NotAfter: a.now.Add(24 * time.Hour),
		ExtraExtensions: []pkix.Extension{{Id: nonceExtension, Value: extension}},
	}
	credential, err := x509.CreateCertificate(rand.Reader, template, a.intermediate, &device.Key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return CBOR(map[string]any{
		"fmt":      "apple-appattest",
		"authData": authData,
		"attStmt": map[string]any{
			"x5c":     []any{credential, a.intermediate.Raw},
			"receipt": []byte("receipt"),
		},
	})
}

// Assert makes what DCAppAttestService.generateAssertion returns.
func (d Device) Assert(t testing.TB, clientDataHash []byte, appID string, counter uint32) []byte {
	t.Helper()
	rpID := sha256.Sum256([]byte(appID))
	authData := append([]byte{}, rpID[:]...)
	authData = append(authData, 0x01)
	authData = binary.BigEndian.AppendUint32(authData, counter)
	nonce := sha256.Sum256(append(append([]byte{}, authData...), clientDataHash...))
	digest := sha256.Sum256(nonce[:])
	signature, err := ecdsa.SignASN1(rand.Reader, d.Key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return CBOR(map[string]any{"signature": signature, "authenticatorData": authData})
}

// SignIn stands in for Sign in with Apple: it serves its keys and signs
// identity tokens.
type SignIn struct {
	KeysURL string
	key     *rsa.PrivateKey
}

// NewSignIn starts a key server that lives as long as the test.
func NewSignIn(t testing.TB) *SignIn {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "apple-1", "alg": "RS256", "use": "sig",
			"n": b64.EncodeToString(key.N.Bytes()),
			"e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(server.Close)
	return &SignIn{KeysURL: server.URL, key: key}
}

// IdentityToken signs claims as Apple would.
func (s *SignIn) IdentityToken(t testing.TB, claims map[string]any) string {
	t.Helper()
	b64 := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "apple-1"})
	payload, _ := json.Marshal(claims)
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64.EncodeToString(signature)
}
