package billing

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
)

// Store is everything billing keeps.
//
// Separate from the control plane's store.Store because the two have no
// transaction in common: a usage charge does not need to commit with a ban, and
// keeping them apart is what lets this half be lifted into a tenancy SDK
// without dragging rooms and moderation with it.
type Store interface {
	CreateTenant(ctx context.Context, tenant Tenant) (Tenant, error)
	GetTenant(ctx context.Context, id string) (Tenant, error)
	SaveTenant(ctx context.Context, tenant Tenant) (Tenant, error)
	FindTenantByStripeCustomer(ctx context.Context, customer string) (Tenant, error)
	// CountTenants reports every tenant ever created, whatever its status
	// now, and how many of them are active.
	CountTenants(ctx context.Context) (total, active int64, err error)
	// ClaimTopUp marks an automatic top-up in flight, and reports false if one
	// already is and is younger than staleAfter. Atomic, so two replicas rating
	// the same tenant's usage at once charge its card once.
	ClaimTopUp(ctx context.Context, tenant string, now time.Time, staleAfter time.Duration) (bool, error)

	// SetSpending replaces a tenant's spend limit and alerts, the only write
	// that touches them.
	SetSpending(ctx context.Context, tenant string, spending Spending, at time.Time) (Tenant, error)
	// UsageSince is the usage charged to a tenant at or after since, as a
	// positive amount.
	UsageSince(ctx context.Context, tenant string, since time.Time) (Micros, error)
	// UsageBreakdown sums a tenant's usage entries at or after from and
	// before to, grouped by the role, tier and hosting their detail records,
	// ordered by those three.
	UsageBreakdown(ctx context.Context, tenant string, from, to time.Time) ([]UsageBucket, error)
	// ClaimSpendAlert records that threshold has alerted for month, and
	// reports false if it already had. Atomic, so replicas rating the same
	// tenant at once send one notice.
	ClaimSpendAlert(ctx context.Context, tenant string, month time.Time, threshold Micros, at time.Time) (bool, error)
	// ReleaseSpendAlert undoes a claim whose notice could not be delivered,
	// so a later report tries again.
	ReleaseSpendAlert(ctx context.Context, tenant string, month time.Time, threshold Micros) error
	// FiredSpendAlerts lists the thresholds that have alerted for month,
	// ascending.
	FiredSpendAlerts(ctx context.Context, tenant string, month time.Time) ([]FiredAlert, error)

	// ClaimDirectSeconds adds seconds to the direct time a tenant has used in
	// month and returns the total before them. Idempotent on key: a second
	// claim with the same key adds nothing and returns what the first did, so
	// a re-sent usage record is priced from the same place against the free
	// allowance. Atomic, so replicas pricing two records at once see
	// different totals.
	ClaimDirectSeconds(ctx context.Context, tenant string, month time.Time, key string, seconds int64) (before int64, err error)
	// ClaimCloudflareSeconds is ClaimDirectSeconds for participant time on
	// Cloudflare's relays, which has a free allowance of its own.
	ClaimCloudflareSeconds(ctx context.Context, tenant string, month time.Time, key string, seconds int64) (before int64, err error)

	// AppendEntry writes an entry unless one with the same idempotency key
	// exists, in which case it returns that one and false. The uniqueness is
	// the store's, because two replicas handling the same webhook at once can
	// only be resolved by whatever holds the constraint.
	AppendEntry(ctx context.Context, entry Entry) (Entry, bool, error)
	FindEntry(ctx context.Context, idempotencyKey string) (Entry, error)
	Balance(ctx context.Context, tenant string) (Micros, error)
	ListEntries(ctx context.Context, tenant string, limit int) ([]Entry, error)

	PutKey(ctx context.Context, key Key) error
	GetKey(ctx context.Context, id string) (Key, error)
	ListKeys(ctx context.Context, tenant string) ([]Key, error)
	RevokeKey(ctx context.Context, tenant, id string, at time.Time) error
	// RehashKey replaces a key's hash if it is still from, so replicas
	// moving the same key to a new pepper at once agree on the result. A key
	// whose hash has already moved is not an error.
	RehashKey(ctx context.Context, id, from, to, version string) error

	// RecordKeyUse adds counts to the key audit, merging entries for the same
	// key, minute, operation, address, outcome and detail.
	RecordKeyUse(ctx context.Context, uses []KeyUse) error
	// ListKeyUse is a tenant's key audit, newest first, for one key or, with
	// key empty, all of them.
	ListKeyUse(ctx context.Context, tenant, key string, limit int) ([]KeyUse, error)

	SaveJob(ctx context.Context, job Job) error
	GetJob(ctx context.Context, id string) (Job, error)
}

