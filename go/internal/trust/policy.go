// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Action is what is done about an assessment, in increasing severity.
type Action int

const (
	ActionNone Action = iota
	// ActionLog records the assessment and nothing else.
	ActionLog
	// ActionShadowFlag marks the participant for moderators without any
	// effect on them.
	ActionShadowFlag
	// ActionChallenge asks for a step-up: a liveness challenge, or a
	// re-attestation from the device key.
	ActionChallenge
	// ActionThrottle lowers what the participant may subscribe to.
	ActionThrottle
	// ActionExclude removes the participant from viewer counts, billable
	// usage and tip eligibility, while still letting them watch.
	ActionExclude
	// ActionKick ends the session; they may rejoin.
	ActionKick
	// ActionBan bars the device key from the room or tenant.
	ActionBan
)

var actionNames = []string{"none", "log", "shadow_flag", "challenge", "throttle", "exclude", "kick", "ban"}

func (a Action) String() string {
	if int(a) < len(actionNames) {
		return actionNames[a]
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Step maps a score to an action.
type Step struct {
	MinScore float64
	Action   Action
	// MinReasons is how many independent reasons must agree. One strong rule
	// can raise a score a long way; it should not on its own end a session.
	MinReasons int
}

// Policy is one tenant's enforcement configuration.
type Policy struct {
	Steps []Step
	// Shadow computes every decision and applies none beyond a shadow flag.
	// Every tenant starts here, and should stay until its false-positive rate
	// has been measured on its own traffic.
	Shadow bool
	// HumanFrom is the least severe action that needs a human. Anything at or
	// above it is queued for review and only AutoCeiling is applied meanwhile.
	HumanFrom   Action
	AutoCeiling Action
	// PublisherCeiling caps automatic action against someone with a publish
	// grant. Removing a streamer is a decision about a business, not a viewer.
	PublisherCeiling Action
	// MaxDelay bounds the random delay before an automated action, so that an
	// outcome cannot be lined up with the change that caused it.
	MaxDelay time.Duration
}

// DefaultPolicy is shadow mode with human review from kick upwards.
func DefaultPolicy() Policy {
	return Policy{
		Steps: []Step{
			{MinScore: 0.25, Action: ActionLog},
			{MinScore: 0.5, Action: ActionShadowFlag},
			{MinScore: 0.7, Action: ActionChallenge, MinReasons: 1},
			{MinScore: 0.8, Action: ActionThrottle, MinReasons: 2},
			{MinScore: 0.85, Action: ActionExclude, MinReasons: 2},
			{MinScore: 0.97, Action: ActionKick, MinReasons: 2},
			{MinScore: 0.995, Action: ActionBan, MinReasons: 3},
		},
		Shadow:           true,
		HumanFrom:        ActionKick,
		AutoCeiling:      ActionExclude,
		PublisherCeiling: ActionShadowFlag,
		MaxDelay:         3 * time.Minute,
	}
}

// Decision is what the policy made of an assessment.
type Decision struct {
	Assessment Assessment
	// Proposed is what the score warrants; Applied is what is done now.
	Proposed Action
	Applied  Action
	// Exclude is whether the participant leaves counts, billing and tips.
	Exclude     bool
	NeedsReview bool
	Shadow      bool
	NotBefore   time.Time
}

// Decide applies a policy to an assessment. secrets may be nil.
func Decide(policy Policy, a Assessment, secrets *Secrets, now time.Time) Decision {
	decision := Decision{Assessment: a}
	for _, step := range policy.Steps {
		threshold := step.MinScore
		if secrets != nil {
			// Perturbed in log-odds, so a threshold near 1 stays below 1.
			threshold = sigmoid(logit(threshold) + 0.3*(2*secrets.Uniform(a.Subject.Tenant, "step:"+step.Action.String(), now)-1))
		}
		if a.Score >= threshold && len(a.Reasons) >= step.MinReasons && step.Action > decision.Proposed {
			decision.Proposed = step.Action
		}
	}
	applied := decision.Proposed
	if policy.HumanFrom != ActionNone && applied >= policy.HumanFrom {
		decision.NeedsReview = true
		applied = min(applied, policy.AutoCeiling)
	}
	if a.Subject.Room != "" && policy.PublisherCeiling != ActionNone && isPublisher(a) {
		if applied > policy.PublisherCeiling {
			decision.NeedsReview = true
			applied = policy.PublisherCeiling
		}
	}
	if policy.Shadow {
		decision.Shadow = true
		applied = min(applied, ActionShadowFlag)
	}
	decision.Applied = applied
	decision.Exclude = applied >= ActionExclude
	if applied > ActionShadowFlag && policy.MaxDelay > 0 {
		u := 0.5
		if secrets != nil {
			u = secrets.Uniform(a.Subject.Tenant, "delay:"+a.Subject.Key()+now.String(), now)
		}
		decision.NotBefore = now.Add(time.Duration(u * float64(policy.MaxDelay)))
	} else {
		decision.NotBefore = now
	}
	return decision
}

func isPublisher(a Assessment) bool { return a.publisher }

// Enforcement is one action to carry out against one participant.
type Enforcement struct {
	Subject Subject
	Action  Action
	// Code is what the participant is told: always the generic tenant-policy
	// code, so that the reason for an action is not a hint for evading it.
	Code   string
	Reason string
	Model  string
	Actor  string
}

// SubjectCode is the moderation code shown to an enforced-against participant.
const SubjectCode = "tenant_policy"

// Enforcer carries out actions. The control plane's implementation resolves
// the pseudonymous subject to a live session and calls the same moderation
// path a human moderator uses, so kicks and bans propagate over the relay link
// and land in moderation_log with DecidedByModel set.
type Enforcer interface {
	Apply(ctx context.Context, e Enforcement) error
	Lift(ctx context.Context, e Enforcement) error
}

// AuditEntry is one line of the trust audit trail.
type AuditEntry struct {
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"at"`
	Tenant  string    `json:"tenant"`
	Subject string    `json:"subject"`
	Event   string    `json:"event"`
	Action  string    `json:"action,omitempty"`
	Actor   string    `json:"actor,omitempty"`
	Model   string    `json:"model,omitempty"`
	Score   float64   `json:"score,omitempty"`
	Reasons []string  `json:"reasons,omitempty"`
	Note    string    `json:"note,omitempty"`
	Prev    string    `json:"prev"`
	Hash    string    `json:"hash"`
}

// AuditLog is append-only and hash-chained, so a deleted or edited line breaks
// every hash after it. Production persists it beside moderation_log, under the
// same protection from the retention sweep.
type AuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
}

// Append adds an entry and returns it with its sequence and hash.
func (l *AuditLog) Append(e AuditEntry) AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = uint64(len(l.entries)) + 1
	if n := len(l.entries); n > 0 {
		e.Prev = l.entries[n-1].Hash
	}
	e.Hash = ""
	body, _ := json.Marshal(e)
	sum := sha256.Sum256(body)
	e.Hash = hex.EncodeToString(sum[:])
	l.entries = append(l.entries, e)
	return e
}

// Entries returns a copy of the log.
func (l *AuditLog) Entries() []AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]AuditEntry(nil), l.entries...)
}

// Verify checks the chain.
func (l *AuditLog) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := ""
	for _, e := range l.entries {
		hash := e.Hash
		e.Hash = ""
		if e.Prev != prev {
			return fmt.Errorf("audit entry %d does not follow its predecessor", e.Seq)
		}
		body, _ := json.Marshal(e)
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != hash {
			return fmt.Errorf("audit entry %d was altered", e.Seq)
		}
		prev = hash
	}
	return nil
}

func reasonCodes(a Assessment) []string {
	out := make([]string, 0, len(a.Reasons))
	for _, r := range a.Reasons {
		out = append(out, string(r.Code))
	}
	return out
}

// CaseKind distinguishes a review from an appeal.
type CaseKind string

const (
	CaseReview CaseKind = "review"
	CaseAppeal CaseKind = "appeal"
)

// Case is one item in the human queue.
type Case struct {
	ID        string
	Kind      CaseKind
	Decision  Decision
	Opened    time.Time
	Statement string
	// Original is the review an appeal is against.
	Original string
	Resolved bool
	Reviewer string
	Bot      bool
	Applied  Action
}

// Errors from the queue.
var (
	ErrNoCase      = errors.New("no such case")
	ErrResolved    = errors.New("case already resolved")
	ErrSameEyes    = errors.New("an appeal must be decided by someone other than the original reviewer")
	ErrNotAppealed = errors.New("only an enforced, reviewed case can be appealed")
)

// Queue is the human review queue and the appeal path.
//
// Every automated action at or above a tenant's HumanFrom waits here, and so
// does every appeal. A reviewer's verdict is the only way a label reaches the
// learner with full weight, and the only way a device enters the cross-tenant
// index; an appeal is decided by a different reviewer from the one who made the
// original call.
type Queue struct {
	mu       sync.Mutex
	cases    map[string]*Case
	next     int
	Detector *Detector
	Enforcer Enforcer
	Audit    *AuditLog
	// Exclusions is updated when a decision applies or lifts exclusion.
	Exclusions *Exclusions
}

// Process applies a decision now: audits it, applies what may be applied
// automatically, and opens a review if one is needed. It returns the case ID
// if one was opened.
func (q *Queue) Process(ctx context.Context, d Decision) (string, error) {
	a := d.Assessment
	if d.Proposed == ActionNone {
		return "", nil
	}
	q.Audit.Append(AuditEntry{
		At: a.At, Tenant: a.Subject.Tenant, Subject: a.Subject.Key(), Event: "decision",
		Action: d.Applied.String(), Actor: "model", Model: a.Model, Score: a.Score, Reasons: reasonCodes(a),
		Note: fmt.Sprintf("proposed=%s shadow=%t review=%t", d.Proposed, d.Shadow, d.NeedsReview),
	})
	if d.Exclude && q.Exclusions != nil {
		q.Exclusions.Set(a.Subject, true)
	}
	if d.Applied > ActionShadowFlag && q.Enforcer != nil {
		if err := q.Enforcer.Apply(ctx, Enforcement{
			Subject: a.Subject, Action: d.Applied, Code: SubjectCode,
			Reason: strings.Join(reasonCodes(a), ","), Model: a.Model, Actor: "model",
		}); err != nil {
			return "", err
		}
	}
	if d.NeedsReview {
		return q.open(CaseReview, d, a.At, "", ""), nil
	}
	if d.Applied <= ActionShadowFlag && !d.Exclude {
		return "", nil
	}
	// Anything that affected the participant must be appealable, so an
	// automatic action is recorded as a case the model already decided.
	id := q.open(CaseReview, d, a.At, "", "")
	q.mu.Lock()
	c := q.cases[id]
	c.Resolved, c.Reviewer, c.Bot, c.Applied = true, "model", true, d.Applied
	q.mu.Unlock()
	return id, nil
}

func (q *Queue) open(kind CaseKind, d Decision, at time.Time, statement, original string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cases == nil {
		q.cases = map[string]*Case{}
	}
	q.next++
	id := fmt.Sprintf("%s-%d", kind, q.next)
	q.cases[id] = &Case{ID: id, Kind: kind, Decision: d, Opened: at, Statement: statement, Original: original}
	return id
}

// Pending lists open cases, appeals first and then oldest first.
func (q *Queue) Pending() []Case {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Case
	for _, c := range q.cases {
		if !c.Resolved {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == CaseAppeal
		}
		return out[i].Opened.Before(out[j].Opened)
	})
	return out
}

// Resolve records a reviewer's verdict on a review. If bot, action is applied
// (it may be lower than proposed); if not, any automatic action is lifted.
func (q *Queue) Resolve(ctx context.Context, id, reviewer string, bot bool, action Action, now time.Time) error {
	q.mu.Lock()
	c := q.cases[id]
	switch {
	case c == nil:
		q.mu.Unlock()
		return ErrNoCase
	case c.Resolved:
		q.mu.Unlock()
		return ErrResolved
	case c.Kind == CaseAppeal:
		q.mu.Unlock()
		return q.decideAppeal(ctx, id, reviewer, !bot, now)
	}
	c.Resolved, c.Reviewer, c.Bot = true, reviewer, bot
	if !bot {
		action = ActionNone
	}
	c.Applied = action
	d := c.Decision
	q.mu.Unlock()

	s := d.Assessment.Subject
	if q.Detector != nil {
		q.Detector.Feedback(s, bot, reviewer, now)
	}
	if bot && action > d.Applied && q.Enforcer != nil {
		if err := q.Enforcer.Apply(ctx, Enforcement{Subject: s, Action: action, Code: SubjectCode, Reason: "reviewed", Model: d.Assessment.Model, Actor: reviewer}); err != nil {
			return err
		}
	}
	if !bot {
		q.lift(ctx, d, reviewer)
	}
	if q.Exclusions != nil {
		q.Exclusions.Set(s, bot && action >= ActionExclude)
	}
	q.Audit.Append(AuditEntry{At: now, Tenant: s.Tenant, Subject: s.Key(), Event: "review", Action: action.String(), Actor: reviewer, Model: d.Assessment.Model, Note: fmt.Sprintf("bot=%t case=%s", bot, id)})
	return nil
}

func (q *Queue) lift(ctx context.Context, d Decision, actor string) {
	if q.Enforcer != nil && d.Applied > ActionShadowFlag {
		_ = q.Enforcer.Lift(ctx, Enforcement{Subject: d.Assessment.Subject, Action: d.Applied, Code: SubjectCode, Model: d.Assessment.Model, Actor: actor})
	}
}

// Appeal opens an appeal against a resolved review. The statement is the
// participant's, relayed by the tenant; it is shown to the reviewer verbatim.
func (q *Queue) Appeal(original, statement string, now time.Time) (string, error) {
	q.mu.Lock()
	c := q.cases[original]
	if c == nil {
		q.mu.Unlock()
		return "", ErrNoCase
	}
	if !c.Resolved || !c.Bot {
		q.mu.Unlock()
		return "", ErrNotAppealed
	}
	d := c.Decision
	d.Applied = c.Applied
	q.mu.Unlock()
	id := q.open(CaseAppeal, d, now, statement, original)
	q.Audit.Append(AuditEntry{At: now, Tenant: d.Assessment.Subject.Tenant, Subject: d.Assessment.Subject.Key(), Event: "appeal_opened", Note: "against " + original})
	return id, nil
}

func (q *Queue) decideAppeal(ctx context.Context, id, reviewer string, overturn bool, now time.Time) error {
	q.mu.Lock()
	c := q.cases[id]
	original := q.cases[c.Original]
	if original != nil && original.Reviewer == reviewer {
		q.mu.Unlock()
		return ErrSameEyes
	}
	c.Resolved, c.Reviewer, c.Bot = true, reviewer, !overturn
	d := c.Decision
	if original != nil && original.Applied > d.Applied {
		// The review may have escalated past the automatic action; lift what
		// is actually in force.
		d.Applied = original.Applied
	}
	q.mu.Unlock()

	s := d.Assessment.Subject
	if overturn {
		q.lift(ctx, d, reviewer)
		if q.Exclusions != nil {
			q.Exclusions.Set(s, false)
		}
		if q.Detector != nil {
			q.Detector.Feedback(s, false, reviewer, now)
		}
	}
	q.Audit.Append(AuditEntry{At: now, Tenant: s.Tenant, Subject: s.Key(), Event: "appeal_decided", Actor: reviewer, Note: fmt.Sprintf("overturned=%t case=%s", overturn, id)})
	return nil
}
