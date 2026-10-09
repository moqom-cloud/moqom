// Package pqtls names the TLS key exchanges MOQOM's own listeners and clients
// accept: ML-KEM-1024, and nothing classical.
//
// Set explicitly rather than left to Go's defaults, which offer classical
// curves, prefer ML-KEM-768, and depend on the main module's go version.
package pqtls

import "crypto/tls"

// LinkCurves is for links where both ends are MOQOM's: relays, the admin API,
// the SDK. ML-KEM-1024 alone, or hybrid with P-384 for a Go peer; rustls
// offers the pure form.
var LinkCurves = []tls.CurveID{tls.SecP384r1MLKEM1024, tls.MLKEM1024}

// PublicCurves is for the listener Apple devices reach. Apple's TLS stack
// chooses its own key exchange, X25519MLKEM768, and offers an app no way to
// ask for another, so it is accepted here and nowhere else. What it protects
// is a sealed token and metadata; room media and keys are end-to-end encrypted
// under ML-KEM-1024 inside it.
var PublicCurves = []tls.CurveID{tls.SecP384r1MLKEM1024, tls.MLKEM1024, tls.X25519MLKEM768}

// Link returns a copy of config limited to LinkCurves and TLS 1.3.
func Link(config *tls.Config) *tls.Config {
	config = config.Clone()
	config.MinVersion = tls.VersionTLS13
	config.CurvePreferences = LinkCurves
	return config
}

// Public returns a copy of config limited to PublicCurves and TLS 1.3.
func Public(config *tls.Config) *tls.Config {
	config = config.Clone()
	config.MinVersion = tls.VersionTLS13
	config.CurvePreferences = PublicCurves
	return config
}
