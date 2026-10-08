// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

var t0 = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

type fakeResolver struct{ class NetworkClass }

func (f fakeResolver) Resolve(netip.Addr) (uint32, NetworkClass, string) { return 64500, f.class, "US" }

func TestPseudonymsAreScopedAndKeyed(t *testing.T) {
	p := NewPseudonymiser([]byte("k1"))
	if p.Tenant("a", "device", "x") == p.Tenant("b", "device", "x") {
		t.Error("the same device has the same pseudonym in two tenants; tenants could join their data")
	}
	if p.Tenant("a", "device", "x") != p.Tenant("a", "device", "x") {
		t.Error("pseudonyms are not stable")
	}
	if NewPseudonymiser([]byte("k2")).Tenant("a", "device", "x") == p.Tenant("a", "device", "x") {
		t.Error("rotating the key did not sever the link")
	}
	if strings.Contains(string(p.Platform("device", "thumb-123")), "thumb") {
		t.Error("a pseudonym leaks its input")
	}
}

func TestClassifyAddressKeepsNoAddress(t *testing.T) {
	p := NewPseudonymiser([]byte("k"))
	n := ClassifyAddress(p, fakeResolver{NetworkHosting}, "t", "[::ffff:203.0.113.9]:4433")
	m := ClassifyAddress(p, fakeResolver{NetworkHosting}, "t", "203.0.113.200")
	if n.Prefix != m.Prefix {
		t.Error("two addresses in one /24, one IPv4-mapped, got different prefixes")
	}
	body, _ := json.Marshal(n)
	if strings.Contains(string(body), "203.0.113") {
		t.Fatalf("the classified network still contains the address: %s", body)
	}
	if n.Class != NetworkHosting || n.ASN != 64500 {
		t.Errorf("resolver output lost: %+v", n)
	}
	if got := ClassifyAddress(p, nil, "t", "not an address"); got.Prefix != "" {
		t.Errorf("garbage produced a prefix: %+v", got)
	}
}

