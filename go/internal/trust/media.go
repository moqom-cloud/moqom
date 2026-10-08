// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Synthetic-media detection.
//
// # What this can and cannot do
//
// Nobody can reliably tell a good real-time deepfake from a camera by looking
// at the pixels, and this package does not pretend to. Published detectors do
// well on the generators they were trained against and degrade badly on new
// ones, on compressed video, and on adversarially perturbed frames; a live
// stream is all three. So detection here is layered, and each layer says how
// much it is worth:
//
//   - Provenance (C2PA) is the strongest signal when present and says nothing
//     when absent. A valid manifest declaring a generative source is close to
//     proof; a missing manifest is what nearly every real camera produces
//     today. Absence is never evidence.
//   - Classifiers are probabilistic and decay. They run server-side, on
//     sampled keyframes, only for tenants who opted in, and only in rooms that
//     are not end-to-end encrypted -- an E2EE room's frames are not ours to
//     decode, and that is a feature.
//   - Relay-side structure (loop detection, cadence) works under E2EE because
//     it reads object sizes and timing, not pixels. It catches replayed
//     recordings, not live synthesis.
//   - Liveness challenges raise the cost of a pre-recorded or pre-rendered
//     stream. Real-time face-swap pipelines can follow simple prompts, so a
//     passed challenge is weak evidence of a human and a failed one is
//     moderate evidence of something else.
//
// None of these should end a stream on its own. They produce reasons for a
// human, and the policy's PublisherCeiling keeps it that way.

// ProvenanceState is what a provenance check found.
type ProvenanceState string

const (
	ProvenanceAbsent    ProvenanceState = "absent"
	ProvenanceInvalid   ProvenanceState = "invalid"
	ProvenanceCaptured  ProvenanceState = "captured"
	ProvenanceSynthetic ProvenanceState = "declared_synthetic"
	ProvenanceUntrusted ProvenanceState = "untrusted_signer"
)

// MediaSample is one sampled keyframe and what travelled with it. Held in
// memory for the duration of an Inspect call and never written anywhere.
type MediaSample struct {
	Subject Subject
	Track   string
	At      time.Time
	Codec   string
	// Keyframe is the encoded frame. Empty in an E2EE room.
	Keyframe []byte
	// Manifest is a C2PA manifest store carried alongside the track, if any.
	Manifest []byte
	E2EE     bool
}

// MediaVerdict is one detector's opinion.
type MediaVerdict struct {
	Detector   string
	Provenance ProvenanceState
	// Synthetic is the probability the frame is generated, or -1 for no
	// opinion. Classifier output, uncalibrated across vendors.
	Synthetic float64
	Note      string
}

// MediaDetector is a pluggable synthetic-media check.
type MediaDetector interface {
	Name() string
	Inspect(ctx context.Context, sample MediaSample) (MediaVerdict, error)
}

// C2PAResult is what a C2PA validator reports. The validation itself --
// COSE signature, certificate chain to the C2PA trust list, hard-binding hash
// over the asset -- is done by a C2PA SDK in the private media service; this
// package interprets the result.
type C2PAResult struct {
	Valid         bool
	SignerTrusted bool
	// DigitalSourceType is the IPTC term from the manifest's actions.
	DigitalSourceType string
	Generator         string
}

// C2PAValidator validates a manifest store.
type C2PAValidator interface {
	Validate(ctx context.Context, manifest []byte) (C2PAResult, error)
}

// C2PADetector checks content credentials.
type C2PADetector struct{ Validator C2PAValidator }

// Name implements MediaDetector.
func (C2PADetector) Name() string { return "c2pa" }

// syntheticSourceTypes are the IPTC digital source types that mean generated.
var syntheticSourceTypes = []string{
	"trainedAlgorithmicMedia",
	"compositeWithTrainedAlgorithmicMedia",
	"algorithmicMedia",
	"compositeSynthetic",
}

// Inspect implements MediaDetector.
func (c C2PADetector) Inspect(ctx context.Context, sample MediaSample) (MediaVerdict, error) {
	verdict := MediaVerdict{Detector: c.Name(), Synthetic: -1, Provenance: ProvenanceAbsent}
	if len(sample.Manifest) == 0 || c.Validator == nil {
		return verdict, nil
	}
	result, err := c.Validator.Validate(ctx, sample.Manifest)
	if err != nil {
		return verdict, err
	}
	switch {
	case !result.Valid:
		verdict.Provenance = ProvenanceInvalid
	case !result.SignerTrusted:
		verdict.Provenance = ProvenanceUntrusted
	default:
		verdict.Provenance = ProvenanceCaptured
		for _, t := range syntheticSourceTypes {
			if strings.HasSuffix(result.DigitalSourceType, "/"+t) || result.DigitalSourceType == t {
				verdict.Provenance = ProvenanceSynthetic
				verdict.Synthetic = 1
				verdict.Note = result.Generator
			}
		}
	}
	return verdict, nil
}

