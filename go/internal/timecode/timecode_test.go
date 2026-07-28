// The claims worth testing here are the refusals.
//
// Producing a timestamp is not interesting and cannot really fail. What can
// fail — silently, and in a way that only surfaces months later in an appeal —
// is a comparison that returns an answer it has no basis for, or a host that
// keeps reporting good time after its time source died.

package timecode

import (
	"errors"
	"testing"
	"time"
)

var epoch = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func TestOverlappingTimecodesAreNotOrdered(t *testing.T) {
	// 20ms apart, 50ms of slop each way. The second may well have happened
	// first, and saying otherwise is the bug this type exists to prevent.
	early := New(epoch, 50*time.Millisecond, Synchronised)
	late := New(epoch.Add(20*time.Millisecond), 50*time.Millisecond, Synchronised)

	if early.DefinitelyPrecedes(late) {
		t.Error("ordered two readings whose intervals overlap")
	}
	if late.DefinitelyPrecedes(early) {
		t.Error("ordered two readings whose intervals overlap, the other way")
	}
	if !early.MayCoincide(late) {
		t.Error("overlapping readings should be able to coincide")
	}
}

func TestDisjointTimecodesAreOrdered(t *testing.T) {
	early := New(epoch, 5*time.Millisecond, Synchronised)
	late := New(epoch.Add(20*time.Millisecond), 5*time.Millisecond, Synchronised)

	if !early.DefinitelyPrecedes(late) {
		t.Error("failed to order readings that do not overlap")
	}
	if late.DefinitelyPrecedes(early) {
		t.Error("ordered readings backwards")
	}
	if early.MayCoincide(late) {
		t.Error("disjoint readings cannot coincide")
	}
}

func TestUnsynchronisedNeverOrders(t *testing.T) {
	// Far apart, so the intervals plainly do not overlap. It still must not
	// order them: one machine's unchecked opinion cannot be placed against
	// another's however different the numbers look.
	adrift := New(epoch, time.Millisecond, Unsynchronised)
	good := New(epoch.Add(time.Hour), time.Millisecond, Synchronised)

	if adrift.DefinitelyPrecedes(good) {
		t.Error("ordered an unsynchronised reading against a good one")
	}
	if good.DefinitelyPrecedes(adrift) {
		t.Error("ordered a good reading against an unsynchronised one")
	}
	if !adrift.MayCoincide(good) {
		t.Error("nothing rules out coincidence when one side is unchecked")
	}
}

func TestNegativeUncertaintyIsRefused(t *testing.T) {
	// A negative width would invert every comparison above, turning "cannot
	// tell" into a confident wrong answer.
	code := New(epoch, -time.Second, Synchronised)
	if code.Uncertainty != 0 {
		t.Fatalf("kept a negative uncertainty: %v", code.Uncertainty)
	}
	if code.Widened(-time.Second).Uncertainty != 0 {
		t.Error("widening by a negative amount narrowed the interval")
	}
}

func TestWideningOnlyGrows(t *testing.T) {
	code := New(epoch, 10*time.Millisecond, Synchronised)
	wider := code.Widened(5 * time.Millisecond)

	if wider.Uncertainty != 15*time.Millisecond {
		t.Fatalf("uncertainty = %v, want 15ms", wider.Uncertainty)
	}
	if !wider.At.Equal(code.At) {
		t.Error("widening moved the estimate")
	}
}

func TestUncheckedIsWideAndUntrusted(t *testing.T) {
	code := Unchecked(epoch)
	if code.IsTrusted() {
		t.Error("an unchecked clock claimed to be trustworthy")
	}
	// An undisciplined clock can be wrong by years; a plausible-looking bound
	// would be the lie the type exists to stop.
	if code.Uncertainty < time.Hour {
		t.Errorf("uncertainty = %v, want something honestly enormous", code.Uncertainty)
	}
}

func TestZeroValueIsUnsynchronised(t *testing.T) {
	// A struct somebody forgot to fill in must read as "we do not know".
	var code Timecode
	if code.IsTrusted() {
		t.Error("the zero value claimed to be trustworthy")
	}
}

func TestTrustNamesMatchTheSwiftSDK(t *testing.T) {
	// These strings cross the wire into MoqomCore.TimeTrust. Renaming one here
	// without renaming it there breaks interop quietly.
	for trust, want := range map[Trust]string{
		Unsynchronised: "unsynchronised",
		Synchronised:   "synchronised",
		Authoritative:  "authoritative",
	} {
		if got := trust.String(); got != want {
			t.Errorf("Trust(%d).String() = %q, want %q", trust, got, want)
		}
	}
}

