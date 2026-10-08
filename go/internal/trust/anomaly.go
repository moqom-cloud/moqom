// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"math"
	"math/rand/v2"
	"sync"
)

// Baseline is one tenant's picture of normal, and the anomaly model fitted to it.
//
// Two models over the same sample: robust z-scores (median and MAD per
// feature), which say *which* feature is odd and so produce a reason a
// moderator can read, and an isolation forest, which catches combinations no
// single feature flags. Both are refitted from a reservoir rather than updated
// per event, so one burst of traffic cannot move them.
//
// # Poisoning
//
// An unsupervised model learns whatever it is shown, so a farm that is present
// long enough becomes normal. Three things stop that:
//
//   - Admission: only profiles the detector already scores as low-risk enter
//     the reservoir, so known-bad traffic never trains the baseline.
//   - Diversity cap: no network prefix may hold more than a small share of the
//     reservoir, so a farm behind one block cannot outvote a city.
//   - Drift alarm: each refit is compared with a frozen reference. If the
//     median of any feature has moved by more than DriftLimit MADs, the refit is
//     refused and the alarm is raised for a human, who calls AcceptDrift if the
//     change is real (a new app release, a holiday). Slow drift is the attack
//     this is for: each step is small, but they add up against a reference that
//     does not move with them.
type Baseline struct {
	mu sync.Mutex

	// Capacity of the reservoir. Default 1024.
	Capacity int
	// RefitEvery is how many admissions trigger a refit. Default 256.
	RefitEvery int
	// MinSamples before the baseline scores anything. Default 96.
	MinSamples int
	// GroupShare caps one network prefix's share of the reservoir. Default 0.04.
	GroupShare float64
	// DriftLimit is the largest median shift, in reference MADs, that halts
	// refitting. Default 1.0.
	DriftLimit float64

	rng       *rand.Rand
	reservoir [][]float64
	groups    []Pseudonym
	seen      int
	since     int

	reference *spread
	current   *spread
	forest    *forest
	mask      []bool

	alarm bool
	drift float64
}

type spread struct {
	median []float64
	mad    []float64
	std    []float64
	// tail is, for a reference, the share of its own samples more than
	// three scales from its median, per feature.
	tail []float64
}

// TailExcess is how many percentage points of extra tail mass on one feature,
// relative to the reference, count as one unit of drift.
//
// The median is the wrong thing to watch for poisoning. A campaign that
// contaminates a third of the reservoir barely moves a median, but it fills a
// tail that was nearly empty, and that is where the farm's real behaviour
// lives.
const TailExcess = 0.05

func (s *spread) scale(d int) float64 {
	return math.Max(math.Max(s.mad[d], 0.5*s.std[d]), 1e-3)
}

func tailMass(samples [][]float64, reference *spread) []float64 {
	out := make([]float64, len(reference.median))
	for d := range out {
		scale := reference.scale(d)
		for _, sample := range samples {
			if math.Abs(sample[d]-reference.median[d]) > 3*scale {
				out[d]++
			}
		}
		out[d] /= float64(len(samples))
	}
	return out
}

func excessTail(reference *spread, tail []float64) float64 {
	var worst float64
	for d := range tail {
		if noZ[d] {
			continue
		}
		worst = math.Max(worst, tail[d]-reference.tail[d])
	}
	return worst
}

// NewBaseline seeds the reservoir and forest randomness. The seed should come
// from the detector secret so tree structure is not predictable.
func NewBaseline(seed uint64) *Baseline {
	return &Baseline{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

func (b *Baseline) defaults() {
	if b.Capacity == 0 {
		b.Capacity = 1024
	}
	if b.RefitEvery == 0 {
		b.RefitEvery = 256
	}
	if b.MinSamples == 0 {
		b.MinSamples = 96
	}
	if b.GroupShare == 0 {
		b.GroupShare = 0.04
	}
	if b.DriftLimit == 0 {
		b.DriftLimit = 1.0
	}
}

// Admit offers a profile vector. It is ignored unless risk is below admitBelow
// and the profile's network group has room.
func (b *Baseline) Admit(vector []float64, group Pseudonym, risk, admitBelow float64, mask []bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.defaults()
	if risk >= admitBelow {
		return
	}
	if group != "" {
		limit := max(2, int(b.GroupShare*float64(b.Capacity)))
		count := 0
		for _, g := range b.groups {
			if g == group {
				count++
			}
		}
		if count >= limit {
			return
		}
	}
	sample := append([]float64(nil), vector...)
	b.seen++
	if len(b.reservoir) < b.Capacity {
		b.reservoir = append(b.reservoir, sample)
		b.groups = append(b.groups, group)
	} else if j := b.rng.IntN(b.seen); j < b.Capacity {
		b.reservoir[j], b.groups[j] = sample, group
	} else {
		return
	}
	b.since++
	if (b.forest == nil && len(b.reservoir) >= b.MinSamples) || b.since >= b.RefitEvery {
		b.refit(mask)
	}
}

func (b *Baseline) refit(mask []bool) {
	b.since = 0
	candidate := spreadOf(b.reservoir)
	if b.reference == nil {
		b.reference = candidate
		b.reference.tail = tailMass(b.reservoir, candidate)
	} else {
		b.drift = math.Max(driftBetween(b.reference, candidate),
			excessTail(b.reference, tailMass(b.reservoir, b.reference))/TailExcess)
		if b.drift > b.DriftLimit {
			b.alarm = true
			return
		}
	}
	b.current = candidate
	b.mask = mask
	b.forest = growForest(b.rng, b.reservoir, mask, 64, 128)
}

// AcceptDrift is the human decision that the population really has changed:
// the current reservoir becomes the reference and refitting resumes.
func (b *Baseline) AcceptDrift() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reference = nil
	b.alarm = false
	b.refit(b.mask)
}

// Drift reports the last measured drift and whether refitting is halted.
func (b *Baseline) Drift() (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drift, b.alarm
}

// Ready reports whether the baseline has enough samples to score.
func (b *Baseline) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forest != nil
}

