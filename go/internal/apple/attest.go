package apple

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"
)

// App Attest, checked as Apple's "Validating apps that connect to your server"
// describes: an attestation once, when a key is made, and an assertion with
// every request after.

//go:embed app_attest_root_ca.pem
var appAttestRootPEM []byte

// AppAttestRoots is Apple's App Attestation Root CA, downloaded from
// https://www.apple.com/certificateauthority/Apple_App_Attestation_Root_CA.pem
// (SHA-256 fingerprint 1C:B9:82:3B:…:62:42:C9:32).
func AppAttestRoots() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(appAttestRootPEM) {
		panic("apple: the embedded App Attest root does not parse")
	}
	return pool
}

// ErrAttestation is an attestation or assertion that does not verify.
var ErrAttestation = errors.New("the App Attest evidence is not valid")

// nonceExtension carries SHA-256(authData || clientDataHash) in the
// credential certificate.
var nonceExtension = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

var (
	aaguidProduction  = append([]byte("appattest"), make([]byte, 7)...)
	aaguidDevelopment = []byte("appattestdevelop")
)

// maxEvidenceBytes bounds what is parsed before anything is verified. An
// attestation is a few kilobytes, mostly its certificates and receipt.
const maxEvidenceBytes = 16 << 10

// Attested is a verified App Attest key.
type Attested struct {
	// PublicKey verifies the key's assertions.
	PublicKey *ecdsa.PublicKey
	// AppID is the team ID and bundle ID it was attested for, "TEAMID.bundle".
	AppID string
	// Development is an attestation from Apple's development environment.
	Development bool
}

// VerifyAttestation checks an attestation object for keyID, made over
// clientDataHash, by one of appIDs ("TEAMID.bundle"), chaining to roots at now.
// A development attestation is refused unless allowDevelopment.
func VerifyAttestation(
	object []byte,
	keyID string,
	clientDataHash []byte,
	appIDs []string,
	roots *x509.CertPool,
	now time.Time,
	allowDevelopment bool,
) (Attested, error) {
	if len(object) == 0 || len(object) > maxEvidenceBytes {
		return Attested{}, fmt.Errorf("%w: attestation length", ErrAttestation)
	}
	identifier, err := base64.StdEncoding.DecodeString(keyID)
	if err != nil || len(identifier) != sha256.Size {
		return Attested{}, fmt.Errorf("%w: key ID", ErrAttestation)
	}
	decoded, err := decodeCBOR(object)
	if err != nil {
		return Attested{}, fmt.Errorf("%w: %w", ErrAttestation, err)
	}
	top, ok := decoded.(map[string]any)
	if !ok || top["fmt"] != "apple-appattest" {
		return Attested{}, fmt.Errorf("%w: format", ErrAttestation)
	}
	authData, ok := top["authData"].([]byte)
	if !ok {
		return Attested{}, fmt.Errorf("%w: no authenticator data", ErrAttestation)
	}
	statement, ok := top["attStmt"].(map[string]any)
	if !ok {
		return Attested{}, fmt.Errorf("%w: no statement", ErrAttestation)
	}
	chain, ok := statement["x5c"].([]any)
	if !ok || len(chain) < 2 {
		return Attested{}, fmt.Errorf("%w: certificate chain", ErrAttestation)
	}

	// 1. The credential certificate chains to Apple's root.
	certificates := make([]*x509.Certificate, 0, len(chain))
	for _, entry := range chain {
		der, ok := entry.([]byte)
		if !ok {
			return Attested{}, fmt.Errorf("%w: certificate chain", ErrAttestation)
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return Attested{}, fmt.Errorf("%w: certificate: %w", ErrAttestation, err)
		}
		certificates = append(certificates, certificate)
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediates.AddCert(certificate)
	}
	credential := certificates[0]
	if _, err := credential.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return Attested{}, fmt.Errorf("%w: certificate chain: %w", ErrAttestation, err)
	}

	// 2 to 4. The certificate's nonce is SHA-256(authData || clientDataHash).
	nonce := sha256.Sum256(append(slices.Clone(authData), clientDataHash...))
	var stated []byte
	for _, extension := range credential.Extensions {
		if extension.Id.Equal(nonceExtension) {
			var wrapper struct {
				Nonce []byte `asn1:"tag:1,explicit"`
			}
			if rest, err := asn1.Unmarshal(extension.Value, &wrapper); err != nil || len(rest) != 0 {
				return Attested{}, fmt.Errorf("%w: nonce extension", ErrAttestation)
			}
			stated = wrapper.Nonce
		}
	}
	if !bytes.Equal(stated, nonce[:]) {
		return Attested{}, fmt.Errorf("%w: nonce", ErrAttestation)
	}

	// 5. The key ID is the SHA-256 of the credential's public key.
	public, ok := credential.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return Attested{}, fmt.Errorf("%w: credential key is not ECDSA", ErrAttestation)
	}
	point, err := public.ECDH()
	if err != nil || point.Curve() != ecdh.P256() {
		return Attested{}, fmt.Errorf("%w: credential key is not P-256", ErrAttestation)
	}
	if digest := sha256.Sum256(point.Bytes()); !bytes.Equal(digest[:], identifier) {
		return Attested{}, fmt.Errorf("%w: key ID does not match the key", ErrAttestation)
	}

	// 6 to 9. The authenticator data names the app, a fresh key, Apple's
	// environment and the key ID.
	parsed, err := parseAuthenticatorData(authData, true)
	if err != nil {
		return Attested{}, err
	}
	appID, ok := matchApp(parsed.rpIDHash, appIDs)
	if !ok {
		return Attested{}, fmt.Errorf("%w: attested for another app", ErrAttestation)
	}
	if parsed.counter != 0 {
		return Attested{}, fmt.Errorf("%w: counter is not zero", ErrAttestation)
	}
	development := bytes.Equal(parsed.aaguid, aaguidDevelopment)
	switch {
	case bytes.Equal(parsed.aaguid, aaguidProduction):
	case development && allowDevelopment:
	default:
		return Attested{}, fmt.Errorf("%w: environment", ErrAttestation)
	}
	if !bytes.Equal(parsed.credentialID, identifier) {
		return Attested{}, fmt.Errorf("%w: credential ID", ErrAttestation)
	}
	return Attested{PublicKey: public, AppID: appID, Development: development}, nil
}

