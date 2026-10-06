// Package byoc records which overlay identities are customers' own relays, and
// for which tenant.
//
// A relay a customer runs ("bring your own compute") reaches the control plane
// the same way the managed fleet does -- RelayLink -- but over the OpenZiti
// overlay and with no credential of its own: the overlay identity it enrolled
// is the credential, and this record is what turns that identity into a relay
// account scoped to one tenant. Revoking the record and deleting the identity
// together is what stops a customer's relay: the overlay refuses its circuits,
// and a stream that was already up is cut.
package byoc

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// Enrollment is one customer relay identity.
type Enrollment struct {
	// ID is this record's identifier, which the admin API names it by.
	ID string
	// Tenant is the only tenant the relay may serve.
	Tenant string
	// Name is the customer's label for the relay.
	Name string
	// IdentityID and IdentityName are the overlay identity. The name is what
	// arrives with every connection, so it is what a stream is matched on.
	IdentityID   string
	IdentityName string
	// CreatedBy is the account that asked for it.
	CreatedBy string
	CreatedAt time.Time
	// RevokedAt is zero while the enrolment is in force.
	RevokedAt time.Time
}

// Active reports whether the enrolment still admits its relay.
func (e Enrollment) Active() bool { return e.RevokedAt.IsZero() }

// Errors a store returns.
var (
	ErrNotFound = errors.New("no such enrolment")
	ErrExists   = errors.New("enrolment already exists")
)

// Store keeps enrolments.
type Store interface {
	// Create adds an enrolment. ErrExists if the ID or the identity name is
	// already recorded, revoked or not.
	Create(ctx context.Context, enrollment Enrollment) error
	// Get returns one by ID, revoked or not.
	Get(ctx context.Context, id string) (Enrollment, error)
	// ByIdentity returns one by overlay identity name, revoked or not.
	ByIdentity(ctx context.Context, identityName string) (Enrollment, error)
	// List returns a tenant's enrolments, newest first, revoked ones included.
	List(ctx context.Context, tenant string) ([]Enrollment, error)
	// Revoke marks one revoked at the given time and returns it. Revoking a
	// revoked enrolment keeps the first time and is not an error.
	Revoke(ctx context.Context, id string, at time.Time) (Enrollment, error)
}

// Memory is a Store in memory, for a single replica and for tests.
type Memory struct {
	mu   sync.Mutex
	byID map[string]Enrollment
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{byID: map[string]Enrollment{}}
}

func (m *Memory) Create(_ context.Context, enrollment Enrollment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[enrollment.ID]; ok {
		return ErrExists
	}
	for _, existing := range m.byID {
		if existing.IdentityName == enrollment.IdentityName {
			return ErrExists
		}
	}
	m.byID[enrollment.ID] = enrollment
	return nil
}

func (m *Memory) Get(_ context.Context, id string) (Enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	enrollment, ok := m.byID[id]
	if !ok {
		return Enrollment{}, ErrNotFound
	}
	return enrollment, nil
}

func (m *Memory) ByIdentity(_ context.Context, identityName string) (Enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, enrollment := range m.byID {
		if enrollment.IdentityName == identityName {
			return enrollment, nil
		}
	}
	return Enrollment{}, ErrNotFound
}

func (m *Memory) List(_ context.Context, tenant string) ([]Enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var listed []Enrollment
	for _, enrollment := range m.byID {
		if enrollment.Tenant == tenant {
			listed = append(listed, enrollment)
		}
	}
	slices.SortFunc(listed, func(a, b Enrollment) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return compareStrings(b.ID, a.ID)
	})
	return listed, nil
}

func (m *Memory) Revoke(_ context.Context, id string, at time.Time) (Enrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	enrollment, ok := m.byID[id]
	if !ok {
		return Enrollment{}, ErrNotFound
	}
	if enrollment.RevokedAt.IsZero() {
		enrollment.RevokedAt = at
		m.byID[id] = enrollment
	}
	return enrollment, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
