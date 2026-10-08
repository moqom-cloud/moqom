// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see ../doc.go.

// Package sim is the adversarial harness for package trust: a synthetic tenant
// of simulated people and simulated bot farms, some of which adapt to being
// caught, run through the real detector, policy and learning loop.
//
// Everything here is synthetic. The behaviour distributions are assumptions,
// chosen to be awkward rather than flattering -- humans on datacenter VDI and
// VPNs, households sharing a device, go-live join spikes, viewers who never
// touch the screen -- and the numbers this produces measure the detector
// against those assumptions, not against the world. They are labelled as such
// wherever they are reported.
package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/trust"
)

// Kind is a population.
type Kind string

const (
	Human Kind = "human"
	// NaiveFarm runs headless clients from a datacenter on a fixed schedule.
	NaiveFarm Kind = "naive_farm"
	// ProxyFarm uses residential proxies and spoofs a browser's fingerprint.
	ProxyFarm Kind = "proxy_farm"
	// AdaptiveMimic samples human timing and changes strategy when its
	// accounts are caught.
	AdaptiveMimic Kind = "adaptive_mimic"
	// PhoneFarm is racks of real phones running the real app: hardware keys,
	// mobile networks, human-like timing. The hardest case and the most
	// expensive to run.
	PhoneFarm Kind = "phone_farm"
)

// Kinds in report order.
var Kinds = []Kind{Human, NaiveFarm, ProxyFarm, AdaptiveMimic, PhoneFarm}

// FarmSpec is one farm.
type FarmSpec struct {
	Kind Kind
	Size int
	// Targets is how many customer rooms the farm inflates.
	Targets int
	// Waves per round.
	Waves int
	// AccountsPerDevice is how many accounts share one device key. Phone
	// farms run more accounts than they own handsets; 1 is a farm that has
	// bought a phone per account.
	AccountsPerDevice int
}

// Config is one simulation.
type Config struct {
	Seed   uint64
	Humans int
	Rooms  int
	Rounds int
	Farms  []FarmSpec
	Policy trust.Policy
	// Defended turns on the learner's poisoning defences.
	Defended bool
	// Poison adds a compromised moderator account that inverts labels.
	Poison bool
	// SpotCheck is the share of participants a trusted reviewer labels at
	// random each round, which is also what calibration is fitted on.
	SpotCheck float64
}

// DefaultConfig is the scenario the documentation reports.
func DefaultConfig(seed uint64) Config {
	policy := trust.DefaultPolicy()
	policy.Shadow = false
	return Config{
		Seed: seed, Humans: 3000, Rooms: 40, Rounds: 5,
		Farms: []FarmSpec{
			{Kind: NaiveFarm, Size: 60, Targets: 3, Waves: 6},
			{Kind: ProxyFarm, Size: 60, Targets: 3, Waves: 6},
			{Kind: AdaptiveMimic, Size: 60, Targets: 3, Waves: 6},
			{Kind: PhoneFarm, Size: 45, Targets: 3, Waves: 6, AccountsPerDevice: 3},
		},
		Policy:    policy,
		Defended:  true,
		SpotCheck: 0.03,
	}
}

// KindStats counts outcomes for one population in one round.
type KindStats struct {
	Agents int
	// Excluded is agents with any profile at or above ActionExclude.
	Excluded int
	// Kick is agents for whom a kick or ban was proposed.
	Kick int
	// Flagged is agents at or above ActionShadowFlag.
	Flagged int
}

// Rate returns n over Agents.
func (k KindStats) Rate(n int) float64 {
	if k.Agents == 0 {
		return 0
	}
	return float64(n) / float64(k.Agents)
}

// Round is one simulated day.
type Round struct {
	Index int
	Stats map[Kind]*KindStats
	// TrueBotShare and EstimatedBotShare are over the farms' target rooms'
	// viewers: what share were bots, and what share the room assessment says.
	TrueBotShare, EstimatedBotShare float64
	Learner                         trust.LearnerStatus
	Rings                           int
	MimicLevel                      int
	Suspects                        []string
	// HumanFalsePositiveReasons counts reasons on human profiles that reached
	// exclusion, which is where tuning effort should go.
	HumanFalsePositiveReasons map[trust.ReasonCode]int
}

// Result is a whole simulation.
type Result struct {
	Config Config
	Rounds []Round
}

