package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"time"
)

var (
	// ErrPaymentRequired is a tenant that has never paid.
	ErrPaymentRequired = errors.New("payment required: add credit to activate this tenant")
	// ErrInsufficientCredit is a tenant whose prepaid balance is spent.
	ErrInsufficientCredit = errors.New("insufficient credit: top up to admit new sessions")
	// ErrSpendLimitReached is a tenant whose usage this month has reached the
	// spend limit it set. Topping up does not help; raising the limit does.
	ErrSpendLimitReached = errors.New("spend limit reached: this month's usage has reached the tenant's spend limit; raise or remove it to admit new sessions")
	// ErrSuspended is a tenant an operator has suspended, or one being torn down.
	ErrSuspended = errors.New("tenant is not active")
	// ErrPaymentsUnavailable is a deployment with no Stripe key configured.
	ErrPaymentsUnavailable = errors.New("payments are not configured on this deployment")
	// ErrInvalid is a request that cannot be satisfied as asked.
	ErrInvalid = errors.New("invalid request")
)

// Config is everything billing needs.
type Config struct {
	Store Store
	// Stripe is nil on a deployment that takes no payments, which can still
	// sign tenants up and meter them but can never activate one.
	Stripe        *Stripe
	WebhookSecret string
	// Infrastructure creates per-tenant GCP resources at signup. Nil skips them.
	Infrastructure Infrastructure
	Pricing        Pricing

	// PublicURL is where Stripe's hosted pages send a customer back to.
	PublicURL string

	MinTopUp     Micros
	MaxTopUp     Micros
	DefaultTopUp Micros
	// SignupCredit is granted at signup and spendable once the tenant has paid
	// once. Zero by default: free credit without a card is the first thing an
	// abuser automates.
	SignupCredit Micros

	// Exempt tenants are admitted without a balance and their usage is not
	// charged: operator test tenants, benchmarks, and the demo.
	Exempt map[string]bool

	// Usage, if set, can withhold a record from charging, such as a
	// participant trust and safety has excluded. Nil charges everything.
	Usage UsageFilter

	// Notifier delivers spend alerts. Nil logs them and sends nothing.
	Notifier Notifier

	// Peppers key the stored API key hashes. None keeps bare SHA-256.
	Peppers Peppers
	// GitHub verifies secret scanning reports. Nil fetches GitHub's
	// published keys.
	GitHub GitHubKeySource

	Now    func() time.Time
	Random io.Reader
	Logger *slog.Logger
}

// UsageFilter decides that a participant's usage is not the tenant's to pay
// for. The reason is logged, never shown to the participant.
type UsageFilter interface {
	Unbillable(ctx context.Context, tenant, room, participant string) (bool, string)
}

// Service is MOQOM Cloud's billing.
type Service struct {
	store         Store
	stripe        *Stripe
	webhookSecret string
	infra         Infrastructure
	pricing       Pricing
	publicURL     string
	minTopUp      Micros
	maxTopUp      Micros
	defaultTopUp  Micros
	signupCredit  Micros
	exempt        map[string]bool
	usage         UsageFilter
	notifier      Notifier
	keys          *KeyAuth
	github        GitHubKeySource
	now           func() time.Time
	random        io.Reader
	log           *slog.Logger
	agora         *agoraClient
}

