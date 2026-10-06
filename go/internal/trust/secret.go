// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// Secrets makes the detector a moving target.
//
// A threshold that is the same everywhere and forever can be found by probing
// once and then sat just under. So every threshold is perturbed by a keyed,
// per-tenant, per-epoch amount, and each epoch the anomaly model drops a keyed
// subset of its features. An attacker who has mapped the boundary in one tenant
// on Monday has mapped nothing about another tenant, or about Tuesday. The key
// lives in Secret Manager and is never in a binary that ships.
type Secrets struct {
	key []byte
	// Epoch is how long one draw lasts. Default one day.
	Epoch time.Duration
	// Spread is the largest relative perturbation of a threshold. Default 0.12.
	Spread float64
	// Drop is the fraction of anomaly features hidden per epoch. Default 0.2.
	Drop float64
}

// NewSecrets takes the detector key.
func NewSecrets(key []byte) *Secrets {
	return &Secrets{key: append([]byte(nil), key...), Epoch: 24 * time.Hour, Spread: 0.12, Drop: 0.2}
}

func (s *Secrets) unit(tenant, name string, at time.Time) float64 {
	if s == nil || len(s.key) == 0 {
		return 0.5
	}
	epoch := s.Epoch
	if epoch <= 0 {
		epoch = 24 * time.Hour
	}
	mac := hmac.New(sha256.New, s.key)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(at.UnixNano()/int64(epoch)))
	mac.Write(n[:])
	mac.Write([]byte(tenant))
	mac.Write([]byte{0})
	mac.Write([]byte(name))
	return float64(binary.BigEndian.Uint64(mac.Sum(nil))>>11) / float64(1<<53)
}

// Threshold perturbs base for one tenant and epoch, by at most Spread either way.
func (s *Secrets) Threshold(tenant, name string, base float64, at time.Time) float64 {
	if s == nil {
		return base
	}
	return base * (1 + s.Spread*(2*s.unit(tenant, name, at)-1))
}

// Mask is which anomaly features are in use this epoch. Features named in keep
// are never dropped: they are the ones whose absence would cost more recall
// than the unpredictability buys.
func (s *Secrets) Mask(tenant string, at time.Time, keep map[int]bool) []bool {
	mask := make([]bool, len(FeatureNames))
	for i := range mask {
		mask[i] = keep[i] || s == nil || s.unit(tenant, "mask:"+FeatureNames[i], at) >= s.Drop
	}
	return mask
}

// Uniform is a keyed draw in [0, 1), for delays and sampling an attacker should
// not be able to predict.
func (s *Secrets) Uniform(tenant, name string, at time.Time) float64 { return s.unit(tenant, name, at) }