// VerifyAssertion checks an assertion over clientDataHash made by an attested
// key for appID, and returns its counter, which must exceed previous.
func VerifyAssertion(
	object []byte,
	clientDataHash []byte,
	public *ecdsa.PublicKey,
	appID string,
	previous uint32,
) (uint32, error) {
	if len(object) == 0 || len(object) > maxEvidenceBytes {
		return 0, fmt.Errorf("%w: assertion length", ErrAttestation)
	}
	decoded, err := decodeCBOR(object)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrAttestation, err)
	}
	top, ok := decoded.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("%w: assertion", ErrAttestation)
	}
	signature, okSignature := top["signature"].([]byte)
	authData, okData := top["authenticatorData"].([]byte)
	if !okSignature || !okData {
		return 0, fmt.Errorf("%w: assertion", ErrAttestation)
	}
	nonce := sha256.Sum256(append(slices.Clone(authData), clientDataHash...))
	digest := sha256.Sum256(nonce[:])
	if !ecdsa.VerifyASN1(public, digest[:], signature) {
		return 0, fmt.Errorf("%w: signature", ErrAttestation)
	}
	parsed, err := parseAuthenticatorData(authData, false)
	if err != nil {
		return 0, err
	}
	if _, ok := matchApp(parsed.rpIDHash, []string{appID}); !ok {
		return 0, fmt.Errorf("%w: asserted for another app", ErrAttestation)
	}
	if parsed.counter <= previous {
		return 0, fmt.Errorf("%w: counter did not increase", ErrAttestation)
	}
	return parsed.counter, nil
}

type authenticatorData struct {
	rpIDHash     []byte
	counter      uint32
	aaguid       []byte
	credentialID []byte
}

// parseAuthenticatorData reads WebAuthn authenticator data: the RP ID hash,
// flags, the counter and, in an attestation, the attested credential data.
func parseAuthenticatorData(data []byte, attested bool) (authenticatorData, error) {
	const header = 32 + 1 + 4
	if len(data) < header {
		return authenticatorData{}, fmt.Errorf("%w: authenticator data", ErrAttestation)
	}
	parsed := authenticatorData{
		rpIDHash: data[:32],
		counter:  binary.BigEndian.Uint32(data[33:37]),
	}
	if !attested {
		return parsed, nil
	}
	rest := data[header:]
	if len(rest) < 16+2 {
		return authenticatorData{}, fmt.Errorf("%w: attested credential data", ErrAttestation)
	}
	parsed.aaguid = rest[:16]
	length := int(binary.BigEndian.Uint16(rest[16:18]))
	if len(rest) < 18+length {
		return authenticatorData{}, fmt.Errorf("%w: credential ID", ErrAttestation)
	}
	parsed.credentialID = rest[18 : 18+length]
	return parsed, nil
}

func matchApp(rpIDHash []byte, appIDs []string) (string, bool) {
	for _, appID := range appIDs {
		digest := sha256.Sum256([]byte(appID))
		if bytes.Equal(digest[:], rpIDHash) {
			return appID, true
		}
	}
	return "", false
}