// New validates the configuration.
func New(config Config) (*Service, error) {
	if config.Store == nil {
		return nil, errors.New("billing needs a store")
	}
	if config.Stripe != nil && config.WebhookSecret == "" {
		return nil, errors.New("billing with Stripe needs a webhook secret, or payments would never be credited")
	}
	if config.MinTopUp == 0 {
		config.MinTopUp = 10 * Dollar
	}
	if config.MaxTopUp == 0 {
		config.MaxTopUp = 10_000 * Dollar
	}
	if config.DefaultTopUp == 0 {
		config.DefaultTopUp = 50 * Dollar
	}
	if config.MinTopUp > config.MaxTopUp || config.DefaultTopUp < config.MinTopUp || config.DefaultTopUp > config.MaxTopUp {
		return nil, errors.New("top-up bounds are inconsistent")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if costs := config.Pricing.Costs; config.Pricing.Ceiling == nil && costs.Storage == nil &&
		costs.EgressPerGiB == 0 && costs.IngressPerGiB == 0 && costs.ConnectedSecond == 0 {
		config.Pricing = DefaultPricing()
	}
	if config.GitHub == nil {
		config.GitHub = &GitHubKeys{URL: GitHubSecretScanningKeysURL}
	}
	return &Service{
		store:         config.Store,
		stripe:        config.Stripe,
		webhookSecret: config.WebhookSecret,
		infra:         config.Infrastructure,
		pricing:       config.Pricing,
		publicURL:     strings.TrimRight(config.PublicURL, "/"),
		minTopUp:      config.MinTopUp,
		maxTopUp:      config.MaxTopUp,
		defaultTopUp:  config.DefaultTopUp,
		signupCredit:  config.SignupCredit,
		exempt:        config.Exempt,
		usage:         config.Usage,
		notifier:      config.Notifier,
		keys:          &KeyAuth{Store: config.Store, Peppers: config.Peppers, Now: config.Now, Log: config.Logger},
		github:        config.GitHub,
		now:           config.Now,
		random:        config.Random,
		log:           config.Logger,
		agora:         newAgoraClient(),
	}, nil
}

// Store is where the ledger and keys live.
func (s *Service) Store() Store { return s.store }

// Pricing is the rate card in force.
func (s *Service) Pricing() Pricing { return s.pricing }

func (s *Service) randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// SignupRequest is a new customer.
type SignupRequest struct {
	Email string
	Name  string
}

// SignupResult carries the API key, which exists in plaintext only here.
type SignupResult struct {
	Tenant   Tenant
	Key      Key
	APIKey   string
	Checkout Checkout
	Job      Job
}

// Signup provisions a tenant and its first API key, and opens a checkout for
// its first top-up. The tenant is pending until that payment lands.
func (s *Service) Signup(ctx context.Context, request SignupRequest) (SignupResult, error) {
	address, err := mail.ParseAddress(strings.TrimSpace(request.Email))
	if err != nil || len(address.Address) > 254 {
		return SignupResult{}, fmt.Errorf("%w: a valid email address is required", ErrInvalid)
	}
	name := strings.TrimSpace(request.Name)
	if name == "" || len(name) > 200 {
		return SignupResult{}, fmt.Errorf("%w: a name of 1 to 200 characters is required", ErrInvalid)
	}

	suffix, err := s.randomHex(8)
	if err != nil {
		return SignupResult{}, err
	}
	jobID, err := s.randomHex(12)
	if err != nil {
		return SignupResult{}, err
	}
	now := s.now()
	result := SignupResult{Tenant: Tenant{
		ID:        "t_" + suffix,
		Name:      name,
		Email:     address.Address,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}}

	steps := []step{{
		name: StepRegisterTenant,
		do: func(ctx context.Context) error {
			created, err := s.store.CreateTenant(ctx, result.Tenant)
			result.Tenant = created
			return err
		},
		undo: func(ctx context.Context) error {
			result.Tenant.Status = StatusDeleted
			_, err := s.store.SaveTenant(ctx, result.Tenant)
			return err
		},
	}}
	if s.stripe != nil {
		steps = append(steps, step{
			name: StepCreateStripeCustomer,
			do: func(ctx context.Context) error {
				customer, err := s.stripe.CreateCustomer(ctx, result.Tenant.ID, result.Tenant.Email, result.Tenant.Name)
				if err != nil {
					return err
				}
				result.Tenant.StripeCustomer = customer
				result.Tenant, err = s.store.SaveTenant(ctx, result.Tenant)
				return err
			},
		})
	}
	if s.infra != nil {
		steps = append(steps,
			step{
				name: StepCreateServiceAccount,
				do:   func(ctx context.Context) error { return s.infra.CreateServiceAccount(ctx, result.Tenant.ID) },
				undo: func(ctx context.Context) error { return s.infra.DeleteServiceAccount(ctx, result.Tenant.ID) },
			},
			step{
				name: StepCreateBucket,
				do:   func(ctx context.Context) error { return s.infra.CreateBucket(ctx, result.Tenant.ID) },
				undo: func(ctx context.Context) error { return s.infra.DeleteBucket(ctx, result.Tenant.ID) },
			},
		)
	}
	steps = append(steps, step{
		name: StepIssueAPIKey,
		do: func(ctx context.Context) error {
			key, plaintext, err := s.issueKey(ctx, result.Tenant.ID, "default", KeyBackend)
			result.Key, result.APIKey = key, plaintext
			return err
		},
		undo: func(ctx context.Context) error {
			return s.store.RevokeKey(ctx, result.Tenant.ID, result.Key.ID, s.now())
		},
	})

	result.Job, err = s.run(ctx, Job{ID: jobID, Tenant: result.Tenant.ID, CreatedAt: now}, steps)
	if err != nil {
		return SignupResult{}, err
	}

	if s.signupCredit > 0 {
		if _, _, err := s.store.AppendEntry(ctx, Entry{
			ID:             jobID,
			Tenant:         result.Tenant.ID,
			Kind:           KindSignupCredit,
			Amount:         s.signupCredit,
			IdempotencyKey: "signup-credit:" + result.Tenant.ID,
			At:             now,
		}); err != nil {
			return SignupResult{}, err
		}
	}

	// Outside the job: a tenant whose checkout could not be opened is still a
	// tenant, and can open one with its new key.
	if s.stripe != nil {
		checkout, err := s.Checkout(ctx, result.Tenant.ID, s.defaultTopUp)
		if err != nil {
			s.log.WarnContext(ctx, "signup succeeded but checkout could not be opened",
				"tenant", result.Tenant.ID, "error", err)
		}
		result.Checkout = checkout
	}
	return result, nil
}

// Allow is the gate in front of everything that consumes media. Moderation and
// administration never pass through it: a tenant out of credit must still be
// able to ban somebody.
//
// A tenant with an empty balance and auto top-up on is topped up here, once,
// before being refused. Usage is what normally triggers a top-up, and a tenant
// whose top-up was held back by its spend limit has no usage left to trigger
// one when the month turns over.
func (s *Service) Allow(ctx context.Context, tenant string) error {
	record, err := s.admit(ctx, tenant)
	if !errors.Is(err, ErrInsufficientCredit) || !record.AutoTopUp.Enabled || record.LastTopUpError != "" {
		return err
	}
	s.maybeTopUp(ctx, tenant, "admission:"+tenant+":"+s.now().UTC().Format(time.RFC3339Nano))
	_, err = s.admit(ctx, tenant)
	return err
}

// Admitted is Allow without the side effect, for reporting.
func (s *Service) Admitted(ctx context.Context, tenant string) error {
	_, err := s.admit(ctx, tenant)
	return err
}

func (s *Service) admit(ctx context.Context, tenant string) (Tenant, error) {
	if s.exempt[tenant] {
		return Tenant{}, nil
	}
	record, err := s.store.GetTenant(ctx, tenant)
	if errors.Is(err, ErrNotFound) {
		return Tenant{}, ErrPaymentRequired
	}
	if err != nil {
		return Tenant{}, err
	}
	switch record.Status {
	case StatusActive:
	case StatusPending:
		return record, ErrPaymentRequired
	default:
		return record, ErrSuspended
	}
	// Before the balance, because topping up does not lift a limit.
	if reached, err := s.limitReached(ctx, record); err != nil {
		return record, err
	} else if reached {
		return record, ErrSpendLimitReached
	}
	balance, err := s.store.Balance(ctx, tenant)
	if err != nil {
		return record, err
	}
	if balance <= 0 {
		return record, ErrInsufficientCredit
	}
	return record, nil
}

func (s *Service) limitReached(ctx context.Context, record Tenant) (bool, error) {
	if record.Spending.Limit <= 0 {
		return false, nil
	}
	spent, err := s.store.UsageSince(ctx, record.ID, MonthOf(s.now()))
	if err != nil {
		return false, err
	}
	return spent >= record.Spending.Limit, nil
}

// Checkout opens a hosted page for buying amount of credit.
func (s *Service) Checkout(ctx context.Context, tenant string, amount Micros) (Checkout, error) {
	if s.stripe == nil {
		return Checkout{}, ErrPaymentsUnavailable
	}
	if amount < s.minTopUp || amount > s.maxTopUp || amount%Cent != 0 {
		return Checkout{}, fmt.Errorf("%w: a top-up must be whole cents between %s and %s", ErrInvalid, s.minTopUp, s.maxTopUp)
	}
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return Checkout{}, err
	}
	if record.Status == StatusArchived || record.Status == StatusDeleted {
		return Checkout{}, ErrSuspended
	}
	if record.StripeCustomer == "" {
		customer, err := s.stripe.CreateCustomer(ctx, record.ID, record.Email, record.Name)
		if err != nil {
			return Checkout{}, err
		}
		record.StripeCustomer = customer
		record.UpdatedAt = s.now()
		if record, err = s.store.SaveTenant(ctx, record); err != nil {
			return Checkout{}, err
		}
	}
	return s.stripe.CreateCheckout(ctx, record.ID, record.StripeCustomer, amount,
		s.publicURL+"/billing/success?session_id={CHECKOUT_SESSION_ID}",
		s.publicURL+"/billing/cancelled")
}

