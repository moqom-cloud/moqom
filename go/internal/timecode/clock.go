// Asking the kernel how good its clock actually is.
//
// A server disciplined by chrony against `time.cloudflare.com` already knows
// its own error bound — the NTP algorithms compute one and hand it to the
// kernel, which is where `adjtimex` reads it from. Reporting that figure is the
// whole job. What must not happen is the alternative, which is the tempting
// one: assume that because chrony is installed the clock is fine, publish
// `time.Now()` with an implied precision of nanoseconds, and produce an audit
// log that is confidently wrong for the hours after an NTP outage.
//
// # Why not run NTP in the process
//
// The clients do exactly that, and for them it is right: a phone's wall clock
// is a user setting and cannot be trusted. A server is the opposite case. It
// has one system clock, disciplined by one daemon that does the job properly —
// filtering, drift compensation, slewing rather than stepping — and a second
// in-process client would be a worse implementation of the same thing, fighting
// the first, with no way to correct the clock it disagrees with. So the server
// reads the disciplined clock and reports the kernel's own error bound.
//
// The Cloudflare dependency is therefore in the host configuration rather than
// here. See `deploy/modules/*/chrony.conf`.

package timecode

import (
	"log/slog"
	"sync"
	"time"
)

// Clock hands out timecodes.
//
// An interface because tests need a clock that does not move and production
// needs one that does, and because a self-hosted deployment with a real local
// reference should be able to supply its own.
type Clock interface {
	Now() Timecode
}

// Health is what the kernel says about its own discipline.
type Health struct {
	// Synchronised reports whether a time daemon is currently disciplining
	// the clock. False after chrony dies, and false for the first minutes
	// after boot, both of which are exactly when a naive server lies.
	Synchronised bool

	// MaxError is the kernel's own bound on how wrong it might be. It grows
	// on its own between corrections, which is what makes it useful: a host
	// that lost its time source an hour ago reports a wide bound without
	// anybody having to notice the daemon died.
	MaxError time.Duration

	// LeapPending reports a leap second at the end of the current UTC day.
	//
	// Cloudflare does not smear, so this really does arrive as a
	// discontinuity. See Host.Now.
	LeapPending bool
}

// Host is a clock backed by the machine's own disciplined system clock.
type Host struct {
	// Ceiling is the width past which a reading stops claiming to be
	// synchronised, however healthy the kernel says it is.
	//
	// Expressed as a quality rather than an age because that is the useful
	// test: what makes a reading unusable is how wide it is.
	Ceiling time.Duration

	// Logger receives the transitions into and out of fallback time. Nil
	// means slog.Default.
	Logger *slog.Logger

	now    func() time.Time
	read   func() (Health, error)
	expiry time.Duration

	mutex     sync.Mutex
	cached    Health
	failed    bool
	read_     time.Time
	announced Trust
	said      bool
	fallbacks uint64
}

// DefaultCeiling is a generous bound that still catches a broken host.
//
// A healthy machine on a nearby NTP server reports single-digit milliseconds.
// A hundred is far outside that and far inside the seconds-level errors that
// make an audit log meaningless, so it separates "slightly noisy" from
// "something is wrong" without flapping.
const DefaultCeiling = 100 * time.Millisecond

// pollInterval is how long a health reading is reused.
//
// The syscall is cheap but not free, and a busy control plane stamps many
// events per second while the kernel's answer changes on the order of seconds.
const pollInterval = time.Second

// NewHost builds a clock over the system clock.
func NewHost() *Host {
	return &Host{
		Ceiling: DefaultCeiling,
		now:     time.Now,
		read:    readHealth,
		expiry:  pollInterval,
	}
}

// Now returns the current instant and how well it is known.
func (h *Host) Now() Timecode {
	code, health, ok := h.stamp()
	h.announce(code.Trust, health, ok)
	return code
}

// The timecode, and the kernel reading behind it.
//
// The reading comes back as well as the verdict because a fallback timecode is
// given a deliberately enormous width, so its own uncertainty says nothing
// about how wrong the clock actually was. What an operator needs on that
// transition is the kernel's number.
func (h *Host) stamp() (Timecode, Health, bool) {
	at := h.now()
	health, ok := h.health()
	if !ok || !health.Synchronised {
		return Fallback(at), health, ok
	}

	width := health.MaxError

	// A pending leap second makes UTC itself ambiguous by a second at the end
	// of the day, and no amount of NTP quality changes that. Widening for the
	// rest of the day is heavy-handed but it is the honest bound, it happens
	// about once a decade, and the alternative — arithmetic exercised once in
	// ten years, at the exact moment the timeline is discontinuous — is not a
	// thing to be clever with.
	if health.LeapPending {
		width += time.Second
	}

	if width > h.Ceiling {
		return New(at, width, TrustFallback), health, true
	}
	return New(at, width, TrustSynchronised), health, true
}

