// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Band is a score as a moderator sees it.
type Band string

const (
	BandLow      Band = "low"
	BandElevated Band = "elevated"
	BandHigh     Band = "high"
	BandCritical Band = "critical"
)

func bandOf(score float64) Band {
	switch {
	case score >= 0.95:
		return BandCritical
	case score >= 0.6:
		return BandHigh
	case score >= 0.25:
		return BandElevated
	}
	return BandLow
}

// Reason is one contribution to a score.
type Reason struct {
	Code ReasonCode
	// Weight is the log-odds contribution. Internal: tenants get the code.
	Weight float64
	// Detail names a feature, for the anomaly reason only.
	Detail string
}

// Assessment is the detector's view of one participant.
type Assessment struct {
	Subject Subject
	At      time.Time
	// Score is the calibrated probability that the participant is automated.
	Score float64
	// LogOdds is the uncalibrated evidence, kept for calibration and audit.
	LogOdds float64
	// Evidence is LogOdds before the learned correction: what the rules and
	// the anomaly model said on their own.
	Evidence float64
	Band     Band
	Reasons  []Reason
	// Model identifies the detector version that produced this, for the
	// moderation log's DecidedByModel.
	Model string

	publisher bool
}

// Has reports whether the assessment includes a reason.
func (a Assessment) Has(code ReasonCode) bool {
	for _, r := range a.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

// DetectorConfig configures a [Detector].
type DetectorConfig struct {
	// Secrets perturb thresholds and features. Nil disables perturbation,
	// which is for tests only.
	Secrets *Secrets
	// Prior is the log-odds before any evidence. Default logit(0.02).
	Prior float64
	// Rules default to DefaultRules.
	Rules []Rule
	// Retention is how long an idle profile is kept. Default 30 days, the
	// same period as a participant's address, after which nothing here can
	// still describe them.
	Retention time.Duration
	// Version names this configuration in every assessment.
	Version string
	// Platform pseudonymises tenant names for the cross-tenant index.
	Platform *Pseudonymiser
}

// Detector holds per-participant profiles and per-tenant models.
type Detector struct {
	mu       sync.Mutex
	config   DetectorConfig
	profiles map[string]*Profile
	devices  map[Pseudonym]*DeviceState
	tenants  map[string]*tenantModels
	global   *GlobalIndex
}

type tenantModels struct {
	baseline   *Baseline
	learner    *Learner
	graph      *Graph
	bursts     map[string]*Bursts
	burst      map[string]bool
	calibrated bool
	a, b       float64
	admitted   map[string]time.Time
}

// NewDetector returns a detector.
func NewDetector(config DetectorConfig) *Detector {
	if config.Prior == 0 {
		config.Prior = logit(0.02)
	}
	if config.Rules == nil {
		config.Rules = DefaultRules()
	}
	if config.Retention == 0 {
		config.Retention = 30 * 24 * time.Hour
	}
	if config.Version == "" {
		config.Version = "trust-1"
	}
	if config.Platform == nil {
		config.Platform = NewPseudonymiser([]byte("trust-default"))
	}
	return &Detector{
		config:   config,
		profiles: map[string]*Profile{},
		devices:  map[Pseudonym]*DeviceState{},
		tenants:  map[string]*tenantModels{},
		global:   NewGlobalIndex(),
	}
}

func (d *Detector) tenant(name string) *tenantModels {
	t := d.tenants[name]
	if t == nil {
		seed := uint64(len(d.tenants) + 1)
		if d.config.Secrets != nil {
			seed = uint64(d.config.Secrets.Uniform(name, "seed", time.Unix(0, 0)) * (1 << 53))
		}
		t = &tenantModels{
			baseline: NewBaseline(seed),
			learner:  NewLearner(seed ^ 0x5bd1e995),
			graph:    NewGraph(),
			bursts:   map[string]*Bursts{},
			burst:    map[string]bool{},
			a:        1,
			admitted: map[string]time.Time{},
		}
		d.tenants[name] = t
	}
	return t
}

func (d *Detector) deviceKey(s Subject) Pseudonym {
	if s.GlobalDevice != "" {
		return s.GlobalDevice
	}
	return s.Device
}

// Observe folds one event into the profiles and graphs.
func (d *Detector) Observe(e Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := e.Subject.Key()
	p := d.profiles[key]
	if p == nil {
		p = newProfile(e.Subject, e.At)
		d.profiles[key] = p
	}
	p.observe(e)
	if dk := d.deviceKey(e.Subject); dk != "" {
		state := d.devices[dk]
		if state == nil {
			state = newDeviceState()
			d.devices[dk] = state
		}
		state.observe(e)
	}
	t := d.tenant(e.Subject.Tenant)
	if e.Kind == KindJoin {
		t.graph.Device(e.Subject.Account, d.deviceKey(e.Subject))
		var prefix Pseudonym
		if e.Network != nil {
			prefix = e.Network.Prefix
		}
		t.graph.Join(e.Subject.Room, e.Subject.Account, e.At, prefix)
		b := t.bursts[e.Subject.Room]
		if b == nil {
			b = &Bursts{}
			t.bursts[e.Subject.Room] = b
		}
		for _, member := range b.Join(e.At, key) {
			t.burst[member] = true
		}
	}
}

// Sweep recomputes rings and forgets idle profiles. Call it periodically.
func (d *Detector) Sweep(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.tenants {
		t.graph.Detect()
	}
	for key, p := range d.profiles {
		if now.Sub(p.LastSeen) > d.config.Retention {
			delete(d.profiles, key)
			delete(d.tenants[p.Subject.Tenant].burst, key)
			delete(d.tenants[p.Subject.Tenant].admitted, key)
		}
	}
}

var alwaysKept = map[int]bool{15: true, 16: true}

// MinCalibration is how many trusted labels of each class calibration needs.
const MinCalibration = 30

// Assess scores one participant.
func (d *Detector) Assess(subject Subject, now time.Time) (Assessment, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.profiles[subject.Key()]
	if p == nil {
		return Assessment{}, false
	}
	return d.assess(p, now), true
}

func (d *Detector) assess(p *Profile, now time.Time) Assessment {
	tenant := p.Subject.Tenant
	t := d.tenant(tenant)
	ctx := &RuleContext{
		Profile: p,
		Device:  d.devices[d.deviceKey(p.Subject)],
		Graph:   t.graph,
		Global:  d.global,
		Tenant:  d.config.Platform.Platform("tenant", tenant),
		Burst:   t.burst[p.Subject.Key()],
		Now:     now,
		Threshold: func(name string, base float64) float64 {
			return d.config.Secrets.Threshold(tenant, name, base, now)
		},
	}
	z := d.config.Prior
	var reasons []Reason
	for _, rule := range d.config.Rules {
		if rule.Match(ctx) {
			z += rule.Weight
			reasons = append(reasons, Reason{Code: rule.Code, Weight: rule.Weight})
		}
	}
	for _, r := range p.external {
		z += r.Weight
		reasons = append(reasons, r)
	}

	vector := p.Vector(now)
	if iso, robust, feature, ok := t.baseline.Score(vector); ok {
		w := clamp((iso-0.6)*10, 0, 2.5) + clamp((robust-6)*0.25, 0, 1)
		if w > 0 {
			detail := ""
			if feature >= 0 && robust >= 6 {
				detail = FeatureNames[feature]
			}
			z += w
			reasons = append(reasons, Reason{Code: ReasonAnomalous, Weight: w, Detail: detail})
		}
	}
	evidence := z
	if m := clamp(t.learner.Logit(vector), -3, 3); m != 0 {
		z += m
		if m > 0.5 {
			reasons = append(reasons, Reason{Code: ReasonModel, Weight: m})
		}
	}

	score := sigmoid(t.a*z + t.b)
	sort.SliceStable(reasons, func(i, j int) bool { return reasons[i].Weight > reasons[j].Weight })

	// Only people the detector is already confident about teach it what
	// normal is, and each only once a day. Confident by score, not by having
	// no reasons at all: a baseline built only from people with nothing
	// remarkable about them would find everyone on a software key or in a
	// go-live spike anomalous, which is a bias, not a detection.
	if score < 0.1 && now.Sub(t.admitted[p.Subject.Key()]) > 24*time.Hour {
		t.admitted[p.Subject.Key()] = now
		t.baseline.Admit(vector, p.network.Prefix, score, 0.1, d.config.Secrets.Mask(tenant, now, alwaysKept))
	}
	return Assessment{
		Subject: p.Subject, At: now, Score: score, LogOdds: z, Evidence: evidence, Band: bandOf(score),
		Reasons: reasons, Model: d.modelName(t), publisher: p.Publisher,
	}
}

func (d *Detector) modelName(t *tenantModels) string {
	return fmt.Sprintf("%s/m%d", d.config.Version, t.learner.Status().Version)
}

// AssessAll scores every profile in a room ("" for all rooms of the tenant).
func (d *Detector) AssessAll(tenant, room string, now time.Time) []Assessment {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for key, p := range d.profiles {
		if p.Subject.Tenant == tenant && (room == "" || p.Subject.Room == room) {
			keys = append(keys, key)
		}
	}
	// Assessing admits to the baseline, so the order is part of the result.
	sort.Strings(keys)
	out := make([]Assessment, 0, len(keys))
	for _, key := range keys {
		out = append(out, d.assess(d.profiles[key], now))
	}
	return out
}

// Feedback records a human decision about a participant as a training label.
// Confirmed bots also enter the cross-tenant index; overturned ones leave it.
func (d *Detector) Feedback(subject Subject, bot bool, labeler string, now time.Time) bool {
	d.mu.Lock()
	p := d.profiles[subject.Key()]
	if p == nil {
		d.mu.Unlock()
		return false
	}
	vector := p.Vector(now)
	offset := d.assess(p, now).Evidence
	t := d.tenant(subject.Tenant)
	trusted := t.learner.Trusted[labeler] > 0
	tenantP := d.config.Platform.Platform("tenant", subject.Tenant)
	if bot && trusted {
		d.global.Confirm(subject.GlobalDevice, tenantP)
	} else if !bot {
		d.global.Retract(subject.GlobalDevice, tenantP)
	}
	learner := t.learner
	d.mu.Unlock()
	return learner.Add(Label{Vector: vector, Offset: offset, Bot: bot, Labeler: labeler, At: now})
}

// Learner is a tenant's supervised model, for configuring trusted labelers and
// triggering retrains.
func (d *Detector) Learner(tenant string) *Learner {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tenant(tenant).learner
}

// Baseline is a tenant's anomaly model, for drift monitoring.
func (d *Detector) Baseline(tenant string) *Baseline {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tenant(tenant).baseline
}

// Rings is the last ring detection for a tenant.
func (d *Detector) Rings(tenant string) []Ring {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Ring(nil), d.tenant(tenant).graph.rings...)
}