// Portal opens Stripe's customer portal for a tenant.
func (s *Service) Portal(ctx context.Context, tenant string) (string, error) {
	if s.stripe == nil {
		return "", ErrPaymentsUnavailable
	}
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return "", err
	}
	if record.StripeCustomer == "" {
		return "", fmt.Errorf("%w: this tenant has not paid yet, so has nothing to manage", ErrInvalid)
	}
	return s.stripe.CreatePortal(ctx, record.StripeCustomer, s.publicURL+"/billing")
}

// SetAutoTopUp configures automatic top-ups.
func (s *Service) SetAutoTopUp(ctx context.Context, tenant string, config AutoTopUp) (Tenant, error) {
	if err := config.Validate(s.minTopUp, s.maxTopUp); err != nil {
		return Tenant{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return Tenant{}, err
	}
	record.AutoTopUp = config
	record.UpdatedAt = s.now()
	return s.store.SaveTenant(ctx, record)
}

// SetSpendLimit sets the monthly spend limit, or clears it with zero.
func (s *Service) SetSpendLimit(ctx context.Context, tenant string, limit Micros) (Tenant, error) {
	return s.changeSpending(ctx, tenant, func(spending *Spending) { spending.Limit = limit })
}

// SetSpendAlerts sets the alert thresholds. No thresholds with alerts on means
// the defaults of the limit.
func (s *Service) SetSpendAlerts(ctx context.Context, tenant string, enabled bool, thresholds []Micros) (Tenant, error) {
	return s.changeSpending(ctx, tenant, func(spending *Spending) {
		spending.AlertsOff = !enabled
		spending.Alerts = nil
		if enabled && len(thresholds) > 0 {
			spending.Alerts = slices.Clone(thresholds)
		}
	})
}

func (s *Service) changeSpending(ctx context.Context, tenant string, change func(*Spending)) (Tenant, error) {
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return Tenant{}, err
	}
	spending := record.Spending
	change(&spending)
	if err := spending.Validate(); err != nil {
		return Tenant{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return s.store.SetSpending(ctx, tenant, spending, s.now())
}

// MonthSpend is a tenant's spending state for the current month.
type MonthSpend struct {
	Month time.Time
	Spent Micros
	// Thresholds are the alerts in force, after defaults.
	Thresholds []Micros
	Fired      []FiredAlert
}

// Spend reports this month's spend and which alerts have fired.
func (s *Service) Spend(ctx context.Context, record Tenant) (MonthSpend, error) {
	month := MonthOf(s.now())
	spent, err := s.store.UsageSince(ctx, record.ID, month)
	if err != nil {
		return MonthSpend{}, err
	}
	fired, err := s.store.FiredSpendAlerts(ctx, record.ID, month)
	if err != nil {
		return MonthSpend{}, err
	}
	return MonthSpend{Month: month, Spent: spent, Thresholds: record.Spending.Thresholds(), Fired: fired}, nil
}

// Summary is a tenant's billing state.
type Summary struct {
	Tenant  Tenant
	Balance Micros
	Spend   MonthSpend
	Recent  []Entry
}

func (s *Service) Summary(ctx context.Context, tenant string) (Summary, error) {
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return Summary{}, err
	}
	balance, err := s.store.Balance(ctx, tenant)
	if err != nil {
		return Summary{}, err
	}
	spend, err := s.Spend(ctx, record)
	if err != nil {
		return Summary{}, err
	}
	recent, err := s.store.ListEntries(ctx, tenant, 50)
	if err != nil {
		return Summary{}, err
	}
	return Summary{Tenant: record, Balance: balance, Spend: spend, Recent: recent}, nil
}

// Keys.

func (s *Service) issueKey(ctx context.Context, tenant, name string, kind KeyKind) (Key, string, error) {
	return s.issueSpec(ctx, tenant, KeySpec{Name: name, Kind: kind})
}

// IssueKey creates another key for a tenant, with no expiry, allowlist or
// rate limit of its own. See IssueKeyWith.
func (s *Service) IssueKey(ctx context.Context, tenant, name string, kind KeyKind) (Key, string, error) {
	return s.IssueKeyWith(ctx, tenant, KeySpec{Name: name, Kind: kind})
}

func (s *Service) ListKeys(ctx context.Context, tenant string) ([]Key, error) {
	return s.store.ListKeys(ctx, tenant)
}

// ErrWouldStopService is a revocation that would leave a tenant with no working
// key of that kind, refused unless the caller confirms it.
var ErrWouldStopService = errors.New("this is the tenant's last working key of its kind")

// Rotation overlap bounds: long enough to roll a deployment, short enough that
// a rotated-out key does not linger.
const (
	DefaultRotationOverlap = 24 * time.Hour
	MaxRotationOverlap     = 7 * 24 * time.Hour
)

// RotateKey issues a replacement for a key and schedules the old one to stop
// working after overlap, so both work while the new one is rolled out.
func (s *Service) RotateKey(ctx context.Context, tenant, id string, overlap time.Duration) (Key, string, time.Time, error) {
	if overlap < 0 || overlap > MaxRotationOverlap {
		return Key{}, "", time.Time{}, fmt.Errorf("%w: overlap must be between 0 and %s", ErrInvalid, MaxRotationOverlap)
	}
	old, err := s.store.GetKey(ctx, id)
	if err != nil || old.Tenant != tenant {
		return Key{}, "", time.Time{}, ErrNotFound
	}
	now := s.now()
	if !old.WorksAt(now) {
		return Key{}, "", time.Time{}, fmt.Errorf("%w: that key is already revoked or expired; issue a new one instead", ErrInvalid)
	}
	issued, plaintext, err := s.IssueKeyWith(ctx, tenant, replacementFor(old, now))
	if err != nil {
		return Key{}, "", time.Time{}, err
	}
	retires := now.Add(overlap)
	if err := s.store.RevokeKey(ctx, tenant, id, retires); err != nil {
		return Key{}, "", time.Time{}, err
	}
	if old.Retiring(now) && old.RevokedAt.Before(retires) {
		retires = old.RevokedAt
	}
	return issued, plaintext, retires, nil
}

// RevokeKey stops a key working now. Revoking a tenant's last working backend
// key stops it minting tokens, so new sessions fail and live ones end when
// their tokens can't renew; the last relay key stops usage reports. Both are
// refused unless confirmed.
func (s *Service) RevokeKey(ctx context.Context, tenant, id string, confirmed bool) error {
	if !confirmed {
		keys, err := s.store.ListKeys(ctx, tenant)
		if err != nil {
			return err
		}
		now := s.now()
		var target *Key
		for i := range keys {
			if keys[i].ID == id {
				target = &keys[i]
			}
		}
		if target == nil {
			return ErrNotFound
		}
		// Only the kinds the service depends on: an agent's scoped key going
		// stops that agent and nothing else.
		if target.WorksAt(now) && (target.Kind == KeyBackend || target.Kind == KeyRelay) {
			remaining := 0
			for _, k := range keys {
				if k.ID != id && k.Kind == target.Kind && k.WorksAt(now) {
					remaining++
				}
			}
			if remaining == 0 {
				return fmt.Errorf("%w (%s)", ErrWouldStopService, target.Kind)
			}
		}
	}
	return s.store.RevokeKey(ctx, tenant, id, s.now())
}

// Usage.

// UsageRecord is one participant's consumption over one interval, as a relay
// reports it. ID is the relay's own, unique per interval, and is what makes a
// retried report charge once.
type UsageRecord struct {
	ID           string
	Tenant       string
	Room         string
	Participant  string
	Role         Role
	Tier         Tier
	Seconds      int64
	EgressBytes  int64
	IngressBytes int64
	// DirectSeconds is time in the interval the participant held a live
	// direct ticket from the relay. Zero from a relay that predates tickets.
	DirectSeconds int64
}

// Reporter is who sent a usage report, which decides whose bytes they were.
type Reporter struct {
	// Tenant is set for a self-hosted relay's key, and every record is charged
	// to it whatever the record says. Empty for MOQOM's own relays, which
	// report for every tenant.
	Tenant  string
	Hosting Hosting
}

// UsageResult says what a report did.
type UsageResult struct {
	Charged    int
	Duplicates int
	Unbilled   int
	Total      Micros
}

const longestInterval = int64(24 * 60 * 60)

func validRole(role Role) bool {
	return role == RoleHost || role == RoleAudience || role == RoleBroadcastAudience
}

func validTier(tier Tier) bool {
	switch tier {
	case TierAudio, TierHD, TierFullHD, Tier2K, Tier2KPlus:
		return true
	}
	return false
}

// RecordUsage rates and charges a batch of usage records.
func (s *Service) RecordUsage(ctx context.Context, reporter Reporter, records []UsageRecord) (UsageResult, error) {
	var result UsageResult
	touched := map[string]string{}
	for i, record := range records {
		if record.ID == "" || len(record.ID) > 200 {
			return result, fmt.Errorf("%w: record %d needs an id of at most 200 characters", ErrInvalid, i)
		}
		if !validRole(record.Role) || !validTier(record.Tier) {
			return result, fmt.Errorf("%w: record %s has an unknown role or tier", ErrInvalid, record.ID)
		}
		if record.Seconds < 0 || record.Seconds > longestInterval || record.EgressBytes < 0 || record.IngressBytes < 0 ||
			record.DirectSeconds < 0 || record.DirectSeconds > longestInterval {
			return result, fmt.Errorf("%w: record %s has a negative or implausible quantity", ErrInvalid, record.ID)
		}
		if reporter.Hosting == HostingCloudflare && record.DirectSeconds > 0 {
			// Only a MOQOM relay issues direct tickets.
			return result, fmt.Errorf("%w: record %s reports direct time from Cloudflare", ErrInvalid, record.ID)
		}
		tenant := record.Tenant
		if reporter.Tenant != "" {
			if tenant != "" && tenant != reporter.Tenant {
				return result, fmt.Errorf("%w: record %s names another tenant", ErrInvalid, record.ID)
			}
			tenant = reporter.Tenant
		}
		if tenant == "" {
			return result, fmt.Errorf("%w: record %s names no tenant", ErrInvalid, record.ID)
		}
		if s.exempt[tenant] {
			result.Unbilled++
			continue
		}
		if s.usage != nil && record.Participant != "" {
			if skip, why := s.usage.Unbillable(ctx, tenant, record.Room, record.Participant); skip {
				s.log.InfoContext(ctx, "usage withheld", "tenant", tenant, "record", record.ID, "reason", why)
				result.Unbilled++
				continue
			}
		}
		if _, err := s.store.GetTenant(ctx, tenant); err != nil {
			if errors.Is(err, ErrNotFound) {
				s.log.WarnContext(ctx, "usage for an unregistered tenant", "tenant", tenant, "record", record.ID)
				result.Unbilled++
				continue
			}
			return result, err
		}

		charge := s.pricing.Rate(Usage{
			Role:         record.Role,
			Tier:         record.Tier,
			Seconds:      record.Seconds,
			EgressBytes:  record.EgressBytes,
			IngressBytes: record.IngressBytes,
			Hosting:      reporter.Hosting,
		})
		id, err := s.randomHex(12)
		if err != nil {
			return result, err
		}
		key := "usage:" + string(reporter.Hosting) + ":" + tenant + ":" + record.ID
		// Claimed under the entry's own key, before the entry, so a report
		// that failed between the two is priced the same when it is re-sent.
		// After the cap, which is a ceiling on what relaying costs and has
		// nothing to say about a direct path.
		var trial bool
		if reporter.Hosting == HostingCloudflare {
			if s.now().Before(s.pricing.CloudflareFreeUntil) {
				trial = true
				charge = Charge{}
			} else {
				before, err := s.store.ClaimCloudflareSeconds(ctx, tenant, MonthOf(s.now()), key, record.Seconds)
				if err != nil {
					return result, err
				}
				charge.Price = s.pricing.CloudflarePrice(charge.Price, before, record.Seconds)
			}
		}
		var directFee Micros
		if record.DirectSeconds > 0 {
			before, err := s.store.ClaimDirectSeconds(ctx, tenant, MonthOf(s.now()), key, record.DirectSeconds)
			if err != nil {
				return result, err
			}
			directFee = s.pricing.DirectFee(before, record.DirectSeconds)
		}
		fields := map[string]any{
			"role": record.Role, "tier": record.Tier, "seconds": record.Seconds,
			"egress_bytes": record.EgressBytes, "ingress_bytes": record.IngressBytes,
			"hosting": reporter.Hosting, "room": record.Room, "participant": record.Participant,
		}
		if trial {
			fields["trial"] = true
		}
		if record.DirectSeconds > 0 {
			fields["direct_seconds"] = record.DirectSeconds
			fields["direct_fee"] = int64(directFee)
		}
		detail, _ := json.Marshal(fields)
		price := charge.Price + directFee
		_, created, err := s.store.AppendEntry(ctx, Entry{
			ID:             id,
			Tenant:         tenant,
			Kind:           KindUsage,
			Amount:         -price,
			Cost:           charge.Cost,
			Capped:         charge.Capped,
			IdempotencyKey: key,
			Reference:      record.Room,
			Detail:         string(detail),
			At:             s.now(),
		})
		if err != nil {
			return result, err
		}
		if !created {
			result.Duplicates++
			continue
		}
		result.Charged++
		result.Total += price
		touched[tenant] = key
	}
	for tenant, trigger := range touched {
		s.checkSpendAlerts(ctx, tenant)
		s.maybeTopUp(ctx, tenant, trigger)
	}
	return result, nil
}

// spendAlertTimeout bounds one delivery, which happens inside a relay's usage
// report.
const spendAlertTimeout = 15 * time.Second

// checkSpendAlerts claims every threshold this month's spend has crossed and
// not yet alerted on, and sends one notice for them. A notice that fails is
// released, so the next usage report tries again.
func (s *Service) checkSpendAlerts(ctx context.Context, tenant string) {
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return
	}
	thresholds := record.Spending.Thresholds()
	if len(thresholds) == 0 {
		return
	}
	month := MonthOf(s.now())
	spent, err := s.store.UsageSince(ctx, tenant, month)
	if err != nil {
		s.log.WarnContext(ctx, "spend alerts not checked", "tenant", tenant, "error", err)
		return
	}
	var crossed []Micros
	for _, threshold := range thresholds {
		if spent < threshold {
			break
		}
		claimed, err := s.store.ClaimSpendAlert(ctx, tenant, month, threshold, s.now())
		if err != nil {
			s.log.WarnContext(ctx, "spend alert not claimed", "tenant", tenant, "threshold", threshold.String(), "error", err)
			break
		}
		if claimed {
			crossed = append(crossed, threshold)
		}
	}
	if len(crossed) == 0 {
		return
	}
	alert := SpendAlert{
		Tenant:     record,
		Month:      month,
		Spent:      spent,
		Threshold:  crossed[len(crossed)-1],
		Thresholds: crossed,
		Limit:      record.Spending.Limit,
	}
	// The compliance module can alert on this event like any other.
	s.log.InfoContext(ctx, "spend alert", "event", "billing_spend_alert", "tenant", tenant,
		"month", month.Format("2006-01"), "spent", spent.String(), "threshold", alert.Threshold.String(),
		"limit", alert.Limit.String(), "limit_reached", alert.LimitReached())
	if s.notifier == nil {
		return
	}
	deliver, cancel := context.WithTimeout(context.WithoutCancel(ctx), spendAlertTimeout)
	defer cancel()
	if err := s.notifier.NotifySpend(deliver, alert); err != nil {
		s.log.WarnContext(ctx, "spend alert not delivered; the next usage report retries it",
			"tenant", tenant, "threshold", alert.Threshold.String(), "error", err)
		for _, threshold := range crossed {
			if err := s.store.ReleaseSpendAlert(ctx, tenant, month, threshold); err != nil {
				s.log.ErrorContext(ctx, "spend alert claim not released; it will not be retried this month",
					"tenant", tenant, "threshold", threshold.String(), "error", err)
			}
		}
	}
}

