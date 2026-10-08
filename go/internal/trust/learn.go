// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Label is one moderator decision, turned into a training example.
type Label struct {
	Vector []float64
	// Offset is the rules-and-anomaly log-odds at the time of the label. The
	// model is trained to correct it, not to replace it: the rules already
	// count a software key or a hosting network, and a model fitted from
	// scratch on the same features would count them again.
	Offset  float64
	Bot     bool
	Labeler string
	At      time.Time
}

// Learner is a per-tenant logistic model trained on moderator decisions.
//
// Logistic rather than boosted trees for now: it trains in microseconds with no
// dependency, its weights are inspectable when an auditor asks why, and its
// probabilities are close to calibrated before calibration. A gradient-boosted
// model is the obvious upgrade once there are enough real labels to justify it.
//
// # Poisoning
//
// The labels are the attack surface. A tenant's moderator account can be
// compromised, a tenant can be the attacker, and a farm can file appeals.
//
//   - Labelers are weighted. MOQOM's own reviewers and a tenant's designated
//     trusted moderators count fully; everyone else counts at UntrustedWeight.
//   - Each labeler is capped per day, so volume cannot buy influence.
//   - A labeler whose labels keep contradicting what the model is confident of
//     loses weight (reputation), which catches a moderator flipping labels.
//   - A sample of every untrusted labeler's decisions is re-labelled blind by
//     a trusted reviewer (Audit), and their weight follows their agreement.
//     Both penalties apply retroactively to every label the labeler gave.
//   - Every retrain is scored on a holdout drawn only from trusted labelers and
//     never trained on. A retrain that makes the holdout worse, pooled or
//     for any one trusted labeler, is refused and counted, and a refused
//     retrain is the alarm.
type Learner struct {
	mu sync.Mutex

	// Trusted labelers and their weight in (0, 1].
	Trusted map[string]float64
	// UntrustedWeight applies to any labeler not in Trusted. Default 0.1.
	UntrustedWeight float64
	// DailyCap is labels per labeler per day. Default 200.
	DailyCap int
	// HoldoutShare of trusted labels go to the holdout. Default 0.2.
	HoldoutShare float64
	// Tolerance is how much worse, in holdout log loss, a retrain may be and
	// still be accepted. Default 0.01.
	Tolerance float64
	// Undefended disables reputation and audit weighting. It exists so the
	// adversarial harness can show what the defences are worth; nothing in
	// production sets it.
	Undefended bool

	rng     *rand.Rand
	train   []weighted
	holdout []Label
	daily   map[string]int
	day     int64
	rep     map[string]*reputation

	weights  []float64
	bias     float64
	mean     []float64
	scale    []float64
	trained  bool
	version  int
	rejected int
}

type weighted struct {
	Label
	weight float64
}

type reputation struct {
	judged, contradicted int
	audited, agreed      int
}

// factor is what a labeler's labels are currently worth, applied at training
// time so that a labeler found out today loses the influence of every label
// they ever gave, not just tomorrow's.
func (l *Learner) factor(labeler string) float64 {
	rep := l.rep[labeler]
	if rep == nil || l.Undefended {
		return 1
	}
	f := 1.0
	if rep.judged >= 20 && float64(rep.contradicted) > 0.5*float64(rep.judged) {
		f *= 0.25
	}
	if rep.audited >= 10 {
		// Agreement with a trusted reviewer, rescaled so that coin-flipping
		// is worth nothing and agreeing every time is worth full weight.
		f *= clamp(2*float64(rep.agreed)/float64(rep.audited)-1, 0, 1)
	}
	return f
}

// Audit records whether a trusted reviewer, re-labelling one of labeler's
// decisions blind, agreed with it. It is how an untrusted labeler earns or
// loses weight, and the only defence that does not depend on the model
// already being right.
func (l *Learner) Audit(labeler string, agreed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rep := l.rep[labeler]
	if rep == nil {
		rep = &reputation{}
		l.rep[labeler] = rep
	}
	rep.audited++
	if agreed {
		rep.agreed++
	}
}

// NewLearner seeds the holdout split.
func NewLearner(seed uint64) *Learner {
	return &Learner{
		rng:   rand.New(rand.NewPCG(seed, ^seed)),
		daily: map[string]int{},
		rep:   map[string]*reputation{},
	}
}

func (l *Learner) defaults() {
	if l.UntrustedWeight == 0 {
		l.UntrustedWeight = 0.1
	}
	if l.DailyCap == 0 {
		l.DailyCap = 200
	}
	if l.HoldoutShare == 0 {
		l.HoldoutShare = 0.2
	}
	if l.Tolerance == 0 {
		l.Tolerance = 0.01
	}
}

