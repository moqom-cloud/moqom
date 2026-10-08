// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import "time"

// ReasonCode names why a score is what it is. Codes go to moderators and
// reviewers; they never go to the participant, and they are deliberately
// coarser than the rule that produced them.
type ReasonCode string

const (
	ReasonHostingNetwork     ReasonCode = "hosting_network"
	ReasonAnonymisingNetwork ReasonCode = "anonymising_network"
	ReasonHeadlessClient     ReasonCode = "headless_client"
	ReasonConcurrency        ReasonCode = "impossible_concurrency"
	ReasonTravel             ReasonCode = "impossible_travel"
	ReasonSharedDevice       ReasonCode = "shared_device"
	ReasonTokenFarming       ReasonCode = "token_farming"
	ReasonRegularWatch       ReasonCode = "regular_watch"
	ReasonExhaustiveSubs     ReasonCode = "exhaustive_subscription"
	ReasonCanary             ReasonCode = "canary"
	ReasonAttestationReplay  ReasonCode = "attestation_replay"
	ReasonSoftwareKey        ReasonCode = "software_key"
	ReasonTelemetryMismatch  ReasonCode = "telemetry_mismatch"
	ReasonLoopedPublish      ReasonCode = "looped_publish"
	ReasonSyntheticCadence   ReasonCode = "synthetic_cadence"
	ReasonJoinBurst          ReasonCode = "join_burst"
	ReasonRing               ReasonCode = "coordinated_ring"
	ReasonKnownElsewhere     ReasonCode = "known_elsewhere"
	ReasonChatCadence        ReasonCode = "chat_cadence"
	ReasonTipCadence         ReasonCode = "tip_cadence"
	ReasonAnomalous          ReasonCode = "anomalous_behaviour"
	ReasonModel              ReasonCode = "learned_pattern"
	ReasonSyntheticMedia     ReasonCode = "synthetic_media"
	ReasonLivenessFailed     ReasonCode = "liveness_failed"
)

// RuleContext is what a rule may look at.
type RuleContext struct {
	Profile *Profile
	Device  *DeviceState
	Graph   *Graph
	Global  *GlobalIndex
	Tenant  Pseudonym
	Burst   bool
	Now     time.Time
	// Threshold returns a base threshold perturbed for this tenant and epoch.
	Threshold func(name string, base float64) float64
}

// Rule is one known pattern. Weight is log-odds of evidence, so independent
// rules add, and a weight of 2 means "seven times likelier than before".
//
// The weights are starting points chosen by hand, not fitted; the supervised
// model and calibration exist to correct them per tenant.
type Rule struct {
	Code   ReasonCode
	Weight float64
	Match  func(c *RuleContext) bool
}

