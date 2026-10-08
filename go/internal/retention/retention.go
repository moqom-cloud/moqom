// Package retention enforces the storage-limitation periods in
// docs/compliance/DATA-RETENTION.md against the control plane's database.
//
// It deletes or scrubs rows that have outlived their purpose, and nothing
// else. Three tables are out of its reach on purpose and a test holds it to
// that: billing_ledger and billing_tenants are financial records with a
// seven-year statutory retention, and moderation_log is the evidence behind
// every enforcement decision a tenant may have to defend. Their retention is a
// documented manual decision, not something a cron job gets to make.
//
// Every rule is judged against a cutoff computed from the caller's clock,
// never from the database's now(), for the same reason the store does it: the
// test that advances a fake clock has to see the effect.
package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the slice of a pgx pool the sweep uses.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Policy is how long each class of row is kept past the event that ends its
// usefulness. A zero disables that rule.
type Policy struct {
	// ParticipantAddress is how long a participant's IP address is kept after
	// they were last seen. Moderation needs it while a room is live and for a
	// short time after (an IP ban is issued from it); past that it is only a
	// liability.
	ParticipantAddress time.Duration
	// DepartedParticipant is how long a participant row survives after the
	// participant left. Long enough to answer "who was in this room" for an
	// abuse report or an appeal; the moderation log keeps anything that was
	// acted on.
	DepartedParticipant time.Duration
	// ExpiredBan and ExpiredMute are how long an expired ban or mute is kept.
	// Expired rows are already ignored by every read, so deleting them changes
	// nothing about enforcement. Bans can carry an IP prefix.
	ExpiredBan  time.Duration
	ExpiredMute time.Duration
	// RevokedKey is how long a revoked API key's row (name, hash, dates) is
	// kept. Longer than an audit period, so access reviews can see who held a
	// key during the window under review.
	RevokedKey time.Duration
	// ExpiredKey is the same for a key that reached its expiry unrevoked.
	ExpiredKey time.Duration
	// KeyUse is how long the per-minute API key audit (key ID, operation,
	// source address, outcome) is kept after its last use. An audit period,
	// for the same access reviews; it holds backend addresses, so no longer.
	KeyUse time.Duration
	// FinishedJob is how long a completed provisioning job is kept.
	FinishedJob time.Duration
}

const day = 24 * time.Hour

// DefaultPolicy is the schedule in DATA-RETENTION.md.
func DefaultPolicy() Policy {
	return Policy{
		ParticipantAddress:  30 * day,
		DepartedParticipant: 400 * day,
		ExpiredBan:          90 * day,
		ExpiredMute:         30 * day,
		RevokedKey:          400 * day,
		ExpiredKey:          400 * day,
		KeyUse:              400 * day,
		FinishedJob:         400 * day,
	}
}

// Rule is one retention action on one table.
type Rule struct {
	Name  string
	Table string
	// Where selects expired rows; $1 is the cutoff.
	Where string
	// Set, when non-empty, scrubs the selected rows instead of deleting them.
	Set string
	Age func(Policy) time.Duration
}

// Rules is every rule the sweep runs, in order.
var Rules = []Rule{
	{
		Name: "participant_addresses", Table: "participants",
		Where: "remote_addr <> '' AND last_seen_at < $1",
		Set:   "remote_addr = ''",
		Age:   func(p Policy) time.Duration { return p.ParticipantAddress },
	},
	{
		Name: "departed_participants", Table: "participants",
		Where: "presence = 'left' AND last_seen_at < $1",
		Age:   func(p Policy) time.Duration { return p.DepartedParticipant },
	},
	{
		Name: "expired_bans", Table: "bans",
		Where: "expires_at IS NOT NULL AND expires_at < $1",
		Age:   func(p Policy) time.Duration { return p.ExpiredBan },
	},
	{
		Name: "expired_mutes", Table: "mutes",
		Where: "expires_at IS NOT NULL AND expires_at < $1",
		Age:   func(p Policy) time.Duration { return p.ExpiredMute },
	},
	{
		Name: "revoked_api_keys", Table: "billing_keys",
		Where: "revoked_at IS NOT NULL AND revoked_at < $1",
		Age:   func(p Policy) time.Duration { return p.RevokedKey },
	},
	{
		// Its own rule, so a database whose billing schema predates expiry
		// fails this one and not the revoked-key sweep beside it.
		Name: "expired_api_keys", Table: "billing_keys",
		Where: "revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at < $1",
		Age:   func(p Policy) time.Duration { return p.ExpiredKey },
	},
	{
		Name: "api_key_use", Table: "billing_key_use",
		Where: "last_at < $1",
		Age:   func(p Policy) time.Duration { return p.KeyUse },
	},
	{
		Name: "finished_billing_jobs", Table: "billing_jobs",
		Where: "completed_at IS NOT NULL AND completed_at < $1",
		Age:   func(p Policy) time.Duration { return p.FinishedJob },
	},
}