// topUpStaleAfter is how long an automatic top-up may stay unconfirmed before
// another is allowed. Long enough that a slow webhook never doubles a charge.
const topUpStaleAfter = time.Hour

func (s *Service) maybeTopUp(ctx context.Context, tenant, trigger string) {
	if s.stripe == nil {
		return
	}
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil || !record.AutoTopUp.Enabled || record.PaymentMethod == "" || record.StripeCustomer == "" {
		return
	}
	balance, err := s.store.Balance(ctx, tenant)
	if err != nil || balance >= record.AutoTopUp.Below {
		return
	}
	// A tenant that capped its spend did not ask for its card to keep being
	// charged past the cap.
	if reached, err := s.limitReached(ctx, record); err != nil || reached {
		return
	}
	claimed, err := s.store.ClaimTopUp(ctx, tenant, s.now(), topUpStaleAfter)
	if err != nil || !claimed {
		return
	}
	intent, err := s.stripe.ChargeSavedCard(ctx, tenant, record.StripeCustomer, record.PaymentMethod,
		record.AutoTopUp.Amount, "auto-top-up:"+trigger)
	if err != nil {
		s.log.WarnContext(ctx, "automatic top-up failed", "tenant", tenant, "error", err)
		s.releaseTopUp(ctx, tenant, err.Error())
		return
	}
	if intent.Status == "succeeded" {
		if err := s.creditPayment(ctx, intent); err != nil {
			s.log.ErrorContext(ctx, "automatic top-up charged but not credited; the webhook will retry it",
				"tenant", tenant, "payment_intent", intent.ID, "error", err)
		}
	}
}

