// Command moqom-retention runs the data-retention sweep against the control
// plane's database: it scrubs participant IP addresses, removes departed
// participants, expired bans and mutes, and old revoked API keys, each after
// the period in docs/compliance/DATA-RETENTION.md. It never touches the
// billing ledger, tenant records, or the moderation log.
//
// It runs daily as a Kubernetes CronJob beside the control plane
// (deploy/modules/retention), as the control plane's own identity, because
// the database has no public address and the control plane's role is the one
// that owns these tables. It connects the same two ways the control plane does:
//
//	MOQOM_CONTROL_SQL_INSTANCE=project:region:instance PGUSER=... PGDATABASE=moqom
//	MOQOM_CONTROL_DATABASE_URL=postgres://...
//
// -dry-run counts what would change. Exit status 1 means at least one rule
// failed; the failure is logged as `retention_sweep_failed`, which the
// compliance module alerts on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/moqom/go/internal/retention"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: cloudLogging})))
	if err := run(); err != nil {
		slog.Error("retention sweep failed", "event", "retention_sweep_failed", "error", err.Error())
		os.Exit(1)
	}
}

func cloudLogging(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		level := a.Value.String()
		if level == "WARN" {
			level = "WARNING"
		}
		return slog.String("severity", level)
	case slog.MessageKey:
		return slog.Attr{Key: "message", Value: a.Value}
	}
	return a
}

func run() error {
	policy := retention.DefaultPolicy()
	dryRun := flag.Bool("dry-run", os.Getenv("MOQOM_RETENTION_DRY_RUN") == "true", "count rows that would change without changing them")
	days := func(name string, into *time.Duration, usage string) {
		flag.Func(name, usage+fmt.Sprintf(" (days, default %d, 0 disables)", int(into.Hours()/24)), func(value string) error {
			var n int
			if _, err := fmt.Sscanf(value, "%d", &n); err != nil || n < 0 {
				return errors.New("want a non-negative number of days")
			}
			*into = time.Duration(n) * 24 * time.Hour
			return nil
		})
	}
	days("participant-address-days", &policy.ParticipantAddress, "scrub participant IP addresses this long after last seen")
	days("departed-participant-days", &policy.DepartedParticipant, "delete departed participants this long after they left")
	days("expired-ban-days", &policy.ExpiredBan, "delete bans this long after they expired")
	days("expired-mute-days", &policy.ExpiredMute, "delete mutes this long after they expired")
	days("revoked-key-days", &policy.RevokedKey, "delete revoked API keys this long after revocation")
	days("expired-key-days", &policy.ExpiredKey, "delete unrevoked API keys this long after they expired")
	days("key-use-days", &policy.KeyUse, "delete API key audit entries this long after their last use")
	days("finished-job-days", &policy.FinishedJob, "delete completed billing jobs this long after completion")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	pool, closer, err := connect(ctx)
	if err != nil {
		return err
	}
	defer closer()

	report := (&retention.Sweeper{DB: pool, Policy: policy, DryRun: *dryRun}).Run(ctx)
	for _, result := range report.Results {
		level := slog.LevelInfo
		if result.Error != "" {
			level = slog.LevelError
		}
		slog.Log(ctx, level, "retention rule", "event", "retention_rule", "rule", result.Rule, "table", result.Table,
			"cutoff", result.Cutoff, "affected", result.Affected, "skipped", result.Skipped, "error", result.Error, "dry_run", report.DryRun)
	}
	if report.Failed() {
		return errors.New("one or more retention rules failed; see retention_rule entries")
	}
	slog.Info("retention sweep complete", "event", "retention_sweep", "dry_run", report.DryRun, "rules", len(report.Results))
	return nil
}

// connect mirrors the control plane's databaseConfig: Cloud SQL over IAM with
// no password anywhere, or a plain URL for anything else.
func connect(ctx context.Context) (*pgxpool.Pool, func(), error) {
	instance := os.Getenv("MOQOM_CONTROL_SQL_INSTANCE")
	if instance == "" {
		url := os.Getenv("MOQOM_CONTROL_DATABASE_URL")
		if url == "" {
			return nil, nil, errors.New("set MOQOM_CONTROL_SQL_INSTANCE (with PGUSER) or MOQOM_CONTROL_DATABASE_URL")
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			return nil, nil, err
		}
		return pool, pool.Close, nil
	}
	user := os.Getenv("PGUSER")
	if user == "" {
		return nil, nil, errors.New("MOQOM_CONTROL_SQL_INSTANCE needs PGUSER: the service account email without .gserviceaccount.com")
	}
	database := os.Getenv("PGDATABASE")
	if database == "" {
		database = "moqom"
	}
	settings, err := pgxpool.ParseConfig(fmt.Sprintf("user=%s dbname=%s sslmode=disable", user, database))
	if err != nil {
		return nil, nil, err
	}
	settings.MaxConns = 2
	dialer, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithIAMAuthN())
	if err != nil {
		return nil, nil, fmt.Errorf("building the Cloud SQL dialer: %w", err)
	}
	settings.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.Dial(ctx, instance, cloudsqlconn.WithPrivateIP())
	}
	pool, err := pgxpool.NewWithConfig(ctx, settings)
	if err != nil {
		_ = dialer.Close()
		return nil, nil, err
	}
	return pool, func() { pool.Close(); _ = dialer.Close() }, nil
}
