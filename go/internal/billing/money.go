// Package billing is how MOQOM Cloud gets paid.
//
// Prepaid only: a tenant buys credit through Stripe, usage draws it down, and an
// empty balance stops new sessions from being admitted. Nothing here extends
// credit, so nothing here can be owed money it will never collect.
//
// Every charge is cost-plus — what Google Cloud charges us for the bytes, plus a
// markup, plus a platform fee — and then capped at a ceiling, which by default
// is Agora's list price less one cent. The ledger records our cost beside every
// price, so the margin is a query rather than an estimate.
package billing

import "fmt"

// Micros is an amount of US dollars in millionths.
//
// Integer rather than float because a ledger that cannot add two amounts and get
// the same answer twice is not a ledger. A millionth of a dollar is fine enough
// that rating a single participant-second at any realistic price rounds to
// something meaningful.
type Micros int64

const (
	Cent   Micros = 10_000
	Dollar Micros = 1_000_000
)

// CentsToMicros converts an amount Stripe reports.
func CentsToMicros(cents int64) Micros { return Micros(cents) * Cent }

// Cents is the amount in whole cents, rounded toward zero. Stripe charges in
// cents, so this is what a top-up of m actually costs the customer.
func (m Micros) Cents() int64 { return int64(m / Cent) }

func (m Micros) String() string {
	sign := ""
	value := int64(m)
	if value < 0 {
		sign = "-"
		value = -value
	}
	return fmt.Sprintf("%s$%d.%06d", sign, value/int64(Dollar), value%int64(Dollar))
}