// announce says, once, when this clock starts or stops being believable.
//
// Falling back is not something a caller sees: Now keeps returning a
// plausible-looking instant, and every consumer that does not inspect Trust
// carries on as though nothing had changed. So the transition has to be said
// out loud, or the first anyone knows of it is an audit trail that will not
// line up against a relay's.
//
// Also counted, via Fallbacks, so the condition is alarmable and not only
// greppable.
//
// The kernel's reading goes on both lines. Without it the warning is a verdict
// with no way to act on it: a host that has genuinely lost its time source and
// one whose error bound merely grew past a ceiling that is too tight for it
// produce the same sentence, and they want opposite fixes.
func (h *Host) announce(trust Trust, health Health, ok bool) {
	h.mutex.Lock()
	if trust == TrustFallback {
		h.fallbacks++
	}
	if h.said && trust == h.announced {
		h.mutex.Unlock()
		return
	}
	previous, first := h.announced, !h.said
	h.announced, h.said = trust, true
	logger, ceiling := h.Logger, h.Ceiling
	h.mutex.Unlock()

	if logger == nil {
		logger = slog.Default()
	}
	if trust == TrustFallback {
		was := previous.String()
		if first {
			was = "never established"
		}
		logger.Warn("clock fell back to local time",
			"previously", was,
			"because", reason(health, ok, ceiling),
			"maxErrorMs", health.MaxError.Milliseconds(),
			"ceilingMs", ceiling.Milliseconds(),
			"consequence", "timecodes cannot be ordered against other hosts",
		)
		return
	}
	logger.Info("clock is disciplined",
		"trust", trust.String(),
		"maxErrorMs", health.MaxError.Milliseconds(),
		"ceilingMs", ceiling.Milliseconds(),
	)
}

// Which of the three ways a clock stops being believable this was.
func reason(health Health, ok bool, ceiling time.Duration) string {
	switch {
	case !ok:
		return "the kernel would not say"
	case !health.Synchronised:
		return "no time daemon is disciplining the clock"
	case health.LeapPending:
		return "a leap second is pending, which widens the bound by a second"
	default:
		return "the kernel's error bound grew past the ceiling"
	}
}

// Report is everything this clock will say about itself.
//
// One struct rather than three getters because it is read by a health endpoint,
// where the parts have to agree with each other: a bound and a ceiling sampled
// a second apart can contradict the verdict printed beside them.
type Report struct {
	Trust     Trust
	Health    Health
	Readable  bool
	Ceiling   time.Duration
	Fallbacks uint64
}

// Report samples the clock without stamping anything.
//
// Deliberately not Now: a probe asking how the clock is should not move the
// transition log or the fallback count, and reading the health is the whole
// question.
func (h *Host) Report() Report {
	code, health, ok := h.stamp()

	h.mutex.Lock()
	defer h.mutex.Unlock()
	return Report{
		Trust:     code.Trust,
		Health:    health,
		Readable:  ok,
		Ceiling:   h.Ceiling,
		Fallbacks: h.fallbacks,
	}
}

// Fallbacks is how many timecodes this clock has stamped from local time.
//
// Exported for the metrics endpoint. A rate above zero on a relay means its
// events cannot be placed against anyone else's, which is worth a page rather
// than a log line somebody reads later.
func (h *Host) Fallbacks() uint64 {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.fallbacks
}

// Healthy reports whether the host clock is currently disciplined.
//
// Split out so an operator can alarm on it directly rather than inferring it
// from the width of timecodes appearing in a log.
func (h *Host) Healthy() (Health, bool) {
	return h.health()
}

func (h *Host) health() (Health, bool) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	at := h.now()
	if !h.read_.IsZero() && at.Sub(h.read_) < h.expiry {
		return h.cached, !h.failed
	}

	health, err := h.read()
	h.read_ = at
	h.cached = health
	h.failed = err != nil
	return h.cached, err == nil
}

// Fixed is a clock that does not move, for tests.
type Fixed struct {
	Timecode Timecode
}

func (f Fixed) Now() Timecode { return f.Timecode }

// FromFunc adapts a bare wall clock, marking everything as fallback time.
//
// For call sites that have a `func() time.Time` and no better source. The
// resulting timecodes say plainly that nothing checked them, which is the
// point: this is a bridge, not a way to launder an unchecked clock into one
// that looks trustworthy.
func FromFunc(now func() time.Time) Clock {
	return funcClock(now)
}

type funcClock func() time.Time

func (f funcClock) Now() Timecode { return Fallback(f()) }
