// A position on the one timeline everything can be compared against.
//
// The Go half of `MoqomCore.Timecode`. The two are deliberately the same shape,
// because the whole value of the type is that a phone, a relay and the control
// plane produce timestamps a human can later place in one order — and that
// falls apart the moment one of them means something slightly different by it.
// Where this file and the Swift type disagree, the Swift type is right and this
// is a bug.
//
// The reasoning is written out there and not repeated here. The short version:
// a timecode is an interval rather than an instant, because two events a
// millisecond apart on machines synced to fifty are simply not ordered and a
// log that renders them as ordered has invented the answer.
package timecode

import (
	"fmt"
	"time"
)

// Trust says how much a timecode should be believed.
type Trust uint8

const (
	// Unsynchronised means no measurement stands behind this value.
	//
	// The zero value on purpose. A struct that someone forgot to fill in
	// should read as "we do not know", never as "this is good time".
	Unsynchronised Trust = iota

	// Synchronised means measured against the reference, recently enough.
	Synchronised

	// Authoritative means produced by a reference clock itself.
	//
	// Nothing MOQOM runs claims this today and that is correct: our servers
	// are disciplined by Cloudflare's NTP, which makes them synchronised
	// clients of an authority rather than authorities. It exists for a
	// self-hosted deployment with a real local reference — GPS or PTP — where
	// the claim would be true.
	Authoritative
)

// String matches the Swift `TimeTrust` raw values, since these cross the wire.
func (t Trust) String() string {
	switch t {
	case Authoritative:
		return "authoritative"
	case Synchronised:
		return "synchronised"
	default:
		return "unsynchronised"
	}
}

// Timecode is an instant with the width of what is actually known about it.
type Timecode struct {
	// At is the best estimate.
	At time.Time

	// Uncertainty is half the width of the interval the true instant lies in:
	// the value is At ± Uncertainty. Zero is not something a real clock
	// produces and nothing here generates it.
	Uncertainty time.Duration

	Trust Trust
}

// New builds a timecode, refusing a negative width.
//
// Negative uncertainty is nonsense that would silently invert every comparison
// below, turning "cannot tell" into a confident wrong answer.
func New(at time.Time, uncertainty time.Duration, trust Trust) Timecode {
	if uncertainty < 0 {
		uncertainty = 0
	}
	return Timecode{At: at, Uncertainty: uncertainty, Trust: trust}
}

// Unchecked is the fallback for a process that cannot ask a reference clock.
//
// Marked Unsynchronised so everything downstream knows what it has, and given
// a deliberately enormous width rather than a plausible one: an undisciplined
// clock can be wrong by years, and a bound implying otherwise would be exactly
// the lie this type exists to prevent.
func Unchecked(at time.Time) Timecode {
	return New(at, 24*time.Hour, Unsynchronised)
}

// Earliest is the earliest instant this could be.
func (t Timecode) Earliest() time.Time { return t.At.Add(-t.Uncertainty) }

// Latest is the latest instant this could be.
func (t Timecode) Latest() time.Time { return t.At.Add(t.Uncertainty) }

// IsTrusted reports whether this can be placed against another machine's.
func (t Timecode) IsTrusted() bool { return t.Trust != Unsynchronised }

// IsZero reports whether this holds no instant at all.
func (t Timecode) IsZero() bool { return t.At.IsZero() }

// DefinitelyPrecedes reports whether this provably happened before other.
//
// Deliberately strict: true only when the intervals do not overlap, and false
// when they do, including when the estimates differ. Both must be trusted,
// since an unsynchronised timecode is one machine's opinion with nothing to
// place it against.
//
// For two entries from the *same* source, use that source's sequence number
// instead. Sequence orders within a source and is exact; a timecode orders
// across sources and is not, so reaching for one here discards a certainty you
// already had.
func (t Timecode) DefinitelyPrecedes(other Timecode) bool {
	if !t.IsTrusted() || !other.IsTrusted() {
		return false
	}
	return t.Latest().Before(other.Earliest())
}

// MayCoincide reports whether these two could be the same instant.
//
// True whenever the intervals overlap, and true whenever either is
// unsynchronised, since there is then no basis for ruling it out.
func (t Timecode) MayCoincide(other Timecode) bool {
	if !t.IsTrusted() || !other.IsTrusted() {
		return true
	}
	return !t.Earliest().After(other.Latest()) && !other.Earliest().After(t.Latest())
}

// Widened returns the same estimate, known less precisely.
//
// For carrying a timecode across something that adds doubt. Uncertainty only
// ever grows this way, which is the point: nothing here makes a timecode more
// certain than it was measured.
func (t Timecode) Widened(extra time.Duration) Timecode {
	if extra < 0 {
		extra = 0
	}
	return New(t.At, t.Uncertainty+extra, t.Trust)
}

// String renders the width too, because a timecode printed without it is the
// thing this type exists to keep out of logs.
func (t Timecode) String() string {
	stamp := t.At.UTC().Format(time.RFC3339Nano)
	if !t.IsTrusted() {
		return stamp + " (unsynchronised)"
	}
	return fmt.Sprintf("%s ±%.1fms", stamp, float64(t.Uncertainty)/float64(time.Millisecond))
}