type agent struct {
	id               int
	kind             Kind
	farm             int
	username, device string
	network          trust.NetworkClass
	prefix           string
	country          string
	travels          bool
	fingerprint      string
	headless         bool
	hardware         bool
	attested         bool
	replay           bool
	watchMedian      float64
	watchSigma       float64
	presenceRate     float64
	switchRate       float64
	chatRate         float64
	chatRegular      bool
	tracks           int
	canary           bool
	mints            int
	spread           time.Duration
	sessions         float64
	rtt, jitter      float64
	cover            bool
	generation       int
}

type farm struct {
	spec    FarmSpec
	targets []int
	members []*agent
	level   int
}

type world struct {
	cfg      Config
	rng      *rand.Rand
	p        *trust.Pseudonymiser
	detector *trust.Detector
	humans   []*agent
	farms    []*farm
	bySubj   map[string]*agent
	zipf     []float64
	spot     struct {
		z   []float64
		bot []bool
	}
	nextID   int
	counters map[string]uint64
	result   Result
	end      time.Time
}

const tenant = "tenant-sim"

var stacks = []string{"ios-sdk", "android-sdk", "chrome-webtransport", "safari-webtransport", "firefox-webtransport", "obs-plugin"}

// Run executes a simulation.
func Run(cfg Config) Result { return runWorld(cfg).result }

func runWorld(cfg Config) *world {
	w := &world{
		cfg:      cfg,
		rng:      rand.New(rand.NewPCG(cfg.Seed, cfg.Seed*0x9e3779b97f4a7c15+1)),
		p:        trust.NewPseudonymiser([]byte(fmt.Sprintf("sim-%d", cfg.Seed))),
		bySubj:   map[string]*agent{},
		counters: map[string]uint64{},
	}
	w.detector = trust.NewDetector(trust.DetectorConfig{
		Secrets:  trust.NewSecrets([]byte(fmt.Sprintf("secret-%d", cfg.Seed))),
		Platform: w.p,
		Version:  "sim",
	})
	learner := w.detector.Learner(tenant)
	learner.Trusted = map[string]float64{"reviewer-1": 1, "reviewer-2": 1}
	if !cfg.Defended {
		learner.Trusted["moderator-x"] = 1
		learner.DailyCap = 1 << 30
		learner.Tolerance = math.Inf(1)
		learner.HoldoutShare = 1e-9
		learner.Undefended = true
	}
	for r := range cfg.Rooms {
		w.zipf = append(w.zipf, 1/math.Pow(float64(r+1), 1.1))
	}
	for range cfg.Humans {
		w.humans = append(w.humans, w.newHuman())
	}
	// Households: a few people share a tablet with one other person.
	for i := 0; i+1 < len(w.humans); i += 2 {
		if w.rng.Float64() < 0.04 {
			w.humans[i+1].device = w.humans[i].device
			w.humans[i+1].hardware = w.humans[i].hardware
		}
	}
	for i, spec := range cfg.Farms {
		f := &farm{spec: spec}
		// Customers buying views are small streamers: the lower half of the
		// popularity curve.
		for _, r := range w.rng.Perm(cfg.Rooms / 2)[:spec.Targets] {
			f.targets = append(f.targets, cfg.Rooms/2+r)
		}
		devices := []string{}
		per := max(spec.AccountsPerDevice, 1)
		for j := range spec.Size {
			a := w.newBot(spec.Kind, i, j)
			if j%per == 0 {
				devices = append(devices, a.device)
			}
			a.device = devices[len(devices)-1]
			f.members = append(f.members, a)
		}
		w.farms = append(w.farms, f)
	}

	w.result.Config = cfg
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	for round := range cfg.Rounds {
		day := start.Add(time.Duration(round) * 24 * time.Hour)
		w.end = day.Add(24 * time.Hour)
		w.result.Rounds = append(w.result.Rounds, w.round(round, day))
	}
	return w
}

func (w *world) id() int { w.nextID++; return w.nextID }

func (w *world) lognormal(median, sigma float64) float64 {
	return median * math.Exp(sigma*w.rng.NormFloat64())
}