// ExternalClassifier sends sampled keyframes to a deepfake classifier: a
// Vertex AI endpoint or a vendor. It is a subprocessor, so a tenant must opt in
// under its DPA, and it never sees an E2EE frame.
type ExternalClassifier struct {
	// Endpoint accepts {"codec","frame"} and returns {"synthetic_probability"}.
	Endpoint string
	// KeyEnv names the environment variable holding the API key, populated
	// from Secret Manager in project moqom-503700. The key is read per call
	// and never logged.
	KeyEnv string
	// OptedIn reports whether a tenant has enabled server-side classification.
	OptedIn func(tenant string) bool
	Client  *http.Client
}

// ErrNotOptedIn is returned for a tenant that has not enabled classification.
var ErrNotOptedIn = errors.New("tenant has not opted in to media classification")

// Name implements MediaDetector.
func (ExternalClassifier) Name() string { return "external_classifier" }

// Inspect implements MediaDetector.
func (e ExternalClassifier) Inspect(ctx context.Context, sample MediaSample) (MediaVerdict, error) {
	verdict := MediaVerdict{Detector: e.Name(), Synthetic: -1}
	if e.OptedIn == nil || !e.OptedIn(sample.Subject.Tenant) {
		return verdict, ErrNotOptedIn
	}
	if sample.E2EE || len(sample.Keyframe) == 0 {
		verdict.Note = "no frame available"
		return verdict, nil
	}
	body, _ := json.Marshal(map[string]string{"codec": sample.Codec, "frame": base64.StdEncoding.EncodeToString(sample.Keyframe)})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(body))
	if err != nil {
		return verdict, err
	}
	request.Header.Set("Content-Type", "application/json")
	if e.KeyEnv != "" {
		if key := os.Getenv(e.KeyEnv); key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return verdict, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return verdict, fmt.Errorf("classifier returned %s", response.Status)
	}
	var out struct {
		Probability float64 `json:"synthetic_probability"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 1<<16)).Decode(&out); err != nil {
		return verdict, err
	}
	verdict.Synthetic = clamp(out.Probability, 0, 1)
	return verdict, nil
}

// Sampler decides which keyframes to inspect. Intervals are keyed-random, so a
// publisher cannot time a swap between samples.
type Sampler struct {
	Secrets *Secrets
	// Mean interval between samples per track. Default 20s.
	Mean time.Duration
	last map[string]time.Time
	next map[string]time.Duration
}

// Take reports whether a keyframe at at on track should be sampled.
func (s *Sampler) Take(subject Subject, track string, at time.Time) bool {
	if s.last == nil {
		s.last, s.next = map[string]time.Time{}, map[string]time.Duration{}
	}
	mean := s.Mean
	if mean == 0 {
		mean = 20 * time.Second
	}
	key := subject.Key() + "\x00" + track
	last, seen := s.last[key]
	if seen && at.Sub(last) < s.next[key] {
		return false
	}
	u := 0.5
	if s.Secrets != nil {
		u = s.Secrets.Uniform(subject.Tenant, "sample:"+key+at.String(), at)
	}
	s.last[key] = at
	s.next[key] = time.Duration((0.25 + 1.5*u) * float64(mean))
	return true
}

// MediaEvidence turns detector verdicts into reasons. A classifier's opinion is
// capped well below a provenance declaration, for the reasons above.
func MediaEvidence(verdicts []MediaVerdict) []Reason {
	var reasons []Reason
	for _, v := range verdicts {
		switch {
		case v.Provenance == ProvenanceSynthetic:
			reasons = append(reasons, Reason{Code: ReasonSyntheticMedia, Weight: 4, Detail: "declared"})
		case v.Synthetic >= 0.9:
			reasons = append(reasons, Reason{Code: ReasonSyntheticMedia, Weight: 1.5, Detail: v.Detector})
		}
	}
	return reasons
}

// Liveness challenges.
//
// The challenge is a protocol message, but the protocol is between the
// control plane and the tenant's own backend -- delivered as a room event the
// tenant's app renders -- not a new message in the public SDKs or the Apache
// proto. Verification uses only what the relay observes, so a modified client
// cannot report success.

// ChallengeKind is what is asked.
type ChallengeKind string

const (
	// ChallengeFreshKeyframe asks for a new group (keyframe) on a track within
	// a deadline. The relay sees group boundaries in object headers. A real
	// encoder answers in one frame interval; a pre-encoded file being replayed
	// cannot produce a keyframe at an arbitrary instant without re-encoding.
	ChallengeFreshKeyframe ChallengeKind = "fresh_keyframe"
	// ChallengeTrackToggle asks the host to stop and restart video. The relay
	// sees the publish state change and its timing against the prompt.
	ChallengeTrackToggle ChallengeKind = "track_toggle"
	// ChallengePrompt shows a randomised visual prompt (a gesture, a number of
	// fingers). Verifying it needs a classifier on the frames that follow, so it
	// is available only where ExternalClassifier is.
	ChallengePrompt ChallengeKind = "visual_prompt"
)

// Challenge is the protocol message.
type Challenge struct {
	ID       string        `json:"id"`
	Kind     ChallengeKind `json:"kind"`
	Track    string        `json:"track,omitempty"`
	Prompt   string        `json:"prompt,omitempty"`
	Issued   time.Time     `json:"issued"`
	Deadline time.Time     `json:"deadline"`
}

// ChallengeOutcome is a verification result.
type ChallengeOutcome string

const (
	ChallengePassed       ChallengeOutcome = "passed"
	ChallengeFailed       ChallengeOutcome = "failed"
	ChallengeInconclusive ChallengeOutcome = "inconclusive"
)

// RelayObservation is what the relay saw after a challenge was issued.
type RelayObservation struct {
	// GroupStarts are times new groups began on the challenged track.
	GroupStarts []time.Time
	// PublishOff and PublishOn are when the track stopped and resumed.
	PublishOff, PublishOn time.Time
	// PromptMatched is the classifier's answer for a visual prompt, -1 if none.
	PromptMatched float64
}

// VerifyChallenge judges a challenge from relay observations alone.
func VerifyChallenge(c Challenge, seen RelayObservation) ChallengeOutcome {
	switch c.Kind {
	case ChallengeFreshKeyframe:
		for _, at := range seen.GroupStarts {
			if !at.Before(c.Issued) && !at.After(c.Deadline) {
				return ChallengePassed
			}
		}
		return ChallengeFailed
	case ChallengeTrackToggle:
		if seen.PublishOff.IsZero() || seen.PublishOn.IsZero() {
			return ChallengeFailed
		}
		if seen.PublishOff.Before(c.Issued) || seen.PublishOn.After(c.Deadline) || seen.PublishOn.Before(seen.PublishOff) {
			return ChallengeFailed
		}
		return ChallengePassed
	case ChallengePrompt:
		switch {
		case seen.PromptMatched < 0:
			return ChallengeInconclusive
		case seen.PromptMatched >= 0.8:
			return ChallengePassed
		case seen.PromptMatched <= 0.2:
			return ChallengeFailed
		}
		return ChallengeInconclusive
	}
	return ChallengeInconclusive
}

// NewChallenge draws a challenge. The kind and the prompt are keyed-random, so
// a farm cannot pre-render answers.
func NewChallenge(secrets *Secrets, subject Subject, track string, classifier bool, now time.Time) Challenge {
	u := 0.5
	if secrets != nil {
		u = secrets.Uniform(subject.Tenant, "challenge:"+subject.Key()+now.String(), now)
	}
	c := Challenge{ID: fmt.Sprintf("ch-%x", uint64(u*(1<<53))), Track: track, Issued: now}
	prompts := []string{"raise_one_hand", "show_two_fingers", "show_three_fingers", "turn_head_left", "turn_head_right", "touch_nose"}
	switch {
	case classifier && u < 0.4:
		c.Kind, c.Prompt, c.Deadline = ChallengePrompt, prompts[int(u*2.5*float64(len(prompts)))%len(prompts)], now.Add(10*time.Second)
	case u < 0.7:
		c.Kind, c.Deadline = ChallengeFreshKeyframe, now.Add(1500*time.Millisecond)
	default:
		c.Kind, c.Deadline = ChallengeTrackToggle, now.Add(15*time.Second)
	}
	return c
}

// ChallengeLimiter bounds challenges per device. A challenge is a response to
// a score, so an unlimited supply would let a farm use them as an oracle for
// where its score sits.
type ChallengeLimiter struct {
	// Per is the minimum interval between challenges to one device. Default 1h.
	Per  time.Duration
	last map[Pseudonym]time.Time
}

// Allow reports whether device may be challenged now, and records it if so.
func (l *ChallengeLimiter) Allow(device Pseudonym, now time.Time) bool {
	if l.last == nil {
		l.last = map[Pseudonym]time.Time{}
	}
	per := l.Per
	if per == 0 {
		per = time.Hour
	}
	if last, ok := l.last[device]; ok && now.Sub(last) < per {
		return false
	}
	l.last[device] = now
	return true
}

// LivenessReason converts an outcome to a reason. Passing removes nothing.
func LivenessReason(outcome ChallengeOutcome) []Reason {
	if outcome == ChallengeFailed {
		return []Reason{{Code: ReasonLivenessFailed, Weight: 2}}
	}
	return nil
}
