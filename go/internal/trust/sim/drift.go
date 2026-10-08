// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see ../doc.go.

package sim

import (
	"fmt"
	"math/rand/v2"

	"github.com/moqom-cloud/moqom/go/internal/trust"
)

// DriftResult is the slow-poisoning study.
type DriftResult struct {
	// AlarmStep is the campaign step at which the drift alarm halted
	// refitting, or -1 if it never did.
	AlarmStep, Steps int
	// Before, Defended and Undefended score the farm's real behaviour against
	// the baseline before the campaign, after it with the alarm honoured, and
	// after it with every alarm overridden.
	Before, Defended, Undefended DriftScore
}

// DriftScore is how anomalous one vector looks.
type DriftScore struct {
	Isolation, Z float64
}

// driftFeatures are the continuous features the simulated farm wants to make
// normal: switch rate, track count, RTT jitter and presence rate.
var driftFeatures = []int{3, 5, 9, 11}

// Drift simulates a farm that slips gradually shifting vectors into the
// baseline -- each step close enough to the last to pass admission -- to teach
// the anomaly model that its real behaviour is normal.
func Drift(seed uint64) DriftResult {
	rng := rand.New(rand.NewPCG(seed, seed+11))
	dims := len(trust.FeatureNames)
	human := func() []float64 {
		v := make([]float64, dims)
		for i := range v {
			v[i] = 1 + 0.15*rng.NormFloat64()
		}
		return v
	}
	target := make([]float64, dims)
	for i := range target {
		target[i] = 1
	}
	for _, i := range driftFeatures {
		target[i] = 0
	}
	const steps = 40
	shifted := func(step int) []float64 {
		v := human()
		f := float64(step) / steps
		for _, i := range driftFeatures {
			v[i] = (1-f)*v[i] + f*target[i]
		}
		return v
	}
	score := func(b *trust.Baseline) DriftScore {
		iso, z, _, _ := b.Score(target)
		return DriftScore{Isolation: iso, Z: z}
	}
	run := func(defended bool) (DriftScore, int, DriftScore) {
		b := trust.NewBaseline(seed)
		b.Capacity, b.RefitEvery = 1024, 128
		n := 0
		for range 1500 {
			n++
			b.Admit(human(), trust.Pseudonym(fmt.Sprint(n)), 0, 1, nil)
		}
		before := score(b)
		alarm := -1
		for step := 1; step <= steps; step++ {
			for range 120 {
				n++
				v := human()
				if rng.Float64() < 0.5 {
					v = shifted(step)
				}
				b.Admit(v, trust.Pseudonym(fmt.Sprint(n)), 0, 1, nil)
			}
			if _, halted := b.Drift(); halted {
				if alarm < 0 {
					alarm = step
				}
				if !defended {
					b.AcceptDrift()
				}
			}
		}
		return before, alarm, score(b)
	}
	before, alarm, defended := run(true)
	_, _, undefended := run(false)
	return DriftResult{AlarmStep: alarm, Steps: steps, Before: before, Defended: defended, Undefended: undefended}
}

// DriftTable renders Drift.
func DriftTable(r DriftResult) string {
	return fmt.Sprintf("| Farm's real behaviour scored against the baseline | Isolation score | Largest robust z |\n|---|---|---|\n"+
		"| Before the campaign | %.3f | %.1f |\n| After %d steps, drift alarm honoured | %.3f | %.1f |\n| After %d steps, every alarm overridden | %.3f | %.1f |\n\nAlarm first raised at step %d of %d.\n",
		r.Before.Isolation, r.Before.Z, r.Steps, r.Defended.Isolation, r.Defended.Z, r.Steps, r.Undefended.Isolation, r.Undefended.Z, r.AlarmStep, r.Steps)
}