// Calibrate fits Platt scaling for a tenant from labelled assessments, so that
// a score of 0.9 means nine in ten of such participants were bots. Labels must
// come from trusted review, never from the detector's own decisions.
//
// A fit on a handful of positives is noise that moves every score in the
// tenant, so it waits for MinCalibration of each class, and a Gaussian prior
// holds the fit near the identity until the data outweighs it.
func (d *Detector) Calibrate(tenant string, logOdds []float64, bot []bool) {
	positives := 0
	for _, b := range bot {
		if b {
			positives++
		}
	}
	if positives < MinCalibration || len(bot)-positives < MinCalibration {
		return
	}
	a, b := 1.0, 0.0
	const prior = 10.0
	for range 300 {
		ga, gb := prior*(a-1), prior*b
		haa, hab, hbb := prior, 0.0, prior
		for i, z := range logOdds {
			p := sigmoid(a*z + b)
			y := 0.0
			if bot[i] {
				y = 1
			}
			ga += (p - y) * z
			gb += p - y
			w := math.Max(p*(1-p), 1e-9)
			haa += w * z * z
			hab += w * z
			hbb += w
		}
		det := haa*hbb - hab*hab
		if det <= 0 {
			break
		}
		da := (hbb*ga - hab*gb) / det
		db := (haa*gb - hab*ga) / det
		a, b = a-da, b-db
		if math.Abs(da)+math.Abs(db) < 1e-9 {
			break
		}
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return
	}
	// A non-positive slope would invert the ranking, and a steep one turns a
	// small change in evidence into a cliff; neither is a calibration.
	a = clamp(a, 0.25, 4)
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tenant(tenant)
	t.a, t.b, t.calibrated = a, b, true
}
