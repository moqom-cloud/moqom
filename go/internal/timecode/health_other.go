//go:build !linux

// Everywhere that is not a deployment target.
//
// In practice this is a developer's Mac. It reports fallback time rather than
// guessing, which means events stamped on a laptop are visibly marked as
// unorderable against events from anywhere else — and that is the correct
// answer, not a limitation to work around. macOS does have `ntp_adjtime`, but
// reaching it needs cgo, and buying a nicer local development experience with a
// cgo dependency in the production binary is a bad trade.

package timecode

func readHealth() (Health, error) {
	return Health{Synchronised: false}, nil
}