// DefaultRules are the known patterns.
func DefaultRules() []Rule {
	return []Rule{
		{ReasonHostingNetwork, 1.5, func(c *RuleContext) bool {
			return !c.Profile.Publisher && c.Profile.network.Class == NetworkHosting
		}},
		{ReasonAnonymisingNetwork, 0.5, func(c *RuleContext) bool { return c.Profile.network.Class == NetworkVPN }},
		{ReasonHeadlessClient, 2.5, func(c *RuleContext) bool { return c.Profile.headless }},
		{ReasonConcurrency, 3, func(c *RuleContext) bool {
			return c.Device != nil && float64(c.Device.peak) >= c.Threshold("concurrency", 3.5)
		}},
		{ReasonTravel, 2, func(c *RuleContext) bool {
			return len(c.Profile.countries) >= 2 && c.Profile.LastSeen.Sub(c.Profile.FirstSeen) < 2*time.Hour
		}},
		{ReasonSharedDevice, 2, func(c *RuleContext) bool {
			return c.Graph != nil && float64(c.Graph.AccountsOn(c.Profile.Subject.GlobalDevice)) >= c.Threshold("shared_device", 4.5)
		}},
		{ReasonTokenFarming, 2.5, func(c *RuleContext) bool {
			p := c.Profile
			perSession := float64(p.mints) / float64(max(p.sessions, 1))
			return perSession >= c.Threshold("mints_per_session", 3.5) ||
				(c.Device != nil && float64(c.Device.mintsLastHour(c.Now)) >= c.Threshold("device_mints", 30))
		}},
		{ReasonRegularWatch, 2, func(c *RuleContext) bool {
			cv := c.Profile.watch.cv()
			return c.Profile.watch.n >= 3 && cv >= 0 && cv < c.Threshold("watch_cv", 0.05)
		}},
		{ReasonExhaustiveSubs, 1.5, func(c *RuleContext) bool {
			return !c.Profile.Publisher && float64(len(c.Profile.tracks)) >= c.Threshold("tracks", 6.5)
		}},
		// Near-conclusive by construction: no renderer selects the canary
		// rung, and an honest device cannot produce a stale counter.
		{ReasonCanary, 5.5, func(c *RuleContext) bool { return c.Profile.canaryHits > 0 }},
		{ReasonAttestationReplay, 5, func(c *RuleContext) bool { return c.Device != nil && c.Device.replays > 0 }},
		{ReasonSoftwareKey, 0.7, func(c *RuleContext) bool { return c.Profile.attested && !c.Profile.hardware }},
		{ReasonTelemetryMismatch, 1.5, func(c *RuleContext) bool { return c.Profile.inconsistency >= 2 }},
		{ReasonLoopedPublish, 3, func(c *RuleContext) bool {
			s := c.Profile.publish
			return s != nil && (c.Profile.loop >= c.Threshold("loop", 0.9) || s.Recurrence >= c.Threshold("recurrence", 0.3))
		}},
		{ReasonSyntheticCadence, 1, func(c *RuleContext) bool {
			s := c.Profile.publish
			return s != nil && s.IntervalMean > 0 && float64(s.IntervalStd) < c.Threshold("cadence", float64(150*time.Microsecond))
		}},
		{ReasonJoinBurst, 0.8, func(c *RuleContext) bool { return c.Burst }},
		{ReasonRing, 2.5, func(c *RuleContext) bool {
			return c.Graph != nil && c.Graph.RingOf(c.Profile.Subject.Account) != 0 &&
				c.Graph.LinkOf(c.Profile.Subject.Account)&(LinkDevice|LinkTimed) != 0
		}},
		{ReasonRing, 1.2, func(c *RuleContext) bool {
			return c.Graph != nil && c.Graph.RingOf(c.Profile.Subject.Account) != 0 &&
				c.Graph.LinkOf(c.Profile.Subject.Account) == LinkCoAudience
		}},
		{ReasonKnownElsewhere, 1.5, func(c *RuleContext) bool {
			return c.Global != nil && c.Global.Elsewhere(c.Profile.Subject.GlobalDevice, c.Tenant)
		}},
		{ReasonChatCadence, 2, func(c *RuleContext) bool {
			cv := c.Profile.chatGap.cv()
			return c.Profile.chats >= 5 && cv >= 0 && cv < c.Threshold("chat_cv", 0.1)
		}},
		{ReasonTipCadence, 1.5, func(c *RuleContext) bool {
			cv := c.Profile.tipGap.cv()
			return c.Profile.tips >= 3 && cv >= 0 && cv < c.Threshold("tip_cv", 0.1)
		}},
	}
}

// DeviceState is what is known about one device key across sessions.
type DeviceState struct {
	active  map[string]bool
	peak    int
	mints   []time.Time
	counter uint64
	nonces  map[Pseudonym]bool
	replays int
}

func newDeviceState() *DeviceState {
	return &DeviceState{active: map[string]bool{}, nonces: map[Pseudonym]bool{}}
}

func (d *DeviceState) mintsLastHour(now time.Time) int {
	n := 0
	for _, at := range d.mints {
		if now.Sub(at) <= time.Hour {
			n++
		}
	}
	return n
}

func (d *DeviceState) observe(e Event) {
	switch e.Kind {
	case KindJoin:
		d.active[e.Subject.Key()] = true
		d.peak = max(d.peak, len(d.active))
	case KindLeave:
		delete(d.active, e.Subject.Key())
	case KindTokenMint:
		d.mints = append(d.mints, e.At)
		if len(d.mints) > 128 {
			d.mints = d.mints[len(d.mints)-128:]
		}
	case KindAttestation:
		// A possession proof is single-use and an attestation counter only
		// moves forward. Either going wrong means an attestation was captured
		// and replayed, which is a farm cloning one real device's credential.
		if e.ProofNonce != "" {
			if d.nonces[e.ProofNonce] {
				d.replays++
			}
			if len(d.nonces) < 4096 {
				d.nonces[e.ProofNonce] = true
			}
		}
		if e.AttestCounter != 0 {
			if e.AttestCounter <= d.counter {
				d.replays++
			}
			d.counter = max(d.counter, e.AttestCounter)
		}
	}
}
