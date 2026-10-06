package billing

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Status is where a tenant is in its lifecycle.
//
// The same lifecycle as the heirloom Totum registry, so the two can share a
// tenancy SDK later: suspension is reversible and preserves everything,
// archiving is read-only, deletion starts a teardown after a retention period.
type Status string

const (
	// StatusPending has signed up and not yet paid. Keys exist and can
	// administer, but nothing that consumes media is admitted.
	StatusPending   Status = "pending"
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusArchived  Status = "archived"
	StatusDeleted   Status = "deleted"
)

var transitions = map[Status][]Status{
	StatusPending:   {StatusActive, StatusDeleted},
	StatusActive:    {StatusSuspended, StatusArchived, StatusDeleted},
	StatusSuspended: {StatusActive, StatusArchived, StatusDeleted},
	StatusArchived:  {StatusDeleted},
}

// ErrTransition is a lifecycle change the registry does not allow.
var ErrTransition = errors.New("tenant status cannot change that way")

// CanBecome reports whether from may change to to.
func CanBecome(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Tenant is one customer of MOQOM Cloud.
//
// The tenant ID is the same string the control plane scopes every room, token
// and moderation entry by, so isolation is the row-level kind the control plane
// already enforces: every table is keyed by tenant and every service checks the
// caller's tenant before it looks anything up.
type Tenant struct {
	ID     string
	Name   string
	Email  string
	Status Status

	StripeCustomer string
	// PaymentMethod is the card saved at the first top-up, used for automatic
	// top-ups off session.
	PaymentMethod string

	AutoTopUp AutoTopUp
	// TopUpPendingSince is set while an automatic top-up is in flight, so a
	// burst of usage reports below the threshold charges the card once.
	TopUpPendingSince time.Time
	LastTopUpError    string

	// Spending is written only by Store.SetSpending. SaveTenant leaves it
	// alone, so a webhook rewriting the tenant cannot undo a customer's change.
	Spending Spending

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Spending is a tenant's optional monthly spend limit and the alerts that warn
// of it. A month is a calendar month in UTC, and spend is usage charged in it.
type Spending struct {
	// Limit refuses new sessions once this month's spend reaches it. Zero is
	// no limit, which is the default.
	Limit Micros
	// Alerts are monthly spend amounts that each send one notice a month.
	// Empty means DefaultAlerts of the limit, if there is one.
	Alerts []Micros
	// AlertsOff silences every alert, the defaults included.
	AlertsOff bool
}

// Bounds on what a tenant may configure. Spend amounts are whole cents.
const (
	MinSpendAmount  Micros = Dollar
	MaxSpendAmount  Micros = 1_000_000 * Dollar
	mostSpendAlerts        = 10
)

// DefaultAlerts are the percentages of a limit that alert when a tenant has a
// limit and has not chosen its own thresholds.
var DefaultAlerts = []int64{50, 80, 100}

// Thresholds are the amounts that alert this month, ascending.
func (s Spending) Thresholds() []Micros {
	if s.AlertsOff {
		return nil
	}
	if len(s.Alerts) > 0 {
		sorted := slices.Clone(s.Alerts)
		slices.Sort(sorted)
		return slices.Compact(sorted)
	}
	if s.Limit <= 0 {
		return nil
	}
	thresholds := make([]Micros, 0, len(DefaultAlerts))
	for _, percent := range DefaultAlerts {
		thresholds = append(thresholds, s.Limit*Micros(percent)/100)
	}
	return thresholds
}

// Validate checks a spending configuration against the bounds.
func (s Spending) Validate() error {
	if s.Limit != 0 && !validSpendAmount(s.Limit) {
		return fmt.Errorf("a spend limit must be whole cents between %s and %s", MinSpendAmount, MaxSpendAmount)
	}
	if len(s.Alerts) > mostSpendAlerts {
		return fmt.Errorf("at most %d spend alerts", mostSpendAlerts)
	}
	seen := map[Micros]bool{}
	for _, threshold := range s.Alerts {
		if !validSpendAmount(threshold) {
			return fmt.Errorf("a spend alert must be whole cents between %s and %s", MinSpendAmount, MaxSpendAmount)
		}
		if seen[threshold] {
			return fmt.Errorf("spend alert %s is listed twice", threshold)
		}
		seen[threshold] = true
	}
	return nil
}

func validSpendAmount(m Micros) bool {
	return m >= MinSpendAmount && m <= MaxSpendAmount && m%Cent == 0
}

// MonthOf is the start of the UTC calendar month containing at.
func MonthOf(at time.Time) time.Time {
	utc := at.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// FiredAlert is a spend alert threshold that has already alerted this month.
type FiredAlert struct {
	Threshold Micros
	At        time.Time
}

// AutoTopUp buys Amount of credit whenever the balance falls below Below.
type AutoTopUp struct {
	Enabled bool
	Below   Micros
	Amount  Micros
}

// Validate checks a top-up configuration against the configured bounds.
func (a AutoTopUp) Validate(minimum, maximum Micros) error {
	if !a.Enabled {
		return nil
	}
	if a.Amount < minimum || a.Amount > maximum {
		return fmt.Errorf("auto top-up amount must be between %s and %s", minimum, maximum)
	}
	if a.Below < 0 {
		return errors.New("auto top-up threshold cannot be negative")
	}
	return nil
}