func (s *Service) releaseTopUp(ctx context.Context, tenant, failure string) {
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return
	}
	record.TopUpPendingSince = time.Time{}
	record.LastTopUpError = failure
	record.UpdatedAt = s.now()
	_, _ = s.store.SaveTenant(ctx, record)
}

// Stripe.

// HandleStripeEvent applies a verified webhook. Unknown event types are
// accepted and ignored, so enabling a new one in the Stripe dashboard is never
// an outage.
func (s *Service) HandleStripeEvent(ctx context.Context, event Event) error {
	switch event.Type {
	case "payment_intent.succeeded":
		var intent PaymentIntent
		if err := json.Unmarshal(event.Data.Object, &intent); err != nil {
			return err
		}
		return s.creditPayment(ctx, intent)

	case "payment_intent.payment_failed":
		var intent PaymentIntent
		if err := json.Unmarshal(event.Data.Object, &intent); err != nil {
			return err
		}
		tenant := intent.Metadata["tenant"]
		if tenant == "" || intent.Metadata["purpose"] != "auto_top_up" {
			return nil
		}
		reason := "payment failed"
		if intent.LastPaymentError != nil {
			reason = intent.LastPaymentError.Message
		}
		s.releaseTopUp(ctx, tenant, reason)
		return nil

	case "refund.created", "refund.updated":
		var refund Refund
		if err := json.Unmarshal(event.Data.Object, &refund); err != nil {
			return err
		}
		if refund.Status != "succeeded" || refund.PaymentIntent == "" {
			return nil
		}
		original, err := s.store.FindEntry(ctx, "stripe-pi:"+refund.PaymentIntent)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		id, err := s.randomHex(12)
		if err != nil {
			return err
		}
		_, _, err = s.store.AppendEntry(ctx, Entry{
			ID:             id,
			Tenant:         original.Tenant,
			Kind:           KindRefund,
			Amount:         -CentsToMicros(refund.Amount),
			IdempotencyKey: "stripe-refund:" + refund.ID,
			Reference:      refund.ID,
			At:             s.now(),
		})
		return err
	}
	return nil
}

