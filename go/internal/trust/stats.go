// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"math"
	"sort"
)

// running is Welford's online mean and variance.
type running struct {
	n    int
	mean float64
	m2   float64
}

func (r *running) add(x float64) {
	r.n++
	d := x - r.mean
	r.mean += d / float64(r.n)
	r.m2 += d * (x - r.mean)
}

func (r running) std() float64 {
	if r.n < 2 {
		return 0
	}
	return math.Sqrt(r.m2 / float64(r.n-1))
}

// cv is the coefficient of variation, or -1 with too few samples to say.
// Machines are regular; people are not, and this is the cheapest way to ask.
func (r running) cv() float64 {
	if r.n < 3 || r.mean == 0 {
		return -1
	}
	return r.std() / math.Abs(r.mean)
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

// medianMAD returns the median and the median absolute deviation scaled to
// agree with a standard deviation under normality.
func medianMAD(values []float64) (float64, float64) {
	m := median(values)
	deviations := make([]float64, len(values))
	for i, v := range values {
		deviations[i] = math.Abs(v - m)
	}
	return m, 1.4826 * median(deviations)
}

// LoopScore measures how strongly a sequence of object sizes repeats itself.
//
// A camera produces object sizes that follow the scene and never repeat at a
// fixed lag. A file played in a loop -- the commonest way to fake a live
// streamer -- repeats exactly, at the loop length, and re-encoding through a
// virtual camera blurs that only slightly. This is the normalised
// autocorrelation of the sizes at the best lag, which the relay can compute from
// object headers without decoding anything, so it works under E2EE.
//
// sizes must exclude the first object of each group: keyframes recur at the
// GOP length in every stream, and would make every camera look like a loop.
// The series is differenced first, so a slow change of scene (a trend) does
// not read as self-similarity either; a loop repeats exactly in differences.
//
// Returns the score in [-1, 1] and the lag it was found at; 0, 0 when the
// sample is too short to say. It only sees loops shorter than half the sample,
// which is what ReplayIndex is for.
func LoopScore(sizes []uint32) (float64, int) {
	n := len(sizes) - 1
	if n < 32 {
		return 0, 0
	}
	x := make([]float64, n)
	var mean float64
	for i := range x {
		x[i] = float64(sizes[i+1]) - float64(sizes[i])
		mean += x[i]
	}
	mean /= float64(n)
	var variance float64
	for i := range x {
		x[i] -= mean
		variance += x[i] * x[i]
	}
	if variance == 0 {
		return 1, 1
	}
	best, bestLag := -1.0, 0
	// A minimum lag excludes the GOP-structure correlation every encoder has at
	// short lags; a loop shorter than this is not a convincing stream anyway.
	for lag := 8; lag <= n/2; lag++ {
		var sum, a2, b2 float64
		for i := 0; i+lag < n; i++ {
			sum += x[i] * x[i+lag]
			a2 += x[i] * x[i]
			b2 += x[i+lag] * x[i+lag]
		}
		if a2 == 0 || b2 == 0 {
			continue
		}
		if r := sum / math.Sqrt(a2*b2); r > best {
			best, bestLag = r, lag
		}
	}
	return best, bestLag
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

func logit(p float64) float64 {
	p = math.Min(math.Max(p, 1e-6), 1-1e-6)
	return math.Log(p / (1 - p))
}

func clamp(x, lo, hi float64) float64 { return math.Min(math.Max(x, lo), hi) }