func (w *world) newHuman() *agent {
	id := w.id()
	a := &agent{
		id: id, kind: Human, farm: -1,
		username: fmt.Sprintf("user-%d", id), device: fmt.Sprintf("dev-%d", id),
		country:     "US",
		fingerprint: stacks[w.rng.IntN(len(stacks)-1)],
		hardware:    w.rng.Float64() < 0.85,
		attested:    w.rng.Float64() < 0.92,
		watchMedian: w.lognormal(900, 0.5), watchSigma: 0.8 + 0.6*w.rng.Float64(),
		presenceRate: 0.3 + 6*w.rng.Float64(),
		switchRate:   0.1 + 4*w.rng.Float64(),
		tracks:       2 + w.rng.IntN(2),
		mints:        1,
		sessions:     0.5 + 2.5*w.rng.Float64(),
		rtt:          w.lognormal(45, 0.5), jitter: 0.1 + 0.4*w.rng.Float64(),
	}
	if a.hardware {
		a.attested = true
	}
	if w.rng.Float64() < 0.3 {
		a.chatRate = 1 + 15*w.rng.Float64()
	}
	switch u := w.rng.Float64(); {
	case u < 0.55:
		a.network = trust.NetworkResidential
	case u < 0.88:
		a.network = trust.NetworkMobile
	case u < 0.95:
		a.network = trust.NetworkVPN
		a.travels = w.rng.Float64() < 0.15
	default:
		// Corporate VDI and cloud desktops: real people on datacenter ASNs.
		a.network = trust.NetworkHosting
	}
	a.prefix = fmt.Sprintf("h%d", w.rng.IntN(humanPrefixes))
	if w.rng.Float64() < 0.1 {
		a.mints = 2
	}
	return a
}

// humanPrefixes is how many /24s the human population is spread over: enough
// that neighbours share one now and then, as they do behind a carrier NAT.
const humanPrefixes = 1500

func (w *world) newBot(kind Kind, farmIndex, j int) *agent {
	id := w.id()
	a := &agent{
		id: id, kind: kind, farm: farmIndex,
		username: fmt.Sprintf("acct-%d", id), device: fmt.Sprintf("bdev-%d", id),
		country: "US", mints: 1, sessions: 1, attested: true,
	}
	switch kind {
	case NaiveFarm:
		a.network, a.prefix = trust.NetworkHosting, fmt.Sprintf("dc-%d-%d", farmIndex, j%3)
		a.fingerprint, a.headless = "headless-chromium", true
		a.replay = j%3 == 0
		a.watchMedian, a.watchSigma = 1800, 0.01
		a.tracks, a.canary, a.mints = 8, true, 4
		a.spread = 2 * time.Second
		a.rtt, a.jitter = 12, 0.02
	case ProxyFarm:
		a.network = trust.NetworkResidential
		a.prefix = fmt.Sprintf("px-%d", w.rng.IntN(5000))
		a.fingerprint = "chrome-webtransport"
		a.watchMedian, a.watchSigma = 1200, 0.3
		a.presenceRate, a.switchRate = 0.5, 0.5
		a.tracks, a.canary = 3, j%2 == 0
		a.spread = 15 * time.Second
		a.rtt, a.jitter = 140, 0.3
	case AdaptiveMimic:
		a.network = []trust.NetworkClass{trust.NetworkResidential, trust.NetworkMobile}[j%2]
		a.prefix = fmt.Sprintf("mx-%d", w.rng.IntN(5000))
		a.fingerprint = stacks[w.rng.IntN(3)]
		a.watchMedian, a.watchSigma = w.lognormal(900, 0.5), 1.0
		a.presenceRate, a.switchRate = 3, 2
		a.tracks, a.canary = 2, j%2 == 0
		a.spread = 30 * time.Second
		a.rtt, a.jitter = w.lognormal(90, 0.4), 0.3
	case PhoneFarm:
		a.network, a.prefix = trust.NetworkMobile, fmt.Sprintf("pf-%d-%d", farmIndex, j%2)
		a.fingerprint = "ios-sdk"
		a.hardware = true
		a.watchMedian, a.watchSigma = w.lognormal(900, 0.4), 0.9
		a.presenceRate, a.switchRate = 2, 1.5
		a.tracks = 2
		a.spread = 5 * time.Minute
		a.rtt, a.jitter = w.lognormal(60, 0.3), 0.3
	}
	return a
}

type session struct {
	a     *agent
	room  int
	at    time.Time
	watch time.Duration
}

func (w *world) pickRoom() int {
	var total float64
	for _, z := range w.zipf {
		total += z
	}
	u := w.rng.Float64() * total
	for r, z := range w.zipf {
		if u -= z; u <= 0 {
			return r
		}
	}
	return len(w.zipf) - 1
}