// Score returns the isolation-forest score in (0, 1), where 0.5 is ordinary and
// values towards 1 are isolated quickly, plus the largest robust z-score and the
// feature it came from. ok is false until the baseline is ready.
func (b *Baseline) Score(vector []float64) (isolation, z float64, feature int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.forest == nil {
		return 0.5, 0, -1, false
	}
	isolation = b.forest.score(vector)
	feature = -1
	for i, v := range vector {
		if (b.mask != nil && !b.mask[i]) || noZ[i] {
			continue
		}
		// A feature most people share exactly -- no presence changes in a
		// short visit -- has a MAD of zero, and dividing by it turns the
		// minority who did something into outliers. Half a standard deviation
		// is the floor: still robust to a few extremes, never zero while
		// anybody differs.
		mad := b.current.scale(i)
		if d := math.Abs(v-b.current.median[i]) / mad; d > z {
			z, feature = d, i
		}
	}
	return isolation, z, feature, true
}

// noZ are features a z-score means nothing for. Flags and small counts: fifteen
// per cent of people on software keys is not fifteen per cent of outliers. And
// coefficients of variation, which sit at the "too little data" default of 1
// for most profiles and are bimodal around it.
var noZ = map[int]bool{2: true, 4: true, 6: true, 7: true, 13: true, 15: true, 16: true, 17: true}

// noForest are features the forest does not split on. Flags, because a split
// on a binary feature isolates its minority side in one step, so fifteen per
// cent of honest people would score as anomalous for owning an older phone.
// And refresh_cv, because token refresh runs on the SDK's timer and so is the
// same for every honest client; it stays in the vector for the supervised
// model, which can learn whether a deviation from the SDK's schedule matters.
var noForest = map[int]bool{6: true, 7: true, 15: true, 16: true, 17: true}

func spreadOf(samples [][]float64) *spread {
	dims := len(samples[0])
	s := &spread{median: make([]float64, dims), mad: make([]float64, dims), std: make([]float64, dims)}
	column := make([]float64, len(samples))
	for d := 0; d < dims; d++ {
		for i, sample := range samples {
			column[i] = sample[d]
		}
		s.median[d], s.mad[d] = medianMAD(column)
		var r running
		for _, v := range column {
			r.add(v)
		}
		s.std[d] = r.std()
	}
	return s
}

// driftBetween is the largest median shift of any feature, in reference MADs.
// The largest rather than the mean, because a poisoning campaign moves the few
// features that describe the farm and leaves the rest alone, and a mean over
// all of them would dilute exactly the shift worth seeing.
func driftBetween(reference, candidate *spread) float64 {
	var worst float64
	for d := range reference.median {
		worst = math.Max(worst, math.Abs(candidate.median[d]-reference.median[d])/reference.scale(d))
	}
	return worst
}

// forest is an isolation forest (Liu, Ting and Zhou, 2008).
type forest struct {
	trees  []*node
	sample int
}

type node struct {
	feature     int
	split       float64
	left, right *node
	size        int
}

func growForest(rng *rand.Rand, data [][]float64, mask []bool, trees, sample int) *forest {
	sample = min(sample, len(data))
	features := []int{}
	for i := range data[0] {
		if (mask == nil || mask[i]) && !noForest[i] {
			features = append(features, i)
		}
	}
	limit := int(math.Ceil(math.Log2(float64(sample))))
	f := &forest{sample: sample}
	for range trees {
		subset := make([][]float64, sample)
		for i := range subset {
			subset[i] = data[rng.IntN(len(data))]
		}
		f.trees = append(f.trees, growTree(rng, subset, features, 0, limit))
	}
	return f
}

func growTree(rng *rand.Rand, data [][]float64, features []int, depth, limit int) *node {
	if depth >= limit || len(data) <= 1 {
		return &node{size: len(data)}
	}
	// Try a few features for one that actually varies in this partition.
	for range 4 {
		feature := features[rng.IntN(len(features))]
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, row := range data {
			lo, hi = math.Min(lo, row[feature]), math.Max(hi, row[feature])
		}
		if hi <= lo {
			continue
		}
		split := lo + rng.Float64()*(hi-lo)
		var left, right [][]float64
		for _, row := range data {
			if row[feature] < split {
				left = append(left, row)
			} else {
				right = append(right, row)
			}
		}
		return &node{
			feature: feature, split: split,
			left:  growTree(rng, left, features, depth+1, limit),
			right: growTree(rng, right, features, depth+1, limit),
		}
	}
	return &node{size: len(data)}
}

// averagePath is c(n): the average path length of an unsuccessful search in a
// binary search tree of n nodes, which normalises depth into a score.
func averagePath(n int) float64 {
	if n <= 1 {
		return 0
	}
	if n == 2 {
		return 1
	}
	h := math.Log(float64(n-1)) + 0.5772156649
	return 2*h - 2*float64(n-1)/float64(n)
}

func (f *forest) score(x []float64) float64 {
	var total float64
	for _, tree := range f.trees {
		depth, current := 0.0, tree
		for current.left != nil {
			if x[current.feature] < current.split {
				current = current.left
			} else {
				current = current.right
			}
			depth++
		}
		total += depth + averagePath(current.size)
	}
	mean := total / float64(len(f.trees))
	return math.Pow(2, -mean/averagePath(f.sample))
}
