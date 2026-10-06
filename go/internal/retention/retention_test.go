package retention

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var now = time.Date(2026, 10, 6, 3, 30, 0, 0, time.UTC)

type call struct {
	sql  string
	args []any
}

// fakeDB records statements and answers from canned values.
type fakeDB struct {
	calls   []call
	missing map[string]bool
	// affected is returned per Exec in order, per table; once exhausted, 0.
	affected map[string][]int64
	failOn   string
	count    int64
}

type row struct {
	value any
	err   error
}

func (r row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch d := dest[0].(type) {
	case *bool:
		*d = r.value.(bool)
	case *int64:
		*d = r.value.(int64)
	}
	return nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls = append(f.calls, call{sql, args})
	if strings.HasPrefix(sql, "SELECT to_regclass") {
		return row{value: !f.missing[args[0].(string)]}
	}
	return row{value: f.count}
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls = append(f.calls, call{sql, args})
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return pgconn.CommandTag{}, errors.New("permission denied")
	}
	for table, queue := range f.affected {
		if strings.Contains(sql, `"`+table+`"`) && len(queue) > 0 {
			f.affected[table] = queue[1:]
			return pgconn.NewCommandTag("DELETE " + itoa(queue[0])), nil
		}
	}
	return pgconn.NewCommandTag("DELETE 0"), nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

func result(t *testing.T, report Report, name string) Result {
	t.Helper()
	for _, r := range report.Results {
		if r.Rule == name {
			return r
		}
	}
	t.Fatalf("no result for %s", name)
	return Result{}
}

func TestCutoffsFollowThePolicy(t *testing.T) {
	db := &fakeDB{}
	report := (&Sweeper{DB: db, Policy: DefaultPolicy(), Now: func() time.Time { return now }}).Run(context.Background())
	want := map[string]time.Duration{
		"participant_addresses": 30 * day,
		"departed_participants": 400 * day,
		"expired_bans":          90 * day,
		"expired_mutes":         30 * day,
		"revoked_api_keys":      400 * day,
		"expired_api_keys":      400 * day,
		"api_key_use":           400 * day,
		"finished_billing_jobs": 400 * day,
	}
	if len(report.Results) != len(want) {
		t.Fatalf("%d results", len(report.Results))
	}
	for name, age := range want {
		r := result(t, report, name)
		if !r.Cutoff.Equal(now.Add(-age)) || r.Error != "" || r.Skipped != "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	for _, c := range db.calls {
		if strings.HasPrefix(c.sql, "DELETE") || strings.HasPrefix(c.sql, "UPDATE") {
			if len(c.args) != 1 {
				t.Errorf("%s: args %v", c.sql, c.args)
			}
			if strings.Contains(c.sql, "now()") {
				t.Errorf("uses the database clock: %s", c.sql)
			}
		}
	}
}

func TestAddressesAreScrubbedNotDeleted(t *testing.T) {
	db := &fakeDB{}
	(&Sweeper{DB: db, Policy: Policy{ParticipantAddress: day}, Now: func() time.Time { return now }}).Run(context.Background())
	var statements []string
	for _, c := range db.calls {
		if !strings.HasPrefix(c.sql, "SELECT") {
			statements = append(statements, c.sql)
		}
	}
	want := `UPDATE "participants" SET remote_addr = '' WHERE ctid IN (SELECT ctid FROM "participants" WHERE remote_addr <> '' AND last_seen_at < $1 LIMIT 5000)`
	if len(statements) != 1 || statements[0] != want {
		t.Fatalf("statements:\n%s", strings.Join(statements, "\n"))
	}
}

func TestDisabledRulesAndMissingTablesAreSkipped(t *testing.T) {
	db := &fakeDB{missing: map[string]bool{"billing_keys": true, "billing_jobs": true}}
	p := DefaultPolicy()
	p.ExpiredMute = 0
	report := (&Sweeper{DB: db, Policy: p, Now: func() time.Time { return now }}).Run(context.Background())
	if r := result(t, report, "expired_mutes"); r.Skipped != "disabled" {
		t.Fatalf("mutes: %+v", r)
	}
	if r := result(t, report, "revoked_api_keys"); r.Skipped != "table does not exist" {
		t.Fatalf("keys: %+v", r)
	}
	for _, c := range db.calls {
		if strings.Contains(c.sql, `"billing_keys"`) || strings.Contains(c.sql, `"mutes"`) {
			t.Fatalf("touched a skipped table: %s", c.sql)
		}
	}
	if report.Failed() {
		t.Fatal("a skip is not a failure")
	}
}

func TestBatchesUntilDrained(t *testing.T) {
	db := &fakeDB{affected: map[string][]int64{"bans": {2, 2, 1}}}
	report := (&Sweeper{DB: db, Policy: Policy{ExpiredBan: day}, BatchSize: 2, Now: func() time.Time { return now }}).Run(context.Background())
	if r := result(t, report, "expired_bans"); r.Affected != 5 {
		t.Fatalf("bans: %+v", r)
	}

	db = &fakeDB{affected: map[string][]int64{"bans": {2, 2, 2, 2}}}
	report = (&Sweeper{DB: db, Policy: Policy{ExpiredBan: day}, BatchSize: 2, MaxBatches: 3, Now: func() time.Time { return now }}).Run(context.Background())
	if r := result(t, report, "expired_bans"); r.Affected != 6 {
		t.Fatalf("batch limit: %+v", r)
	}
}

func TestOneFailureDoesNotStopTheRest(t *testing.T) {
	db := &fakeDB{failOn: `"bans"`, affected: map[string][]int64{"mutes": {3}}}
	report := (&Sweeper{DB: db, Policy: DefaultPolicy(), Now: func() time.Time { return now }}).Run(context.Background())
	if r := result(t, report, "expired_bans"); r.Error == "" {
		t.Fatal("ban failure not reported")
	}
	if r := result(t, report, "expired_mutes"); r.Affected != 3 || r.Error != "" {
		t.Fatalf("mutes: %+v", r)
	}
	if !report.Failed() {
		t.Fatal("report should be failed")
	}
}

func TestDryRunOnlyCounts(t *testing.T) {
	db := &fakeDB{count: 7}
	report := (&Sweeper{DB: db, Policy: DefaultPolicy(), DryRun: true, Now: func() time.Time { return now }}).Run(context.Background())
	for _, c := range db.calls {
		if !strings.HasPrefix(c.sql, "SELECT") {
			t.Fatalf("dry run changed something: %s", c.sql)
		}
	}
	if r := result(t, report, "revoked_api_keys"); r.Affected != 7 || !report.DryRun {
		t.Fatalf("%+v", r)
	}
}

// The ledger is a financial record and the moderation log is evidence; no
// rule may touch either, however it is phrased.
func TestProtectedTablesAreUntouchable(t *testing.T) {
	for _, rule := range Rules {
		for _, protected := range Protected {
			if rule.Table == protected || strings.Contains(rule.Where+rule.Set, protected) {
				t.Errorf("rule %s touches %s", rule.Name, protected)
			}
		}
	}

	saved := Rules
	defer func() { Rules = saved }()
	Rules = []Rule{{Name: "oops", Table: "billing_ledger", Where: "at < $1", Age: func(Policy) time.Duration { return day }}}
	db := &fakeDB{}
	report := (&Sweeper{DB: db, Policy: DefaultPolicy(), Now: func() time.Time { return now }}).Run(context.Background())
	if r := report.Results[0]; !strings.Contains(r.Error, "protected") || len(db.calls) != 0 {
		t.Fatalf("protected table reached: %+v, %d calls", r, len(db.calls))
	}
}
