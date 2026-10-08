// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see ../doc.go.

package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/trust"
)

// Table renders per-round results averaged over seeds.
func Table(results []Result) string {
	var b strings.Builder
	b.WriteString("| Day | Human FP @exclude | Human FP @kick proposed | Naive farm | Proxy farm | Adaptive mimic (level) | Phone farm | Bot share true / est. | Retrains refused |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	rounds := len(results[0].Rounds)
	for r := range rounds {
		avg := func(f func(Round) float64) float64 {
			var total float64
			for _, res := range results {
				total += f(res.Rounds[r])
			}
			return total / float64(len(results))
		}
		recall := func(k Kind) float64 {
			return avg(func(x Round) float64 { return x.Stats[k].Rate(x.Stats[k].Excluded) })
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s (%.1f) | %s | %s / %s | %.1f |\n",
			r+1,
			pct(avg(func(x Round) float64 { return x.Stats[Human].Rate(x.Stats[Human].Excluded) })),
			pct(avg(func(x Round) float64 { return x.Stats[Human].Rate(x.Stats[Human].Kick) })),
			pct(recall(NaiveFarm)), pct(recall(ProxyFarm)), pct(recall(AdaptiveMimic)),
			avg(func(x Round) float64 { return float64(x.MimicLevel) }),
			pct(recall(PhoneFarm)),
			pct(avg(func(x Round) float64 { return x.TrueBotShare })),
			pct(avg(func(x Round) float64 { return x.EstimatedBotShare })),
			avg(func(x Round) float64 { return float64(x.Learner.Rejected) }),
		)
	}
	return b.String()
}

func pct(x float64) string { return fmt.Sprintf("%.1f%%", 100*x) }

// PublisherResult is replay detection over synthetic object-size streams.
type PublisherResult struct {
	Name     string
	Streams  int
	Detected int
}

// EvaluatePublishers runs n synthetic streams of each kind, ten minutes at 30
// frames a second with a two-second GOP, through ReplayIndex and LoopScore.
func EvaluatePublishers(seed uint64, n int) []PublisherResult {
	rng := rand.New(rand.NewPCG(seed, seed+7))
	camera := func(frames int) []uint32 {
		out := make([]uint32, frames)
		level := math.Log(6000 + 6000*rng.Float64())
		for i := range out {
			level += 0.02 * rng.NormFloat64()
			if rng.Float64() < 0.002 {
				level = math.Log(3000 + 15000*rng.Float64())
			}
			out[i] = uint32(math.Exp(level + 0.15*rng.NormFloat64()))
		}
		return out
	}
	static := func(frames int) []uint32 {
		out := make([]uint32, frames)
		for i := range out {
			out[i] = uint32(180 + rng.IntN(6))
			if rng.Float64() < 0.001 {
				out[i] = uint32(4000 + rng.IntN(2000))
			}
		}
		return out
	}
	loop := func(clip []uint32, frames int, noise float64) []uint32 {
		out := make([]uint32, frames)
		for i := range out {
			s := float64(clip[i%len(clip)])
			if noise > 0 {
				s *= math.Exp(noise * rng.NormFloat64())
			}
			out[i] = uint32(s)
		}
		return out
	}
	// Delta objects only: 59 of every 60 frames.
	const frames = 10 * 60 * 30 * 59 / 60
	kinds := []struct {
		name string
		gen  func() []uint32
	}{
		{"live camera", func() []uint32 { return camera(frames) }},
		{"static slide (live)", func() []uint32 { return static(frames) }},
		{"file replay, 90 s loop, no re-encode", func() []uint32 { return loop(camera(2655), frames, 0) }},
		{"file replay, 90 s loop, re-encoded each pass", func() []uint32 { return loop(camera(2655), frames, 0.03) }},
		{"short loop (4 s), re-encoded", func() []uint32 { return loop(camera(118), frames, 0.03) }},
	}
	var out []PublisherResult
	for _, k := range kinds {
		res := PublisherResult{Name: k.name, Streams: n}
		for range n {
			sizes := k.gen()
			var index trust.ReplayIndex
			for _, s := range sizes {
				index.Add(s)
			}
			score, _ := trust.LoopScore(sizes[len(sizes)-256:])
			if index.Recurrence() >= 0.3 || score >= 0.9 {
				res.Detected++
			}
		}
		out = append(out, res)
	}
	return out
}

// PublisherTable renders EvaluatePublishers.
func PublisherTable(results []PublisherResult) string {
	var b strings.Builder
	b.WriteString("| Stream | Streams | Flagged as replay |\n|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %d | %s |\n", r.Name, r.Streams, pct(float64(r.Detected)/float64(r.Streams)))
	}
	return b.String()
}