// Protected are tables no rule may touch. Checked at sweep time as well as in
// tests, so a rule added carelessly fails loudly instead of deleting a ledger.
var Protected = []string{"billing_ledger", "billing_tenants", "moderation_log"}

// Result is what one rule did.
type Result struct {
	Rule     string    `json:"rule"`
	Table    string    `json:"table"`
	Cutoff   time.Time `json:"cutoff"`
	Affected int64     `json:"affected"`
	// Skipped explains a rule that did not run: disabled, or its table does
	// not exist (billing has never been switched on in this database).
	Skipped string `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Report is one sweep.
type Report struct {
	At      time.Time `json:"at"`
	DryRun  bool      `json:"dry_run"`
	Results []Result  `json:"results"`
}

// Failed reports whether any rule errored.
func (r Report) Failed() bool {
	for _, result := range r.Results {
		if result.Error != "" {
			return true
		}
	}
	return false
}

// Sweeper runs the rules.
type Sweeper struct {
	DB     DB
	Policy Policy
	Now    func() time.Time
	// DryRun counts what would change without changing it.
	DryRun bool
	// BatchSize bounds each statement, so a large backlog is drained in
	// short transactions rather than one long lock. Default 5000.
	BatchSize int
	// MaxBatches bounds one run per rule; the rest waits for tomorrow.
	// Default 200.
	MaxBatches int
}

// ErrProtected is a rule that targets a protected table.
var ErrProtected = errors.New("rule targets a protected table")

// Run executes every rule. One rule failing does not stop the others: an
// expired ban that cannot be deleted is no reason to keep an IP address.
func (s *Sweeper) Run(ctx context.Context) Report {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	at := now().UTC()
	report := Report{At: at, DryRun: s.DryRun}
	for _, rule := range Rules {
		report.Results = append(report.Results, s.apply(ctx, rule, at))
	}
	return report
}

func (s *Sweeper) apply(ctx context.Context, rule Rule, at time.Time) Result {
	result := Result{Rule: rule.Name, Table: rule.Table}
	for _, protected := range Protected {
		if rule.Table == protected || strings.Contains(rule.Where+rule.Set, protected) {
			result.Error = fmt.Sprintf("%v: %s", ErrProtected, protected)
			return result
		}
	}
	age := rule.Age(s.Policy)
	if age <= 0 {
		result.Skipped = "disabled"
		return result
	}
	result.Cutoff = at.Add(-age)

	var exists bool
	if err := s.DB.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", rule.Table).Scan(&exists); err != nil {
		result.Error = "checking the table exists: " + err.Error()
		return result
	}
	if !exists {
		result.Skipped = "table does not exist"
		return result
	}

	table := pgx.Identifier{rule.Table}.Sanitize()
	if s.DryRun {
		err := s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+rule.Where, result.Cutoff).Scan(&result.Affected)
		if err != nil {
			result.Error = err.Error()
		}
		return result
	}

	batch, batches := s.BatchSize, s.MaxBatches
	if batch <= 0 {
		batch = 5000
	}
	if batches <= 0 {
		batches = 200
	}
	selection := fmt.Sprintf("ctid IN (SELECT ctid FROM %s WHERE %s LIMIT %d)", table, rule.Where, batch)
	statement := "DELETE FROM " + table + " WHERE " + selection
	if rule.Set != "" {
		statement = "UPDATE " + table + " SET " + rule.Set + " WHERE " + selection
	}
	for i := 0; i < batches; i++ {
		tag, err := s.DB.Exec(ctx, statement, result.Cutoff)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Affected += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return result
		}
	}
	return result
}
