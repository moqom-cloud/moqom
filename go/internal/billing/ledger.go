package billing

import "time"

// EntryKind is why the balance moved.
type EntryKind string

const (
	KindTopUp        EntryKind = "top_up"
	KindUsage        EntryKind = "usage"
	KindRefund       EntryKind = "refund"
	KindSignupCredit EntryKind = "signup_credit"
	KindAdjustment   EntryKind = "adjustment"
)

// Entry is one movement of a tenant's balance.
//
// The ledger is append-only and the balance is its sum, so there is no running
// total that can disagree with the entries that produced it. Every entry has an
// idempotency key that is unique across the whole ledger — a Stripe payment
// intent, a relay's usage record — so a webhook delivered twice or a usage
// report retried after a timeout moves the balance once.
type Entry struct {
	ID     string
	Tenant string
	Kind   EntryKind
	// Amount is positive for credit and negative for a charge.
	Amount Micros
	// Cost is what the movement cost MOQOM, recorded for usage so margin is a
	// sum rather than a reconstruction.
	Cost   Micros
	Capped bool

	IdempotencyKey string
	// Reference names the outside thing this came from: a payment intent, a
	// refund, a relay session.
	Reference string
	Detail    string
	At        time.Time
}
