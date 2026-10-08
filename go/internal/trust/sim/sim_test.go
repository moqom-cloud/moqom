// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see ../doc.go.

package sim

import (
	"reflect"
	"sync"
	"testing"
)

// Targets. These are what the harness must keep doing as the detector changes;
// they are set below what it currently achieves on synthetic data, and they say
// nothing about real traffic.
const (
	// A real viewer wrongly left out of counts, billing and tips.
	maxHumanExcluded = 0.02
	// A real viewer for whom a kick or ban would be proposed to a reviewer.
	maxHumanKick = 0.005
	// Known-pattern farms.
	minNaive = 0.95
	// Proxy and multi-account phone farms on the final day, once the
	// coordination graph has seen them for a few days.
	minCoordinated = 0.6
)

var (
	cacheMu sync.Mutex
	cache   = map[string]Result{}
)

func run(t *testing.T, name string, cfg Config) Result {
	t.Helper()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if r, ok := cache[name]; ok {
		return r
	}
	r := Run(cfg)
	cache[name] = r
	return r
}

func baseline(t *testing.T, seed uint64) Result {
	return run(t, "baseline-"+string(rune('0'+seed)), DefaultConfig(seed))
}

func poisoned(t *testing.T, defended bool) Result {
	cfg := DefaultConfig(1)
	cfg.Poison, cfg.Defended = true, defended
	name := "poisoned-undefended"
	if defended {
		name = "poisoned-defended"
	}
	return run(t, name, cfg)
}

func TestHumansStayUnderFalsePositiveTargets(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		for _, round := range baseline(t, seed).Rounds {
			h := round.Stats[Human]
			excluded, kick := h.Rate(h.Excluded), h.Rate(h.Kick)
			t.Logf("seed %d day %d: humans excluded %.2f%%, kick proposed %.2f%%, reasons %v",
				seed, round.Index+1, 100*excluded, 100*kick, round.HumanFalsePositiveReasons)
			if excluded > maxHumanExcluded {
				t.Errorf("seed %d day %d: %.2f%% of humans excluded, target %.1f%%", seed, round.Index+1, 100*excluded, 100*maxHumanExcluded)
			}
			if kick > maxHumanKick {
				t.Errorf("seed %d day %d: kick proposed for %.2f%% of humans, target %.1f%%", seed, round.Index+1, 100*kick, 100*maxHumanKick)
			}
		}
	}
}

func TestKnownPatternFarmsAreCaught(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		res := baseline(t, seed)
		for _, round := range res.Rounds {
			if s := round.Stats[NaiveFarm]; s.Rate(s.Excluded) < minNaive {
				t.Errorf("seed %d day %d: naive farm recall %.1f%%", seed, round.Index+1, 100*s.Rate(s.Excluded))
			}
		}
		last := res.Rounds[len(res.Rounds)-1]
		for _, k := range []Kind{ProxyFarm, PhoneFarm} {
			s := last.Stats[k]
			t.Logf("seed %d final day: %s recall %.1f%%", seed, k, 100*s.Rate(s.Excluded))
			if s.Rate(s.Excluded) < minCoordinated {
				t.Errorf("seed %d: %s final-day recall %.1f%%, target %.0f%%", seed, k, 100*s.Rate(s.Excluded), 100*minCoordinated)
			}
		}
	}
}

// An adaptive farm that only changes cheap things -- avoiding the canary,
// spoofing a fingerprint, jittering its timing -- keeps being caught, so it is
// forced up the cost ladder. Recall against its most expensive level is a
// documented limit, not a target.
func TestAdaptiveFarmIsForcedToEscalate(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		res := baseline(t, seed)
		if level := res.Rounds[2].MimicLevel; level < 2 {
			t.Errorf("seed %d: adaptive farm at level %d by day 3; cheap evasion should not have worked", seed, level)
		}
		for _, round := range res.Rounds {
			s := round.Stats[AdaptiveMimic]
			t.Logf("seed %d day %d: adaptive mimic level %d recall %.1f%%", seed, round.Index+1, round.MimicLevel, 100*s.Rate(s.Excluded))
		}
	}
}

func coordinatedRecall(r Result) float64 {
	last := r.Rounds[len(r.Rounds)-1]
	p, f := last.Stats[ProxyFarm], last.Stats[PhoneFarm]
	return (p.Rate(p.Excluded) + f.Rate(f.Excluded)) / 2
}

func TestPoisoningDefencesHold(t *testing.T) {
	clean := coordinatedRecall(baseline(t, 1))
	defended := coordinatedRecall(poisoned(t, true))
	undefended := coordinatedRecall(poisoned(t, false))
	t.Logf("final-day coordinated recall: clean %.1f%%, poisoned+defended %.1f%%, poisoned+undefended %.1f%%",
		100*clean, 100*defended, 100*undefended)
	if defended < clean-0.15 {
		t.Errorf("defended learner lost %.1f points to a compromised moderator", 100*(clean-defended))
	}
	if undefended > defended-0.3 {
		t.Errorf("the defences are not doing anything: undefended %.1f%% vs defended %.1f%%", 100*undefended, 100*defended)
	}
	for _, round := range poisoned(t, true).Rounds {
		h := round.Stats[Human]
		if h.Rate(h.Excluded) > maxHumanExcluded {
			t.Errorf("poisoned day %d: %.2f%% of humans excluded", round.Index+1, 100*h.Rate(h.Excluded))
		}
	}
}

func TestReplayedPublishersAreFlaggedAndCamerasAreNot(t *testing.T) {
	for _, r := range EvaluatePublishers(3, 100) {
		rate := float64(r.Detected) / float64(r.Streams)
		t.Logf("%s: %.1f%%", r.Name, 100*rate)
		switch r.Name {
		case "live camera":
			if rate > 0 {
				t.Errorf("live camera flagged %.1f%%", 100*rate)
			}
		case "static slide (live)":
			if rate > 0.02 {
				t.Errorf("static slide flagged %.1f%%", 100*rate)
			}
		case "file replay, 90 s loop, no re-encode", "short loop (4 s), re-encoded":
			if rate < 0.99 {
				t.Errorf("%s flagged only %.1f%%", r.Name, 100*rate)
			}
		}
	}
}

func TestKeyedThresholdsDefeatBoundaryProbing(t *testing.T) {
	r := Probe(2, 30, 20)
	if r.CaughtStatic != 0 {
		t.Errorf("a fixed threshold caught a prober sitting under it: %v", r.CaughtStatic)
	}
	if r.CaughtKeyed < 0.3 {
		t.Errorf("keyed thresholds caught the prober in only %.1f%% of epochs", 100*r.CaughtKeyed)
	}
}

func TestDriftAlarmProtectsTheBaseline(t *testing.T) {
	r := Drift(1)
	if r.AlarmStep < 0 {
		t.Fatal("a slow poisoning campaign never raised the drift alarm")
	}
	if r.Defended.Z < r.Undefended.Z+1 {
		t.Errorf("honouring the alarm kept z %.1f against %.1f overridden; it should keep much more", r.Defended.Z, r.Undefended.Z)
	}
}

func TestDeterministic(t *testing.T) {
	cfg := DefaultConfig(9)
	cfg.Humans, cfg.Rounds = 600, 2
	a, b := Run(cfg), Run(cfg)
	if !reflect.DeepEqual(a.Rounds, b.Rounds) {
		t.Fatal("the same seed produced different results; the documented numbers would not be reproducible")
	}
}
