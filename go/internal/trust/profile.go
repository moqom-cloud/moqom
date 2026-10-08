// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"math"
	"time"
)

// Profile is everything the detector knows about one participant in one room,
// as running summaries rather than a history: an event is folded in and then
// discarded, so the profile's size does not grow with the length of a stream.
type Profile struct {
	Subject   Subject
	Publisher bool
	FirstSeen time.Time
	LastSeen  time.Time

	sessions      int
	openAt        time.Time
	watch         running // session seconds
	presence      int
	joinTimes     []time.Time
	subscribes    int
	unsubscribes  int
	switchGap     running // seconds between subscription changes
	lastSwitch    time.Time
	tracks        map[string]bool
	mints         int
	refreshes     int
	refreshGap    running
	lastRefresh   time.Time
	rtt           running // ms
	jitterRatio   running
	loss          running
	fingerprint   Pseudonym
	headless      bool
	network       Network
	countries     map[string]bool
	chats         int
	chatGap       running
	lastChat      time.Time
	tips          int
	tipGap        running
	lastTip       time.Time
	publish       *PublishStats
	loop          float64
	canaryHits    int
	hardware      bool
	attested      bool
	inconsistency int
	clientEvents  int
	external      []Reason
}

func newProfile(subject Subject, at time.Time) *Profile {
	return &Profile{Subject: subject, FirstSeen: at, LastSeen: at, tracks: map[string]bool{}, countries: map[string]bool{}}
}

const maxJoinTimes = 16

func (p *Profile) observe(e Event) {
	if e.At.After(p.LastSeen) {
		p.LastSeen = e.At
	}
	p.Publisher = p.Publisher || e.Publisher
	if e.Network != nil {
		p.network = *e.Network
		if e.Network.Country != "" && len(p.countries) < 8 {
			p.countries[e.Network.Country] = true
		}
	}
	if e.Source == SourceClient {
		p.clientEvents++
	}
	switch e.Kind {
	case KindJoin:
		p.sessions++
		p.openAt = e.At
		p.joinTimes = append(p.joinTimes, e.At)
		if len(p.joinTimes) > maxJoinTimes {
			p.joinTimes = p.joinTimes[1:]
		}
	case KindLeave:
		if !p.openAt.IsZero() {
			p.watch.add(e.At.Sub(p.openAt).Seconds())
			p.openAt = time.Time{}
		}
	case KindPresence:
		p.presence++
	case KindSubscribe, KindUnsubscribe:
		if e.Kind == KindSubscribe {
			p.subscribes++
			if len(p.tracks) < 64 {
				p.tracks[e.Track] = true
			}
		} else {
			p.unsubscribes++
		}
		if !p.lastSwitch.IsZero() {
			p.switchGap.add(e.At.Sub(p.lastSwitch).Seconds())
		}
		p.lastSwitch = e.At
	case KindTokenMint:
		p.mints++
	case KindTokenRefresh:
		p.refreshes++
		if !p.lastRefresh.IsZero() {
			p.refreshGap.add(e.At.Sub(p.lastRefresh).Seconds())
		}
		p.lastRefresh = e.At
	case KindTransport:
		if t := e.Transport; t != nil {
			ms := float64(t.RTT) / float64(time.Millisecond)
			p.rtt.add(ms)
			if ms > 0 {
				p.jitterRatio.add(float64(t.RTTVar) / float64(t.RTT))
			}
			p.loss.add(t.LossRatio)
			p.fingerprint = t.Fingerprint
			p.headless = p.headless || t.HeadlessHint
		}
	case KindChat:
		p.chats++
		if !p.lastChat.IsZero() {
			p.chatGap.add(e.At.Sub(p.lastChat).Seconds())
		}
		p.lastChat = e.At
	case KindTip:
		p.tips++
		if !p.lastTip.IsZero() {
			p.tipGap.add(e.At.Sub(p.lastTip).Seconds())
		}
		p.lastTip = e.At
	case KindPublishStats:
		if e.Publish != nil {
			stats := *e.Publish
			p.publish = &stats
			p.loop, _ = LoopScore(stats.Sizes)
		}
	case KindCanaryHit:
		p.canaryHits++
	case KindAttestation:
		p.attested = true
		p.hardware = p.hardware || e.HardwareBacked
	case KindClientTelemetry:
		if c := e.Claim; c != nil && p.publish != nil && p.publish.IntervalMean > 0 {
			observed := float64(time.Second) / float64(p.publish.IntervalMean)
			if c.FrameRate > 0 && math.Abs(c.FrameRate-observed)/observed > 0.25 {
				p.inconsistency++
			}
		}
	}
}

// Feature names, in vector order. The order is part of a trained model's
// meaning: append, never reorder.
var FeatureNames = []string{
	"sessions_log",
	"watch_mean_log",
	"watch_cv",
	"switch_rate",
	"switch_cv",
	"tracks",
	"refresh_cv",
	"mints_per_session",
	"rtt_log",
	"rtt_jitter",
	"loss",
	"presence_rate",
	"chat_rate",
	"chat_cv",
	"tip_rate",
	"hosting",
	"hardware",
	"countries",
}

// Vector is the profile as numbers for the anomaly and supervised models.
//
// Rates are per hour connected, with a floor of ten minutes so one event in a
// fifteen-second visit is not a rate of 240 an hour, and are log-scaled because
// people's rates are heavy-tailed. A coefficient of variation that cannot be
// computed yet is reported as 1 -- "as irregular as a person" -- so that too
// little data never looks like a machine.
func (p *Profile) Vector(now time.Time) []float64 {
	connected := p.watch.mean * float64(p.watch.n)
	if !p.openAt.IsZero() {
		connected += now.Sub(p.openAt).Seconds()
	}
	hours := math.Max(connected/3600, 1.0/6)
	rate := func(n int) float64 { return math.Log1p(float64(n) / hours) }
	cv := func(r running) float64 {
		if c := r.cv(); c >= 0 {
			return math.Min(c, 5)
		}
		return 1
	}
	watchMean := p.watch.mean
	if p.watch.n == 0 && !p.openAt.IsZero() {
		watchMean = now.Sub(p.openAt).Seconds()
	}
	bool01 := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	return []float64{
		math.Log1p(float64(p.sessions)),
		math.Log1p(watchMean),
		cv(p.watch),
		rate(p.subscribes + p.unsubscribes),
		cv(p.switchGap),
		math.Log1p(float64(len(p.tracks))),
		cv(p.refreshGap),
		float64(p.mints) / math.Max(float64(p.sessions), 1),
		math.Log1p(p.rtt.mean),
		p.jitterRatio.mean,
		p.loss.mean,
		rate(p.presence),
		rate(p.chats),
		cv(p.chatGap),
		rate(p.tips),
		bool01(p.network.Class == NetworkHosting),
		bool01(p.hardware),
		float64(len(p.countries)),
	}
}