func TestEventCarriesNoRawIdentifiers(t *testing.T) {
	// The schema is the privacy boundary: no field may hold a username, an
	// address, media or message text. Checked by name so a field added later
	// has to be argued for.
	forbidden := []string{"addr", "ip", "username", "email", "text", "frame", "payload", "media"}
	for _, typ := range []reflect.Type{reflect.TypeOf(Event{}), reflect.TypeOf(Subject{}), reflect.TypeOf(Network{}), reflect.TypeOf(Transport{})} {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, f := range forbidden {
				if name == f || strings.HasPrefix(name, f) {
					t.Errorf("%s.%s looks like a raw identifier or content", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

func subject(n int) Subject {
	p := NewPseudonymiser([]byte("k"))
	return SubjectFor(p, "t", "r", fmt.Sprintf("user-%d", n), fmt.Sprintf("dev-%d", n))
}

func TestCanaryAndReplayAreStrongEnoughAlone(t *testing.T) {
	d := NewDetector(DetectorConfig{})
	s := subject(1)
	d.Observe(Event{At: t0, Kind: KindJoin, Subject: s})
	before, _ := d.Assess(s, t0)
	d.Observe(Event{At: t0, Kind: KindCanaryHit, Subject: s, Track: "video/4320"})
	after, _ := d.Assess(s, t0)
	if !after.Has(ReasonCanary) || after.Score <= before.Score || after.Score < 0.5 {
		t.Errorf("canary: before %.3f after %.3f %v", before.Score, after.Score, after.Reasons)
	}

	r := subject(2)
	for i, counter := range []uint64{5, 6, 6} {
		d.Observe(Event{At: t0.Add(time.Duration(i) * time.Minute), Kind: KindAttestation, Subject: r, AttestCounter: counter, HardwareBacked: true})
	}
	if a, _ := d.Assess(r, t0); !a.Has(ReasonAttestationReplay) {
		t.Errorf("a repeated attestation counter was not a replay: %v", a.Reasons)
	}
}

func TestImpossibleConcurrencyAndTokenFarming(t *testing.T) {
	d := NewDetector(DetectorConfig{})
	base := subject(1)
	for room := range 5 {
		s := base
		s.Room = fmt.Sprintf("r%d", room)
		d.Observe(Event{At: t0, Kind: KindJoin, Subject: s})
	}
	base.Room = "r0"
	if a, _ := d.Assess(base, t0); !a.Has(ReasonConcurrency) {
		t.Errorf("one device in five rooms at once: %v", a.Reasons)
	}
	farm := subject(9)
	for i := range 40 {
		d.Observe(Event{At: t0.Add(time.Duration(i) * time.Second), Kind: KindTokenMint, Subject: farm})
	}
	d.Observe(Event{At: t0.Add(time.Minute), Kind: KindJoin, Subject: farm})
	if a, _ := d.Assess(farm, t0.Add(time.Minute)); !a.Has(ReasonTokenFarming) {
		t.Errorf("forty mints for one join: %v", a.Reasons)
	}
}

func TestRegularWatchNeedsEvidence(t *testing.T) {
	d := NewDetector(DetectorConfig{})
	bot, person := subject(1), subject(2)
	for i := range 4 {
		start := t0.Add(time.Duration(i) * time.Hour)
		d.Observe(Event{At: start, Kind: KindJoin, Subject: bot})
		d.Observe(Event{At: start.Add(30 * time.Minute), Kind: KindLeave, Subject: bot})
		d.Observe(Event{At: start, Kind: KindJoin, Subject: person})
		d.Observe(Event{At: start.Add(time.Duration(5+i*13) * time.Minute), Kind: KindLeave, Subject: person})
	}
	if a, _ := d.Assess(bot, t0.Add(5*time.Hour)); !a.Has(ReasonRegularWatch) {
		t.Errorf("four identical sessions: %v", a.Reasons)
	}
	if a, _ := d.Assess(person, t0.Add(5*time.Hour)); a.Has(ReasonRegularWatch) {
		t.Errorf("irregular sessions flagged: %v", a.Reasons)
	}
}

func TestLoopScoreIgnoresGOPAndTrends(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	camera := make([]uint32, 300)
	level := 8000.0
	for i := range camera {
		level *= 1 + 0.01*rng.NormFloat64()
		camera[i] = uint32(level * (1 + 0.15*rng.NormFloat64()))
	}
	if s, _ := LoopScore(camera); s > 0.5 {
		t.Errorf("a trending camera scored %.2f", s)
	}
	clip := camera[:40]
	var loop []uint32
	for len(loop) < 300 {
		loop = append(loop, clip...)
	}
	if s, lag := LoopScore(loop); s < 0.95 || lag%40 != 0 {
		t.Errorf("an exact 40-frame loop scored %.2f at lag %d", s, lag)
	}
	if s, _ := LoopScore(camera[:10]); s != 0 {
		t.Error("too short a sample produced an opinion")
	}
}

func TestReplayIndex(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	clip := make([]uint32, 900)
	for i := range clip {
		clip[i] = uint32(4000 + rng.IntN(8000))
	}
	var replay, live ReplayIndex
	for i := range 5000 {
		replay.Add(clip[i%len(clip)])
		live.Add(uint32(4000 + rng.IntN(8000)))
	}
	if replay.Recurrence() < 0.9 {
		t.Errorf("an exact replay recurred %.2f", replay.Recurrence())
	}
	if live.Recurrence() > 0.01 {
		t.Errorf("a live stream recurred %.2f", live.Recurrence())
	}
	var black ReplayIndex
	for range 5000 {
		black.Add(uint32(120 + rng.IntN(3)))
	}
	if black.Recurrence() != 0 {
		t.Errorf("a black screen read as a replay: %.2f", black.Recurrence())
	}
}

func TestRingFromSharedDevicesAndCoArrival(t *testing.T) {
	g := NewGraph()
	for i := range 6 {
		g.Device(Pseudonym(fmt.Sprintf("acct-%d", i)), "one-phone")
	}
	// A different group arrives together in three rooms.
	for room := range 3 {
		for i := range 5 {
			g.Join(fmt.Sprintf("room-%d", room), Pseudonym(fmt.Sprintf("sched-%d", i)), t0.Add(time.Duration(room)*time.Hour+time.Duration(i)*200*time.Millisecond), "")
		}
	}
	// Strangers who once arrived together.
	for i := range 5 {
		g.Join("room-9", Pseudonym(fmt.Sprintf("stranger-%d", i)), t0.Add(time.Duration(i)*100*time.Millisecond), "")
	}
	g.Detect()
	if g.RingOf("acct-0") == 0 || g.RingOf("acct-0") != g.RingOf("acct-5") {
		t.Error("six accounts on one device are not a ring")
	}
	if g.RingOf("sched-0") == 0 || g.LinkOf("sched-0")&LinkTimed == 0 {
		t.Error("repeated co-arrival in three rooms is not a ring")
	}
	if g.RingOf("stranger-0") != 0 {
		t.Error("one co-arrival made strangers a ring")
	}
}

func TestCoAudienceIgnoresPopularRooms(t *testing.T) {
	g := NewGraph()
	// 200 people all watch the big room; that links nobody.
	for i := range 200 {
		g.Join("big", Pseudonym(fmt.Sprintf("fan-%d", i)), t0.Add(time.Duration(i)*time.Minute), "")
		g.Join(fmt.Sprintf("own-%d", i), Pseudonym(fmt.Sprintf("fan-%d", i)), t0, "")
	}
	// A farm watches the same three small rooms, at unrelated times.
	for i := range 8 {
		for room := range 3 {
			g.Join(fmt.Sprintf("customer-%d", room), Pseudonym(fmt.Sprintf("bot-%d", i)), t0.Add(time.Duration(i*37+room*101)*time.Minute), "")
		}
	}
	g.Detect()
	if g.RingOf("fan-1") != 0 {
		t.Error("sharing the most popular room made a ring")
	}
	if g.RingOf("bot-0") == 0 || g.LinkOf("bot-0") != LinkCoAudience {
		t.Errorf("a farm sharing three rare rooms was not linked by co-audience: ring %d link %d", g.RingOf("bot-0"), g.LinkOf("bot-0"))
	}
}

func TestBurstsAgainstTheRoomsOwnRate(t *testing.T) {
	var b Bursts
	var flagged []string
	at := t0
	for i := range 60 {
		at = at.Add(30 * time.Second)
		flagged = append(flagged, b.Join(at, fmt.Sprint("steady-", i))...)
	}
	if len(flagged) != 0 {
		t.Fatalf("a steady trickle was a burst: %v", flagged)
	}
	at = at.Add(30 * time.Second).Truncate(5 * time.Second)
	for i := range 20 {
		b.Join(at.Add(time.Duration(i)*100*time.Millisecond), fmt.Sprint("burst-", i))
	}
	flagged = b.Join(at.Add(time.Minute), "after")
	if len(flagged) != 20 {
		t.Errorf("twenty joins in two seconds flagged %d", len(flagged))
	}
	if poissonTail(1, 0) != 1 || poissonTail(1, 30) > 1e-20 {
		t.Error("poisson tail is wrong")
	}
}

func TestGlobalIndexIsKAnonymous(t *testing.T) {
	x := NewGlobalIndex()
	x.Confirm("dev", "t1")
	x.Confirm("dev", "t2")
	if x.Elsewhere("dev", "t9") {
		t.Error("two confirmations revealed; one tenant could learn what one competitor saw")
	}
	x.Confirm("dev", "t3")
	if !x.Elsewhere("dev", "t9") || x.Elsewhere("dev", "t3") {
		t.Error("three others should reveal, and a tenant's own confirmation should not count")
	}
	x.Retract("dev", "t1")
	if x.Elsewhere("dev", "t9") {
		t.Error("a retracted confirmation still counts")
	}
}

func TestSecretsMoveThresholds(t *testing.T) {
	s := NewSecrets([]byte("k"))
	a := s.Threshold("t1", "x", 1, t0)
	b := s.Threshold("t2", "x", 1, t0)
	c := s.Threshold("t1", "x", 1, t0.Add(48*time.Hour))
	if a == b && a == c {
		t.Error("thresholds do not vary by tenant or epoch")
	}
	for _, v := range []float64{a, b, c} {
		if v < 0.88 || v > 1.12 {
			t.Errorf("threshold %.3f outside the spread", v)
		}
	}
	if s.Threshold("t1", "x", 1, t0) != a {
		t.Error("the same tenant and epoch drew differently")
	}
	mask := s.Mask("t1", t0, map[int]bool{15: true})
	dropped := 0
	for _, m := range mask {
		if !m {
			dropped++
		}
	}
	if !mask[15] || dropped == 0 || dropped == len(mask) {
		t.Errorf("mask kept %v", mask)
	}
	if (*Secrets)(nil).Threshold("t", "x", 2, t0) != 2 {
		t.Error("nil secrets should leave thresholds alone")
	}
}

func TestBaselineDriftAlarmAndAdmission(t *testing.T) {
	b := NewBaseline(1)
	b.RefitEvery = 64
	rng := rand.New(rand.NewPCG(5, 6))
	vec := func(shift float64) []float64 {
		v := make([]float64, len(FeatureNames))
		for i := range v {
			v[i] = 1 + 0.1*rng.NormFloat64()
		}
		v[3] += shift
		return v
	}
	for i := range 400 {
		b.Admit(vec(0), Pseudonym(fmt.Sprint(i)), 0, 0.1, nil)
	}
	if !b.Ready() {
		t.Fatal("not ready after 400 samples")
	}
	normal, _, _, _ := b.Score(vec(0))
	odd, z, feature, _ := b.Score(vec(3))
	if odd <= normal || z < 6 || feature != 3 {
		t.Errorf("an outlier on feature 3: iso %.3f vs %.3f, z %.1f on %d", odd, normal, z, feature)
	}
	for i := range 2000 {
		b.Admit(vec(0), "one-farm", 0, 0.1, nil)
		b.Admit(vec(3), Pseudonym(fmt.Sprint("high-", i)), 0.5, 0.1, nil)
		_ = i
	}
	if _, alarm := b.Drift(); alarm {
		t.Error("high-risk samples and one capped group raised the alarm, so they got in")
	}
	for i := range 3000 {
		b.Admit(vec(1.5), Pseudonym(fmt.Sprint("drift-", i)), 0, 0.1, nil)
	}
	if _, alarm := b.Drift(); !alarm {
		t.Error("a shifted population did not raise the drift alarm")
	}
	b.AcceptDrift()
	if _, alarm := b.Drift(); alarm {
		t.Error("accepting drift did not clear the alarm")
	}
}

func TestLearnerRefusesARetrainThatHurtsTheHoldout(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	l := NewLearner(1)
	l.Trusted = map[string]float64{"rev": 1}
	l.HoldoutShare = 0.3
	example := func(bot bool) []float64 {
		v := make([]float64, len(FeatureNames))
		for i := range v {
			v[i] = rng.NormFloat64()
		}
		if bot {
			v[0] += 2
		}
		return v
	}
	for i := range 300 {
		bot := i%2 == 0
		l.Add(Label{Vector: example(bot), Bot: bot, Labeler: "rev", At: t0})
	}
	if !l.Retrain() {
		t.Fatal("first retrain refused")
	}
	if l.Logit(example(true)) <= l.Logit(example(false))-1 {
		t.Error("the model did not learn the separating feature")
	}
	// A trusted-but-compromised account floods inverted labels at full
	// weight; only the holdout gate stands in the way.
	l.Trusted["insider"] = 1
	l.DailyCap = 1 << 20
	for i := range 3000 {
		bot := i%2 == 0
		l.Add(Label{Vector: example(bot), Bot: !bot, Labeler: "insider", At: t0})
	}
	if l.Retrain() {
		t.Error("a retrain on inverted labels was adopted")
	}
	if st := l.Status(); st.Rejected != 1 {
		t.Errorf("status %+v", st)
	}
	if len(l.Suspect()) == 0 {
		t.Error("the insider was not flagged as a suspect labeler")
	}
}

func TestLearnerCapsAndAudits(t *testing.T) {
	l := NewLearner(2)
	l.DailyCap = 5
	accepted := 0
	for range 10 {
		if l.Add(Label{Vector: make([]float64, 3), Labeler: "spammer", At: t0}) {
			accepted++
		}
	}
	if accepted != 5 {
		t.Errorf("cap let %d labels through", accepted)
	}
	for range 10 {
		l.Audit("spammer", false)
	}
	if f := l.factor("spammer"); f != 0 {
		t.Errorf("a labeler who never agreed with an audit keeps weight %.2f", f)
	}
}

func TestCalibrationNeedsBothClasses(t *testing.T) {
	d := NewDetector(DetectorConfig{})
	d.Calibrate("t", []float64{1, 2, 3}, []bool{true, true, false})
	if d.tenant("t").calibrated {
		t.Error("calibrated on three labels")
	}
	var z []float64
	var y []bool
	for i := range 200 {
		v := float64(i%20) - 10
		z = append(z, v)
		y = append(y, v > 2)
	}
	d.Calibrate("t", z, y)
	tm := d.tenant("t")
	if !tm.calibrated || tm.a <= 0 || tm.a > 4 {
		t.Errorf("calibration a=%.2f b=%.2f", tm.a, tm.b)
	}
}

func assessment(score float64, reasons int, publisher bool) Assessment {
	a := Assessment{Subject: subject(1), Score: score, At: t0, publisher: publisher, Model: "m"}
	for i := range reasons {
		a.Reasons = append(a.Reasons, Reason{Code: ReasonCode(fmt.Sprint("r", i))})
	}
	return a
}

func TestPolicyLadder(t *testing.T) {
	p := DefaultPolicy()
	if d := Decide(p, assessment(0.999, 4, false), nil, t0); !d.Shadow || d.Applied != ActionShadowFlag || d.Proposed != ActionBan {
		t.Errorf("shadow mode: %+v", d)
	}
	p.Shadow = false
	d := Decide(p, assessment(0.999, 4, false), nil, t0)
	if d.Proposed != ActionBan || d.Applied != ActionExclude || !d.NeedsReview || !d.Exclude {
		t.Errorf("a ban must wait for a human, excluding meanwhile: %+v", d)
	}
	if d.NotBefore.Before(t0) || d.NotBefore.After(t0.Add(p.MaxDelay)) {
		t.Errorf("delay out of range: %v", d.NotBefore)
	}
	if d := Decide(p, assessment(0.99, 1, false), nil, t0); d.Proposed >= ActionThrottle {
		t.Errorf("one reason reached %s", d.Proposed)
	}
	if d := Decide(p, assessment(0.999, 4, true), nil, t0); d.Applied > ActionShadowFlag || !d.NeedsReview {
		t.Errorf("a publisher was acted on automatically: %+v", d)
	}
	if d := Decide(p, assessment(0.1, 0, false), nil, t0); d.Proposed != ActionNone {
		t.Errorf("a low score did something: %+v", d)
	}
}

type recorder struct{ applied, lifted []Enforcement }

func (r *recorder) Apply(_ context.Context, e Enforcement) error {
	r.applied = append(r.applied, e)
	return nil
}
func (r *recorder) Lift(_ context.Context, e Enforcement) error {
	r.lifted = append(r.lifted, e)
	return nil
}

func TestReviewAppealAndAudit(t *testing.T) {
	ctx := context.Background()
	p := NewPseudonymiser([]byte("k"))
	rec := &recorder{}
	q := &Queue{Enforcer: rec, Audit: &AuditLog{}, Exclusions: NewExclusions(p)}
	policy := DefaultPolicy()
	policy.Shadow = false
	a := assessment(0.999, 4, false)
	a.Subject = SubjectFor(p, "t", "r", "alice", "phone")
	id, err := q.Process(ctx, Decide(policy, a, nil, t0))
	if err != nil || id == "" {
		t.Fatalf("no review opened: %q %v", id, err)
	}
	if len(rec.applied) != 1 || rec.applied[0].Action != ActionExclude || rec.applied[0].Code != SubjectCode {
		t.Fatalf("automatic part: %+v", rec.applied)
	}
	if strings.Contains(rec.applied[0].Code, "bot") {
		t.Error("the subject-facing code names the mechanism")
	}
	if !q.Exclusions.Excluded(a.Subject) {
		t.Error("not excluded while awaiting review")
	}
	if err := q.Resolve(ctx, id, "rev-1", true, ActionBan, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec.applied[len(rec.applied)-1].Action != ActionBan {
		t.Error("the reviewer's ban was not applied")
	}
	if _, err := q.Appeal("nope", "", t0); err != ErrNoCase {
		t.Errorf("appeal on a missing case: %v", err)
	}
	appeal, err := q.Appeal(id, "I am a person", t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pending := q.Pending(); len(pending) != 1 || pending[0].Kind != CaseAppeal {
		t.Errorf("pending %+v", pending)
	}
	if err := q.Resolve(ctx, appeal, "rev-1", false, ActionNone, t0.Add(3*time.Hour)); err != ErrSameEyes {
		t.Errorf("the original reviewer decided the appeal: %v", err)
	}
	if err := q.Resolve(ctx, appeal, "rev-2", false, ActionNone, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(rec.lifted) != 1 || rec.lifted[0].Action != ActionBan || q.Exclusions.Excluded(a.Subject) {
		t.Errorf("an upheld appeal did not lift the reviewer's ban and the exclusion: %+v", rec.lifted)
	}
	if err := q.Resolve(ctx, appeal, "rev-3", true, ActionBan, t0); err != ErrResolved {
		t.Errorf("a resolved appeal was reopened: %v", err)
	}

	entries := q.Audit.Entries()
	if len(entries) < 4 {
		t.Fatalf("audit has %d entries", len(entries))
	}
	if err := q.Audit.Verify(); err != nil {
		t.Fatal(err)
	}
	q.Audit.entries[1].Note = "edited"
	if q.Audit.Verify() == nil {
		t.Error("an edited audit entry verified")
	}
}

func TestAutomaticActionsAreAppealable(t *testing.T) {
	ctx := context.Background()
	p := NewPseudonymiser([]byte("k"))
	rec := &recorder{}
	q := &Queue{Enforcer: rec, Audit: &AuditLog{}, Exclusions: NewExclusions(p)}
	policy := DefaultPolicy()
	policy.Shadow = false
	a := assessment(0.9, 3, false)
	a.Subject = SubjectFor(p, "t", "r", "bob", "phone")
	d := Decide(policy, a, nil, t0)
	if d.NeedsReview || !d.Exclude {
		t.Fatalf("expected an automatic exclusion: %+v", d)
	}
	id, err := q.Process(ctx, d)
	if err != nil || id == "" {
		t.Fatalf("an automatic exclusion left nothing to appeal: %q %v", id, err)
	}
	if len(q.Pending()) != 0 {
		t.Error("an automatic action is waiting in the review queue")
	}
	appeal, err := q.Appeal(id, "not a bot", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Resolve(ctx, appeal, "rev-1", false, ActionNone, t0); err != nil {
		t.Fatal(err)
	}
	if q.Exclusions.Excluded(a.Subject) || len(rec.lifted) != 1 {
		t.Errorf("upheld appeal: excluded=%t lifted=%+v", q.Exclusions.Excluded(a.Subject), rec.lifted)
	}
}

func TestExclusionsFeedCountsAndBilling(t *testing.T) {
	p := NewPseudonymiser([]byte("k"))
	x := NewExclusions(p)
	x.Set(SubjectFor(p, "t", "r", "bot", "d1"), true)
	viewer := control.Participant{Identity: control.Identity{Username: "bot", Device: "d1"}}
	if x.Countable("t", "r", viewer) {
		t.Error("an excluded viewer is counted")
	}
	if !x.Countable("t", "other", viewer) {
		t.Error("exclusion leaked into another room")
	}
	host := viewer
	host.Grants.Publish = 1
	if !x.Countable("t", "r", host) {
		t.Error("a publisher was dropped from counts")
	}
	if no, why := x.Unbillable(context.Background(), "t", "r", "bot/d1"); !no || why == "" {
		t.Error("an excluded viewer is billable")
	}
	if no, _ := x.Unbillable(context.Background(), "t", "r", "alice/d2"); no {
		t.Error("an ordinary viewer is unbillable")
	}
	if x.ExcludedIn("t", "r") != 1 {
		t.Error("room count wrong")
	}
	x.Set(SubjectFor(p, "t", "r", "bot", "d1"), false)
	if x.ExcludedIn("t", "r") != 0 {
		t.Error("room count not decremented")
	}
}

func TestTipsFromARingToItsOwnStreamerAreHeld(t *testing.T) {
	p := NewPseudonymiser([]byte("k"))
	d := NewDetector(DetectorConfig{Platform: p})
	for i := range 6 {
		s := SubjectFor(p, "t", "r", fmt.Sprint("acct-", i), "shared-phone")
		d.Observe(Event{At: t0, Kind: KindJoin, Subject: s})
	}
	streamer := SubjectFor(p, "t", "r", "acct-0", "shared-phone")
	_ = streamer
	d.Sweep(t0)
	scorer := &TipScorer{Detector: d, Pseudonyms: p}
	v := scorer.Score(context.Background(), TipObservation{TipID: "tip1", Tenant: "t", Room: "r", FromUsername: "acct-1", FromDevice: "shared-phone", ToUsername: "acct-0", AmountBand: 6, At: t0})
	if !v.Hold {
		t.Errorf("a tip inside one ring settled: %+v", v)
	}
	clean := scorer.Score(context.Background(), TipObservation{TipID: "tip2", Tenant: "t", Room: "r", FromUsername: "fan", FromDevice: "own-phone", ToUsername: "acct-0", AmountBand: 6, At: t0})
	if clean.Hold {
		t.Errorf("an unremarkable tip was held: %+v", clean)
	}
}

type fakeC2PA struct{ result C2PAResult }

func (f fakeC2PA) Validate(context.Context, []byte) (C2PAResult, error) { return f.result, nil }

func TestC2PA(t *testing.T) {
	ctx := context.Background()
	gen := C2PADetector{Validator: fakeC2PA{C2PAResult{Valid: true, SignerTrusted: true, DigitalSourceType: "http://cv.iptc.org/newscodes/digitalsourcetype/trainedAlgorithmicMedia", Generator: "x"}}}
	v, _ := gen.Inspect(ctx, MediaSample{Manifest: []byte{1}})
	if v.Provenance != ProvenanceSynthetic || len(MediaEvidence([]MediaVerdict{v})) != 1 {
		t.Errorf("declared synthetic: %+v", v)
	}
	if v, _ := gen.Inspect(ctx, MediaSample{}); v.Provenance != ProvenanceAbsent || len(MediaEvidence([]MediaVerdict{v})) != 0 {
		t.Errorf("an absent manifest was evidence: %+v", v)
	}
	camera := C2PADetector{Validator: fakeC2PA{C2PAResult{Valid: true, SignerTrusted: true, DigitalSourceType: "http://cv.iptc.org/newscodes/digitalsourcetype/digitalCapture"}}}
	if v, _ := camera.Inspect(ctx, MediaSample{Manifest: []byte{1}}); v.Provenance != ProvenanceCaptured {
		t.Errorf("capture: %+v", v)
	}
	forged := C2PADetector{Validator: fakeC2PA{C2PAResult{Valid: false}}}
	if v, _ := forged.Inspect(ctx, MediaSample{Manifest: []byte{1}}); v.Provenance != ProvenanceInvalid {
		t.Errorf("invalid: %+v", v)
	}
}

func TestExternalClassifierRespectsOptInAndE2EE(t *testing.T) {
	ctx := context.Background()
	c := ExternalClassifier{Endpoint: "http://127.0.0.1:1", OptedIn: func(tenant string) bool { return tenant == "yes" }}
	if _, err := c.Inspect(ctx, MediaSample{Subject: Subject{Tenant: "no"}, Keyframe: []byte{1}}); err != ErrNotOptedIn {
		t.Errorf("a tenant that did not opt in was classified: %v", err)
	}
	v, err := c.Inspect(ctx, MediaSample{Subject: Subject{Tenant: "yes"}, Keyframe: []byte{1}, E2EE: true})
	if err != nil || v.Synthetic != -1 {
		t.Errorf("an E2EE frame was sent: %+v %v", v, err)
	}
}

func TestChallenges(t *testing.T) {
	s := NewSecrets([]byte("k"))
	kinds := map[ChallengeKind]bool{}
	for i := range 50 {
		kinds[NewChallenge(s, subject(i), "video/720", true, t0.Add(time.Duration(i)*time.Second)).Kind] = true
	}
	if len(kinds) < 3 {
		t.Errorf("challenge kinds are predictable: %v", kinds)
	}
	c := Challenge{Kind: ChallengeFreshKeyframe, Issued: t0, Deadline: t0.Add(1500 * time.Millisecond)}
	if VerifyChallenge(c, RelayObservation{GroupStarts: []time.Time{t0.Add(-time.Second), t0.Add(2 * time.Second)}}) != ChallengeFailed {
		t.Error("keyframes outside the window passed")
	}
	if VerifyChallenge(c, RelayObservation{GroupStarts: []time.Time{t0.Add(40 * time.Millisecond)}}) != ChallengePassed {
		t.Error("a prompt keyframe failed")
	}
	toggle := Challenge{Kind: ChallengeTrackToggle, Issued: t0, Deadline: t0.Add(15 * time.Second)}
	if VerifyChallenge(toggle, RelayObservation{PublishOff: t0.Add(2 * time.Second), PublishOn: t0.Add(5 * time.Second)}) != ChallengePassed {
		t.Error("a toggle failed")
	}
	if VerifyChallenge(Challenge{Kind: ChallengePrompt}, RelayObservation{PromptMatched: -1}) != ChallengeInconclusive {
		t.Error("a prompt with no classifier was decided")
	}
	var l ChallengeLimiter
	if !l.Allow("d", t0) || l.Allow("d", t0.Add(time.Minute)) || !l.Allow("d", t0.Add(2*time.Hour)) {
		t.Error("challenge rate limit")
	}
	if len(LivenessReason(ChallengePassed)) != 0 || len(LivenessReason(ChallengeFailed)) != 1 {
		t.Error("liveness reasons")
	}
}

func TestSamplerIsIrregular(t *testing.T) {
	sampler := &Sampler{Secrets: NewSecrets([]byte("k")), Mean: 10 * time.Second}
	var gaps []time.Duration
	last := time.Time{}
	for i := range 2000 {
		at := t0.Add(time.Duration(i) * 100 * time.Millisecond)
		if sampler.Take(subject(1), "video", at) {
			if !last.IsZero() {
				gaps = append(gaps, at.Sub(last))
			}
			last = at
		}
	}
	distinct := map[time.Duration]bool{}
	for _, g := range gaps {
		distinct[g] = true
	}
	if len(gaps) < 5 || len(distinct) < 3 {
		t.Errorf("sampling gaps %v", gaps)
	}
}

func TestRetentionForgetsIdleProfiles(t *testing.T) {
	d := NewDetector(DetectorConfig{})
	s := subject(1)
	d.Observe(Event{At: t0, Kind: KindJoin, Subject: s})
	d.Sweep(t0.Add(29 * 24 * time.Hour))
	if _, ok := d.Assess(s, t0); !ok {
		t.Fatal("forgot too early")
	}
	d.Sweep(t0.Add(31 * 24 * time.Hour))
	if _, ok := d.Assess(s, t0); ok {
		t.Error("a profile outlived the 30-day retention")
	}
}

func TestAttachedMediaEvidenceRaisesThePublisher(t *testing.T) {
	d := NewDetector(DetectorConfig{})
	s := subject(1)
	d.Observe(Event{At: t0, Kind: KindJoin, Subject: s, Publisher: true})
	before, _ := d.Assess(s, t0)
	d.Attach(s, MediaEvidence([]MediaVerdict{{Provenance: ProvenanceSynthetic}}))
	after, _ := d.Assess(s, t0)
	if !after.Has(ReasonSyntheticMedia) || after.Score <= before.Score {
		t.Errorf("before %.3f after %.3f", before.Score, after.Score)
	}
	room := d.AssessRoom("t", "r", 0, t0)
	if room.Publisher == nil || room.Publisher.Score != after.Score {
		t.Errorf("room view lost the publisher: %+v", room)
	}
}