// Memory is a Store in a map, for tests and for a control plane run without
// Postgres. It forgets every balance on restart, which is why the control plane
// refuses to enable billing on it outside development.
type Memory struct {
	mu      sync.Mutex
	tenants map[string]Tenant
	entries []Entry
	byKey   map[string]int
	keys    map[string]Key
	jobs    map[string]Job
	alerts  map[alertKey]time.Time
	direct  map[directMonth]int64
	claims  map[string]int64
	cfUsed  map[directMonth]int64
	cfClaim map[string]int64
	audit   []AuditEntry
	keyUse  []KeyUse
}

// mostKeyUseInMemory bounds Memory's key audit, which has no retention sweep:
// the oldest entries go first.
const mostKeyUseInMemory = 50_000

type directMonth struct {
	tenant string
	month  time.Time
}

type alertKey struct {
	tenant    string
	month     time.Time
	threshold Micros
}

func NewMemory() *Memory {
	return &Memory{
		tenants: map[string]Tenant{},
		byKey:   map[string]int{},
		keys:    map[string]Key{},
		jobs:    map[string]Job{},
		alerts:  map[alertKey]time.Time{},
		direct:  map[directMonth]int64{},
		claims:  map[string]int64{},
		cfUsed:  map[directMonth]int64{},
		cfClaim: map[string]int64{},
	}
}

func (m *Memory) ClaimCloudflareSeconds(_ context.Context, tenant string, month time.Time, key string, seconds int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if before, claimed := m.cfClaim[key]; claimed {
		return before, nil
	}
	which := directMonth{tenant, MonthOf(month)}
	before := m.cfUsed[which]
	m.cfUsed[which] = before + seconds
	m.cfClaim[key] = before
	return before, nil
}

func (m *Memory) ClaimDirectSeconds(_ context.Context, tenant string, month time.Time, key string, seconds int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if before, claimed := m.claims[key]; claimed {
		return before, nil
	}
	which := directMonth{tenant, MonthOf(month)}
	before := m.direct[which]
	m.direct[which] = before + seconds
	m.claims[key] = before
	return before, nil
}

func cloneTenant(tenant Tenant) Tenant {
	tenant.Spending.Alerts = slices.Clone(tenant.Spending.Alerts)
	return tenant
}

func (m *Memory) CreateTenant(_ context.Context, tenant Tenant) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tenants[tenant.ID]; exists {
		return Tenant{}, ErrAlreadyExists
	}
	m.tenants[tenant.ID] = cloneTenant(tenant)
	return cloneTenant(tenant), nil
}

func (m *Memory) GetTenant(_ context.Context, id string) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant, ok := m.tenants[id]
	if !ok {
		return Tenant{}, ErrNotFound
	}
	return cloneTenant(tenant), nil
}

func (m *Memory) SaveTenant(_ context.Context, tenant Tenant) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.tenants[tenant.ID]
	if !ok {
		return Tenant{}, ErrNotFound
	}
	tenant.Spending = existing.Spending
	m.tenants[tenant.ID] = cloneTenant(tenant)
	return cloneTenant(tenant), nil
}

func (m *Memory) SetSpending(_ context.Context, id string, spending Spending, at time.Time) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant, ok := m.tenants[id]
	if !ok {
		return Tenant{}, ErrNotFound
	}
	tenant.Spending = spending
	tenant.UpdatedAt = at
	m.tenants[id] = cloneTenant(tenant)
	return cloneTenant(tenant), nil
}