// ProbeResult measures what threshold randomisation costs a prober.
type ProbeResult struct {
	Tenants, Epochs int
	// CaughtStatic is the share of later epochs in which an attacker who found
	// the exclusion threshold in epoch 0 and sat just below it is caught, with
	// fixed thresholds. CaughtKeyed is the same with keyed thresholds.
	CaughtStatic, CaughtKeyed float64
	// SafeMargin is how far below the epoch-0 threshold, in log-odds, the
	// attacker must sit to be safe in every later epoch.
	SafeMargin float64
}

// Probe gives the attacker a perfect, immediate oracle -- better than the
// delayed, coarse one it really gets -- and asks whether it helps.
func Probe(seed uint64, tenants, epochs int) ProbeResult {
	secrets := trust.NewSecrets([]byte(fmt.Sprintf("probe-%d", seed)))
	policy := trust.DefaultPolicy()
	policy.Shadow = false
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	excluded := func(tenant string, score float64, at time.Time, s *trust.Secrets) bool {
		a := trust.Assessment{Subject: trust.Subject{Tenant: tenant, Room: "r"}, Score: score,
			Reasons: []trust.Reason{{Code: trust.ReasonSoftwareKey}, {Code: trust.ReasonJoinBurst}}}
		return trust.Decide(policy, a, s, at).Proposed >= trust.ActionExclude
	}
	threshold := func(tenant string, at time.Time, s *trust.Secrets) float64 {
		lo, hi := 0.0, 1.0
		for range 50 {
			mid := (lo + hi) / 2
			if excluded(tenant, mid, at, s) {
				hi = mid
			} else {
				lo = mid
			}
		}
		return hi
	}
	res := ProbeResult{Tenants: tenants, Epochs: epochs}
	var caughtStatic, caughtKeyed, trials int
	for t := range tenants {
		tenant := fmt.Sprintf("tenant-%d", t)
		staticFound := threshold(tenant, start, nil)
		keyedFound := threshold(tenant, start, secrets)
		staticSit := sigmoidf(logitf(staticFound) - 0.05)
		keyedSit := sigmoidf(logitf(keyedFound) - 0.05)
		for e := 1; e <= epochs; e++ {
			at := start.Add(time.Duration(e) * 24 * time.Hour)
			trials++
			if excluded(tenant, staticSit, at, nil) {
				caughtStatic++
			}
			if excluded(tenant, keyedSit, at, secrets) {
				caughtKeyed++
			}
			res.SafeMargin = math.Max(res.SafeMargin, logitf(keyedFound)-logitf(threshold(tenant, at, secrets)))
		}
	}
	res.CaughtStatic = float64(caughtStatic) / float64(trials)
	res.CaughtKeyed = float64(caughtKeyed) / float64(trials)
	return res
}

func sigmoidf(x float64) float64 { return 1 / (1 + math.Exp(-x)) }
func logitf(p float64) float64   { return math.Log(p / (1 - p)) }

// ProbeTable renders Probe.
func ProbeTable(r ProbeResult) string {
	return fmt.Sprintf("| Thresholds | Attacker sitting 0.05 log-odds under the epoch-0 boundary is caught in later epochs |\n|---|---|\n"+
		"| Fixed | %s |\n| Keyed per tenant and epoch | %s |\n\nMargin needed to be safe across %d epochs x %d tenants: %.2f log-odds below the boundary it found.\n",
		pct(r.CaughtStatic), pct(r.CaughtKeyed), r.Epochs, r.Tenants, r.SafeMargin)
}
