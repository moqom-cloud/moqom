package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var errEnrollment = errors.New("the enrollment is not valid")

const (
	enrollmentVersion = 1
	enrollmentAAD     = "moqom-sample-tokens enrollment v1"
)

// enrollment is what a device holds after signing in: a promise from this
// server that the person Apple named may have tokens bound to one key.
//
// Sealed rather than stored, so the server keeps no session table. It is
// worthless away from the device that holds the key it names, because every
// token minted against it is bound to that key and the relay demands a proof
// from it.
type enrollment struct {
	Subject    string `json:"sub"`
	Thumbprint string `json:"jkt"`
	Expires    int64  `json:"exp"`
}

type enrollmentSealer struct {
	aead cipher.AEAD
}

func newEnrollmentSealer(secret []byte) (*enrollmentSealer, error) {
	if len(secret) != 32 {
		return nil, fmt.Errorf("enrollment key is %d bytes, want 32", len(secret))
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &enrollmentSealer{aead: aead}, nil
}

func (s *enrollmentSealer) seal(e enrollment) (string, error) {
	plaintext, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	out := make([]byte, 1+s.aead.NonceSize(), 1+s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	out[0] = enrollmentVersion
	if _, err := rand.Read(out[1:]); err != nil {
		return "", err
	}
	out = s.aead.Seal(out, out[1:], plaintext, []byte(enrollmentAAD))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (s *enrollmentSealer) open(sealed string, now time.Time) (enrollment, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(raw) < 1+s.aead.NonceSize()+s.aead.Overhead() || raw[0] != enrollmentVersion {
		return enrollment{}, fmt.Errorf("%w: malformed", errEnrollment)
	}
	nonce := raw[1 : 1+s.aead.NonceSize()]
	plaintext, err := s.aead.Open(nil, nonce, raw[1+s.aead.NonceSize():], []byte(enrollmentAAD))
	if err != nil {
		return enrollment{}, fmt.Errorf("%w: not sealed by this server", errEnrollment)
	}
	var e enrollment
	if err := json.Unmarshal(plaintext, &e); err != nil || e.Subject == "" || e.Thumbprint == "" {
		return enrollment{}, fmt.Errorf("%w: contents", errEnrollment)
	}
	if now.Unix() >= e.Expires {
		return enrollment{}, fmt.Errorf("%w: expired", errEnrollment)
	}
	return e, nil
}