// creditPayment turns a successful payment into credit, and activates a
// tenant on its first one. Idempotent on the payment intent, which is what makes
// the webhook and the synchronous auto-top-up path safe to both run.
func (s *Service) creditPayment(ctx context.Context, intent PaymentIntent) error {
	if intent.Status != "succeeded" || !strings.EqualFold(intent.Currency, "usd") || intent.AmountReceived <= 0 {
		return nil
	}
	tenant := intent.Metadata["tenant"]
	var record Tenant
	var err error
	if tenant != "" {
		record, err = s.store.GetTenant(ctx, tenant)
	} else {
		record, err = s.store.FindTenantByStripeCustomer(ctx, intent.Customer)
	}
	if errors.Is(err, ErrNotFound) {
		// A payment for something other than MOQOM credit on the same Stripe
		// account. Not ours to credit.
		return nil
	}
	if err != nil {
		return err
	}

	id, err := s.randomHex(12)
	if err != nil {
		return err
	}
	if _, _, err := s.store.AppendEntry(ctx, Entry{
		ID:             id,
		Tenant:         record.ID,
		Kind:           KindTopUp,
		Amount:         CentsToMicros(intent.AmountReceived),
		IdempotencyKey: "stripe-pi:" + intent.ID,
		Reference:      intent.ID,
		Detail:         intent.Metadata["purpose"],
		At:             s.now(),
	}); err != nil {
		return err
	}

	if intent.PaymentMethod != "" && (intent.SetupFutureUsage == "off_session" || record.PaymentMethod == "") {
		record.PaymentMethod = intent.PaymentMethod
	}
	if record.StripeCustomer == "" {
		record.StripeCustomer = intent.Customer
	}
	if record.Status == StatusPending {
		record.Status = StatusActive
	}
	if intent.Metadata["purpose"] == "auto_top_up" {
		record.TopUpPendingSince = time.Time{}
		record.LastTopUpError = ""
	}
	record.UpdatedAt = s.now()
	_, err = s.store.SaveTenant(ctx, record)
	return err
}

// SetStatus moves a tenant through its lifecycle.
func (s *Service) SetStatus(ctx context.Context, tenant string, to Status) (Tenant, error) {
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return Tenant{}, err
	}
	if record.Status == to {
		return record, nil
	}
	if !CanBecome(record.Status, to) {
		return Tenant{}, fmt.Errorf("%w: %s to %s", ErrTransition, record.Status, to)
	}
	record.Status = to
	record.UpdatedAt = s.now()
	return s.store.SaveTenant(ctx, record)
}