// MARK: - The host clock

// host builds a Host over a scripted kernel.
func host(health Health, err error) (*Host, *int) {
	calls := 0
	return &Host{
		Ceiling: DefaultCeiling,
		now:     func() time.Time { return epoch },
		read: func() (Health, error) {
			calls++
			return health, err
		},
		expiry: pollInterval,
	}, &calls
}

func TestHostReportsTheKernelsOwnBound(t *testing.T) {
	clock, _ := host(Health{Synchronised: true, MaxError: 3 * time.Millisecond}, nil)

	code := clock.Now()
	if code.Trust != Synchronised {
		t.Fatalf("trust = %v, want synchronised", code.Trust)
	}
	if code.Uncertainty != 3*time.Millisecond {
		t.Errorf("uncertainty = %v, want the kernel's 3ms", code.Uncertainty)
	}
}

func TestHostWithNoTimeSourceSaysSo(t *testing.T) {
	// chrony died, or the box has just booted. This is precisely when a naive
	// server hands out confident nanoseconds.
	clock, _ := host(Health{Synchronised: false}, nil)

	if clock.Now().IsTrusted() {
		t.Error("claimed good time with no time source")
	}
}

func TestHostBeyondTheCeilingIsNotSynchronised(t *testing.T) {
	// The daemon is alive but the bound has grown past what is usable. Still
	// worth recording, still not worth ordering by.
	clock, _ := host(Health{Synchronised: true, MaxError: time.Second}, nil)

	code := clock.Now()
	if code.Trust != Unsynchronised {
		t.Errorf("trust = %v, want unsynchronised past the ceiling", code.Trust)
	}
	if code.Uncertainty != time.Second {
		t.Errorf("uncertainty = %v, want the real bound preserved", code.Uncertainty)
	}
}

func TestHostWidensForAPendingLeapSecond(t *testing.T) {
	// Cloudflare does not smear, so UTC really does repeat or skip a second.
	clock, _ := host(Health{
		Synchronised: true,
		MaxError:     2 * time.Millisecond,
		LeapPending:  true,
	}, nil)

	code := clock.Now()
	if code.Uncertainty < time.Second {
		t.Errorf("uncertainty = %v, want at least a second across a leap", code.Uncertainty)
	}
	if code.Trust != Unsynchronised {
		t.Error("a second of ambiguity is past the ceiling and should say so")
	}
}

func TestHostSurvivesAFailingSyscall(t *testing.T) {
	clock, _ := host(Health{}, errors.New("adjtimex unavailable"))

	code := clock.Now()
	if code.IsTrusted() {
		t.Error("a failed health read must not produce trusted time")
	}
	if code.At.IsZero() {
		t.Error("the instant is still worth having even when its quality is not")
	}
}

func TestHostCachesTheHealthReading(t *testing.T) {
	// A busy control plane stamps many events per second and the kernel's
	// answer changes on the order of seconds.
	clock, calls := host(Health{Synchronised: true, MaxError: time.Millisecond}, nil)

	for range 100 {
		clock.Now()
	}
	if *calls != 1 {
		t.Errorf("read the kernel %d times for 100 stamps, want 1", *calls)
	}
}

func TestHostRereadsOnceTheCacheExpires(t *testing.T) {
	calls := 0
	at := epoch
	clock := &Host{
		Ceiling: DefaultCeiling,
		now:     func() time.Time { return at },
		read: func() (Health, error) {
			calls++
			return Health{Synchronised: true, MaxError: time.Millisecond}, nil
		},
		expiry: pollInterval,
	}

	clock.Now()
	at = at.Add(2 * pollInterval)
	clock.Now()

	if calls != 2 {
		t.Errorf("read the kernel %d times, want a fresh read after the interval", calls)
	}
}

func TestFromFuncLaundersNothing(t *testing.T) {
	// The bridge for call sites holding a bare wall clock. It must not make an
	// unchecked clock look like a checked one.
	clock := FromFunc(func() time.Time { return epoch })

	code := clock.Now()
	if code.IsTrusted() {
		t.Error("a bare wall clock was promoted to trusted time")
	}
	if !code.At.Equal(epoch) {
		t.Errorf("At = %v, want the wrapped clock's reading", code.At)
	}
}