func (m *Memory) UsageSince(_ context.Context, tenant string, since time.Time) (Micros, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var spent Micros
	for _, entry := range m.entries {
		if entry.Tenant == tenant && entry.Kind == KindUsage && !entry.At.Before(since) {
			spent -= entry.Amount
		}
	}
	return spent, nil
}

func (m *Memory) UsageBreakdown(_ context.Context, tenant string, from, to time.Time) ([]UsageBucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type group struct {
		role    Role
		tier    Tier
		hosting Hosting
		product string
	}
	sums := map[group]*UsageBucket{}
	for _, entry := range m.entries {
		if entry.Tenant != tenant || entry.Kind != KindUsage || entry.At.Before(from) || !entry.At.Before(to) {
			continue
		}
		var detail UsageDetail
		_ = json.Unmarshal([]byte(entry.Detail), &detail)
		key := group{detail.Role, detail.Tier, detail.Hosting, detail.Product}
		bucket := sums[key]
		if bucket == nil {
			bucket = &UsageBucket{Role: key.role, Tier: key.tier, Hosting: key.hosting, Product: key.product}
			sums[key] = bucket
		}
		bucket.Records++
		bucket.Seconds += detail.Seconds
		bucket.EgressBytes += detail.EgressBytes
		bucket.IngressBytes += detail.IngressBytes
		bucket.StorageByteHours += detail.StorageByteHours
		bucket.Charged -= entry.Amount
		if entry.Capped {
			bucket.Capped++
		}
	}
	buckets := make([]UsageBucket, 0, len(sums))
	for _, bucket := range sums {
		buckets = append(buckets, *bucket)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].less(buckets[j]) })
	return buckets, nil
}

func (m *Memory) ClaimSpendAlert(_ context.Context, tenant string, month time.Time, threshold Micros, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[tenant]; !ok {
		return false, ErrNotFound
	}
	key := alertKey{tenant, month.UTC(), threshold}
	if _, fired := m.alerts[key]; fired {
		return false, nil
	}
	m.alerts[key] = at
	return true, nil
}

func (m *Memory) ReleaseSpendAlert(_ context.Context, tenant string, month time.Time, threshold Micros) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.alerts, alertKey{tenant, month.UTC(), threshold})
	return nil
}

func (m *Memory) FiredSpendAlerts(_ context.Context, tenant string, month time.Time) ([]FiredAlert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var fired []FiredAlert
	for key, at := range m.alerts {
		if key.tenant == tenant && key.month.Equal(month.UTC()) {
			fired = append(fired, FiredAlert{Threshold: key.threshold, At: at})
		}
	}
	sort.Slice(fired, func(i, j int) bool { return fired[i].Threshold < fired[j].Threshold })
	return fired, nil
}

func (m *Memory) FindTenantByStripeCustomer(_ context.Context, customer string) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tenant := range m.tenants {
		if customer != "" && tenant.StripeCustomer == customer {
			return tenant, nil
		}
	}
	return Tenant{}, ErrNotFound
}

func (m *Memory) CountTenants(_ context.Context) (total, active int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tenant := range m.tenants {
		total++
		if tenant.Status == StatusActive {
			active++
		}
	}
	return total, active, nil
}

func (m *Memory) ClaimTopUp(_ context.Context, id string, now time.Time, staleAfter time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant, ok := m.tenants[id]
	if !ok {
		return false, ErrNotFound
	}
	if !tenant.TopUpPendingSince.IsZero() && now.Sub(tenant.TopUpPendingSince) < staleAfter {
		return false, nil
	}
	tenant.TopUpPendingSince = now
	m.tenants[id] = tenant
	return true, nil
}