func (w *world) round(index int, day time.Time) Round {
	var sessions []session
	goLive := make([]time.Time, w.cfg.Rooms)
	for r := range goLive {
		goLive[r] = day.Add(time.Duration(w.rng.Float64() * 10 * float64(time.Hour)))
	}
	for _, h := range w.humans {
		n := w.poisson(h.sessions)
		for range n {
			room := w.pickRoom()
			at := day.Add(time.Duration(w.rng.Float64() * 12 * float64(time.Hour)))
			if w.rng.Float64() < 0.6 {
				// Most people arrive because the stream went live and their
				// phone told them.
				at = goLive[room].Add(time.Duration(w.rng.ExpFloat64() * 120 * float64(time.Second)))
			}
			sessions = append(sessions, session{a: h, room: room, at: at, watch: w.watchFor(h)})
		}
	}
	for _, f := range w.farms {
		for wave := range f.spec.Waves {
			room := f.targets[wave%len(f.targets)]
			at := day.Add(time.Duration(w.rng.Float64() * 10 * float64(time.Hour)))
			for _, a := range f.members {
				if w.rng.Float64() > 0.9 {
					continue
				}
				join := at.Add(time.Duration(w.rng.Float64() * float64(a.spread)))
				sessions = append(sessions, session{a: a, room: room, at: join, watch: w.watchFor(a)})
			}
		}
		for _, a := range f.members {
			if a.cover {
				for range 3 {
					at := day.Add(time.Duration(w.rng.Float64() * 12 * float64(time.Hour)))
					sessions = append(sessions, session{a: a, room: w.pickRoom(), at: at, watch: w.watchFor(a)})
				}
			}
		}
	}

	// In time order, so a device's attestation counter only moves forward for
	// the honest -- it is per device, and households share one.
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].at.Before(sessions[j].at) })
	var events []trust.Event
	for _, s := range sessions {
		events = append(events, w.emit(s)...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	for _, e := range events {
		w.detector.Observe(e)
	}
	end := day.Add(24 * time.Hour)
	w.detector.Sweep(end)

	out := Round{Index: index, Stats: map[Kind]*KindStats{}, HumanFalsePositiveReasons: map[trust.ReasonCode]int{}}
	for _, k := range Kinds {
		out.Stats[k] = &KindStats{}
	}
	worst := map[*agent]trust.Action{}
	active := map[*agent]bool{}
	for _, s := range sessions {
		active[s.a] = true
	}
	var assessed []trust.Assessment
	for _, a := range w.detector.AssessAll(tenant, "", end) {
		agent := w.bySubj[a.Subject.Key()]
		if agent == nil {
			continue
		}
		assessed = append(assessed, a)
		d := trust.Decide(w.cfg.Policy, a, nil, end)
		if d.Proposed > worst[agent] {
			worst[agent] = d.Proposed
		}
		if agent.kind == Human && d.Proposed >= trust.ActionExclude {
			for _, r := range a.Reasons {
				out.HumanFalsePositiveReasons[r.Code]++
			}
		}
	}
	for a := range active {
		stats := out.Stats[a.kind]
		stats.Agents++
		switch action := worst[a]; {
		case action >= trust.ActionKick:
			stats.Kick++
			fallthrough
		case action >= trust.ActionExclude:
			stats.Excluded++
			fallthrough
		case action >= trust.ActionShadowFlag:
			stats.Flagged++
		}
	}

	// Room view: bot share in the farms' target rooms, true and estimated.
	var trueBots, viewers, suspected int
	seen := map[int]bool{}
	var targets []int
	for _, f := range w.farms {
		for _, r := range f.targets {
			if !seen[r] {
				seen[r] = true
				targets = append(targets, r)
			}
		}
	}
	sort.Ints(targets)
	for _, r := range targets {
		ra := w.detector.AssessRoom(tenant, roomName(r), 0.85, end)
		suspected += ra.Suspected
		for _, a := range w.detector.AssessAll(tenant, roomName(r), end) {
			if ag := w.bySubj[a.Subject.Key()]; ag != nil {
				viewers++
				if ag.kind != Human {
					trueBots++
				}
			}
		}
	}
	if viewers > 0 {
		out.TrueBotShare = float64(trueBots) / float64(viewers)
		out.EstimatedBotShare = float64(suspected) / float64(viewers)
	}

	w.label(assessed, end)
	learner := w.detector.Learner(tenant)
	learner.Retrain()
	w.detector.Calibrate(tenant, w.spot.z, w.spot.bot)
	out.Learner = learner.Status()
	out.Suspects = learner.Suspect()
	out.Rings = len(w.detector.Rings(tenant))
	for _, f := range w.farms {
		if f.spec.Kind == AdaptiveMimic {
			w.adapt(f, worst)
			out.MimicLevel = f.level
		}
	}
	return out
}

func roomName(r int) string { return fmt.Sprintf("room-%d", r) }

func (w *world) poisson(lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= w.rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func (w *world) watchFor(a *agent) time.Duration {
	seconds := clampf(w.lognormal(a.watchMedian, a.watchSigma), 15, 3*3600)
	return time.Duration(seconds * float64(time.Second))
}

func clampf(x, lo, hi float64) float64 { return math.Min(math.Max(x, lo), hi) }

var trackNames = []string{"audio", "video/720", "video/360", "video/180", "screen", "video/1080", "video/1440", "video/540"}

// canaryTrack is advertised as the top rung, in a profile no real decoder
// takes, so the official SDK's render-aware selection never subscribes to it
// and a client that grabs "the best" does.
const canaryTrack = "video/4320"

func (w *world) emit(s session) []trust.Event {
	a := s.a
	subject := trust.SubjectFor(w.p, tenant, roomName(s.room), a.username, a.device)
	w.bySubj[subject.Key()] = a
	network := &trust.Network{Class: a.network, Prefix: w.p.Tenant(tenant, "prefix", a.prefix), Country: a.country}
	ev := func(at time.Time, kind trust.Kind, src trust.Source) trust.Event {
		return trust.Event{At: at, Kind: kind, Source: src, Subject: subject}
	}
	var out []trust.Event
	for i := range a.mints {
		out = append(out, ev(s.at.Add(-time.Duration(a.mints-i)*time.Second), trust.KindTokenMint, trust.SourceControl))
	}
	if a.attested {
		e := ev(s.at.Add(-500*time.Millisecond), trust.KindAttestation, trust.SourceControl)
		e.HardwareBacked = a.hardware
		w.counters[a.device]++
		e.AttestCounter = w.counters[a.device]
		e.ProofNonce = w.p.Tenant(tenant, "nonce", fmt.Sprintf("%s-%d", a.device, e.AttestCounter))
		if a.replay {
			e.AttestCounter = 7
			e.ProofNonce = w.p.Tenant(tenant, "nonce", fmt.Sprintf("captured-%d", a.farm))
		}
		out = append(out, e)
	}
	join := ev(s.at, trust.KindJoin, trust.SourceRelay)
	join.Network = network
	if a.travels && w.rng.Float64() < 0.5 {
		other := *network
		other.Country = "NL"
		join.Network = &other
	}
	out = append(out, join)
	for i := 0; i < a.tracks && i < len(trackNames); i++ {
		e := ev(s.at.Add(time.Duration(200+i*50)*time.Millisecond), trust.KindSubscribe, trust.SourceRelay)
		e.Track = trackNames[i]
		out = append(out, e)
	}
	if a.canary {
		e := ev(s.at.Add(400*time.Millisecond), trust.KindCanaryHit, trust.SourceRelay)
		e.Track = canaryTrack
		out = append(out, e)
	}
	end := s.at.Add(s.watch)
	for t := s.at.Add(time.Minute); t.Before(end); t = t.Add(5 * time.Minute) {
		e := ev(t, trust.KindTransport, trust.SourceRelay)
		rtt := a.rtt * (1 + a.jitter*w.rng.NormFloat64()*0.3)
		e.Transport = &trust.Transport{
			RTT:          time.Duration(math.Max(rtt, 1) * float64(time.Millisecond)),
			RTTVar:       time.Duration(a.jitter * a.rtt * float64(time.Millisecond)),
			LossRatio:    math.Max(0, 0.002+0.004*w.rng.NormFloat64()*a.jitter),
			Fingerprint:  w.p.Tenant(tenant, "fp", a.fingerprint),
			HeadlessHint: a.headless,
		}
		out = append(out, e)
	}
	for t := s.at.Add(10 * time.Minute); t.Before(end); t = t.Add(10 * time.Minute) {
		out = append(out, ev(t, trust.KindTokenRefresh, trust.SourceControl))
	}
	out = append(out, w.poissonEvents(s.at, end, a.presenceRate, func(t time.Time) trust.Event {
		e := ev(t, trust.KindPresence, trust.SourceRelay)
		e.Presence = "backgrounded"
		return e
	})...)
	out = append(out, w.poissonEvents(s.at, end, a.switchRate, func(t time.Time) trust.Event {
		e := ev(t, trust.KindSubscribe, trust.SourceRelay)
		e.Track = trackNames[1+w.rng.IntN(3)]
		return e
	})...)
	if a.chatRate > 0 {
		if a.chatRegular {
			gap := time.Duration(3600 / a.chatRate * float64(time.Second))
			for t := s.at.Add(gap); t.Before(end); t = t.Add(gap) {
				out = append(out, ev(t, trust.KindChat, trust.SourceControl))
			}
		} else {
			out = append(out, w.poissonEvents(s.at, end, a.chatRate, func(t time.Time) trust.Event {
				return ev(t, trust.KindChat, trust.SourceControl)
			})...)
		}
	}
	out = append(out, ev(end, trust.KindLeave, trust.SourceRelay))
	return out
}

func (w *world) poissonEvents(from, to time.Time, perHour float64, build func(time.Time) trust.Event) []trust.Event {
	if perHour <= 0 {
		return nil
	}
	var out []trust.Event
	t := from
	for {
		t = t.Add(time.Duration(w.rng.ExpFloat64() / perHour * float64(time.Hour)))
		if !t.Before(to) {
			return out
		}
		out = append(out, build(t))
	}
}

// label is the moderation loop: trusted reviewers work the queue and spot-check
// at random; a compromised moderator, if configured, inverts labels at volume.
func (w *world) label(assessed []trust.Assessment, now time.Time) {
	for _, a := range assessed {
		ag := w.bySubj[a.Subject.Key()]
		bot := ag.kind != Human
		d := trust.Decide(w.cfg.Policy, a, nil, now)
		if d.NeedsReview {
			verdict := bot
			if w.rng.Float64() < 0.02 {
				verdict = !verdict
			}
			w.detector.Feedback(a.Subject, verdict, "reviewer-1", now)
		}
		if w.rng.Float64() < w.cfg.SpotCheck {
			w.detector.Feedback(a.Subject, bot, "reviewer-2", now)
			w.spot.z = append(w.spot.z, a.LogOdds)
			w.spot.bot = append(w.spot.bot, bot)
		}
	}
	if w.cfg.Poison {
		for i := 0; i < 400 && len(assessed) > 0; i++ {
			a := assessed[w.rng.IntN(len(assessed))]
			bot := w.bySubj[a.Subject.Key()].kind != Human
			w.detector.Feedback(a.Subject, !bot, "moderator-x", now)
			// One in twenty is re-labelled blind by a trusted reviewer.
			if w.cfg.Defended && w.rng.Float64() < 0.05 {
				w.detector.Learner(tenant).Audit("moderator-x", false)
			}
		}
	}
}

// adapt is the adaptive farm's response to coarse, delayed feedback: it sees
// which of its accounts stopped counting (its customer complains), not why.
// Caught accounts are replaced with fresh ones, and if too many were caught it
// escalates to a more expensive way of looking human.
func (w *world) adapt(f *farm, worst map[*agent]trust.Action) {
	caught := 0
	for _, a := range f.members {
		if worst[a] >= trust.ActionExclude {
			caught++
		}
	}
	if float64(caught) > 0.2*float64(len(f.members)) && f.level < 3 {
		f.level++
	}
	for j, a := range f.members {
		if worst[a] >= trust.ActionExclude {
			fresh := w.newBot(AdaptiveMimic, a.farm, j)
			fresh.generation = a.generation + 1
			f.members[j] = fresh
			a = fresh
		}
		if f.level >= 1 {
			a.canary = false
			a.spread = 2 * time.Minute
			a.fingerprint = stacks[w.rng.IntN(2)]
		}
		if f.level >= 2 {
			a.spread = 10 * time.Minute
			a.presenceRate = 0.3 + 6*w.rng.Float64()
			a.switchRate = 0.1 + 4*w.rng.Float64()
			a.watchSigma = 0.8 + 0.6*w.rng.Float64()
		}
		if f.level >= 3 {
			a.cover = true
		}
	}
}