// Add offers a label. It reports whether it was accepted.
func (l *Learner) Add(label Label) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.defaults()
	if day := label.At.Unix() / 86400; day != l.day {
		l.day, l.daily = day, map[string]int{}
	}
	if l.daily[label.Labeler] >= l.DailyCap {
		return false
	}
	l.daily[label.Labeler]++

	weight, trusted := l.Trusted[label.Labeler]
	if !trusted {
		weight = l.UntrustedWeight
	}
	rep := l.rep[label.Labeler]
	if rep == nil {
		rep = &reputation{}
		l.rep[label.Labeler] = rep
	}
	if l.trained {
		p := sigmoid(label.Offset + l.logitLocked(label.Vector))
		if p > 0.9 || p < 0.1 {
			rep.judged++
			if (p > 0.5) != label.Bot {
				rep.contradicted++
			}
		}
	}
	if trusted && l.rng.Float64() < l.HoldoutShare {
		l.holdout = append(l.holdout, label)
		return true
	}
	l.train = append(l.train, weighted{Label: label, weight: weight})
	return true
}

// Suspect lists labelers whose labels mostly contradict a confident model.
func (l *Learner) Suspect() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for name, rep := range l.rep {
		if rep.judged >= 20 && float64(rep.contradicted) > 0.5*float64(rep.judged) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Retrain fits a candidate model and adopts it only if it does not make the
// trusted holdout worse. It reports whether the candidate was adopted.
func (l *Learner) Retrain() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.defaults()
	if len(l.train) < 20 {
		return false
	}
	dims := len(l.train[0].Vector)
	mean, scale := make([]float64, dims), make([]float64, dims)
	for d := 0; d < dims; d++ {
		var r running
		for _, example := range l.train {
			r.add(example.Vector[d])
		}
		mean[d], scale[d] = r.mean, math.Max(r.std(), 1e-3)
	}
	weights, bias := make([]float64, dims), 0.0
	const rate, l2 = 0.05, 1e-3
	factors := map[string]float64{}
	for _, example := range l.train {
		if _, ok := factors[example.Labeler]; !ok {
			factors[example.Labeler] = l.factor(example.Labeler)
		}
	}
	order := l.rng.Perm(len(l.train))
	for epoch := 0; epoch < 40; epoch++ {
		for _, i := range order {
			example := l.train[i]
			z := example.Offset + bias
			for d, v := range example.Vector {
				z += weights[d] * (v - mean[d]) / scale[d]
			}
			y := 0.0
			if example.Bot {
				y = 1
			}
			g := (sigmoid(z) - y) * example.weight * factors[example.Labeler]
			bias -= rate * g
			for d, v := range example.Vector {
				weights[d] -= rate * (g*(v-mean[d])/scale[d] + l2*weights[d])
			}
		}
	}
	candidate := func(x []float64) float64 {
		z := bias
		for d, v := range x {
			z += weights[d] * (v - mean[d]) / scale[d]
		}
		return z
	}
	if l.trained && len(l.holdout) >= 10 && l.worsens(candidate) {
		l.rejected++
		return false
	}
	l.weights, l.bias, l.mean, l.scale = weights, bias, mean, scale
	l.trained = true
	l.version++
	return true
}

// worsens reports whether candidate is worse than the current model on the
// pooled holdout or on any one trusted labeler's share of it. Judged per
// labeler because a compromised trusted account also feeds the holdout, and by
// volume alone would outvote the reviewers it disagrees with.
func (l *Learner) worsens(candidate func([]float64) float64) bool {
	if lossOf(candidate, l.holdout) > lossOf(l.logitLocked, l.holdout)+l.Tolerance {
		return true
	}
	by := map[string][]Label{}
	for _, label := range l.holdout {
		by[label.Labeler] = append(by[label.Labeler], label)
	}
	for _, labels := range by {
		if len(labels) < 10 {
			continue
		}
		// Small shares are noisy; allow roughly one standard error of log loss.
		slack := l.Tolerance + 1/math.Sqrt(float64(len(labels)))
		if lossOf(candidate, labels) > lossOf(l.logitLocked, labels)+slack {
			return true
		}
	}
	return false
}

func lossOf(f func([]float64) float64, labels []Label) float64 {
	var total float64
	for _, label := range labels {
		p := clamp(sigmoid(label.Offset+f(label.Vector)), 1e-6, 1-1e-6)
		if label.Bot {
			total -= math.Log(p)
		} else {
			total -= math.Log(1 - p)
		}
	}
	return total / float64(len(labels))
}

// Logit is the model's correction, in log-odds, to the rules and anomaly
// evidence for vector; 0 (no opinion) until the first retrain.
func (l *Learner) Logit(vector []float64) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logitLocked(vector)
}

func (l *Learner) logitLocked(vector []float64) float64 {
	if !l.trained {
		return 0
	}
	z := l.bias
	for d, v := range vector {
		z += l.weights[d] * (v - l.mean[d]) / l.scale[d]
	}
	return z
}

// LearnerStatus is the learner's state for the operator.
type LearnerStatus struct {
	Version, Train, Holdout, Rejected int
	Trained                           bool
}

// Status reports training state.
func (l *Learner) Status() LearnerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LearnerStatus{Version: l.version, Train: len(l.train), Holdout: len(l.holdout), Rejected: l.rejected, Trained: l.trained}
}