func (m *Memory) AppendEntry(_ context.Context, entry Entry) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.IdempotencyKey == "" {
		return Entry{}, false, errors.New("an entry needs an idempotency key")
	}
	if index, ok := m.byKey[entry.IdempotencyKey]; ok {
		return m.entries[index], false, nil
	}
	m.byKey[entry.IdempotencyKey] = len(m.entries)
	m.entries = append(m.entries, entry)
	return entry, true, nil
}

func (m *Memory) FindEntry(_ context.Context, idempotencyKey string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index, ok := m.byKey[idempotencyKey]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return m.entries[index], nil
}

func (m *Memory) Balance(_ context.Context, tenant string) (Micros, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var balance Micros
	for _, entry := range m.entries {
		if entry.Tenant == tenant {
			balance += entry.Amount
		}
	}
	return balance, nil
}

func (m *Memory) ListEntries(_ context.Context, tenant string, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []Entry
	for i := len(m.entries) - 1; i >= 0 && (limit <= 0 || len(found) < limit); i-- {
		if m.entries[i].Tenant == tenant {
			found = append(found, m.entries[i])
		}
	}
	return found, nil
}

func (m *Memory) PutKey(_ context.Context, key Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.keys[key.ID]; exists {
		return ErrAlreadyExists
	}
	m.keys[key.ID] = cloneKey(key)
	return nil
}

func cloneKey(key Key) Key {
	key.AllowedNetworks = slices.Clone(key.AllowedNetworks)
	return key
}

func (m *Memory) GetKey(_ context.Context, id string) (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	return cloneKey(key), nil
}

func (m *Memory) ListKeys(_ context.Context, tenant string) ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []Key
	for _, key := range m.keys {
		if key.Tenant == tenant {
			found = append(found, cloneKey(key))
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.Before(found[j].CreatedAt) })
	return found, nil
}

func (m *Memory) RevokeKey(_ context.Context, tenant, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[id]
	if !ok || key.Tenant != tenant {
		return ErrNotFound
	}
	if key.RevokedAt.IsZero() || at.Before(key.RevokedAt) {
		key.RevokedAt = at
		m.keys[id] = key
	}
	return nil
}

func (m *Memory) RehashKey(_ context.Context, id, from, to, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[id]
	if !ok {
		return ErrNotFound
	}
	if key.Hash == from {
		key.Hash, key.PepperVersion = to, version
		m.keys[id] = key
	}
	return nil
}

func sameUse(a, b KeyUse) bool {
	return a.Key == b.Key && a.Minute.Equal(b.Minute) && a.Operation == b.Operation &&
		a.Address == b.Address && a.Outcome == b.Outcome && a.Detail == b.Detail
}

func (m *Memory) RecordKeyUse(_ context.Context, uses []KeyUse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, use := range uses {
		use.Minute = use.Minute.UTC()
		merged := false
		// Entries merge within a minute, so only the recent tail can match.
		for i := len(m.keyUse) - 1; i >= max(0, len(m.keyUse)-5000) && !merged; i-- {
			if existing := &m.keyUse[i]; sameUse(*existing, use) {
				existing.Count += use.Count
				if use.FirstAt.Before(existing.FirstAt) {
					existing.FirstAt = use.FirstAt
				}
				if use.LastAt.After(existing.LastAt) {
					existing.LastAt = use.LastAt
				}
				merged = true
			}
		}
		if !merged {
			m.keyUse = append(m.keyUse, use)
		}
	}
	if over := len(m.keyUse) - mostKeyUseInMemory; over > 0 {
		m.keyUse = slices.Delete(m.keyUse, 0, over)
	}
	return nil
}

func (m *Memory) ListKeyUse(_ context.Context, tenant, key string, limit int) ([]KeyUse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []KeyUse
	for _, use := range m.keyUse {
		if use.Tenant == tenant && (key == "" || use.Key == key) {
			found = append(found, use)
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].LastAt.After(found[j].LastAt) })
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}

func (m *Memory) SaveJob(_ context.Context, job Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job.Completed = append([]Step(nil), job.Completed...)
	m.jobs[job.ID] = job
	return nil
}

func (m *Memory) GetJob(_ context.Context, id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return job, nil
}
