// Package applesignin keeps each tenant's settings for Apple sign-in hosted by
// MOQOM, and the App Attest counters that stop an assertion being replayed.
package applesignin

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// Settings are one tenant's: whose apps may sign in, where, and to do what.
type Settings struct {
	Tenant string
	// TeamID is the Apple team the apps are signed by.
	TeamID string
	// BundleIDs are the apps. An identity token must be issued to one, and an
	// App Attest key attested for one.
	BundleIDs []string
	// Rooms are the existing rooms a signed-in person may be minted for.
	Rooms []string
	// Publish is what they may publish there. Never moderation or ownership.
	Publish control.PublishScope
	// AllowDevelopment accepts App Attest keys from Apple's development
	// environment, for builds run from Xcode.
	AllowDevelopment bool
	UpdatedAt        time.Time
}

// AppIDs are the App Attest app identifiers, "TEAMID.bundle".
func (s Settings) AppIDs() []string {
	ids := make([]string, 0, len(s.BundleIDs))
	for _, bundle := range s.BundleIDs {
		ids = append(ids, s.TeamID+"."+bundle)
	}
	return ids
}

// ErrNotFound is a tenant that has not turned Apple sign-in on.
var ErrNotFound = errors.New("Apple sign-in is not set up for this tenant")

// Store keeps settings and counters.
type Store interface {
	// Get returns a tenant's settings, or ErrNotFound.
	Get(ctx context.Context, tenant string) (Settings, error)
	// Put replaces a tenant's settings.
	Put(ctx context.Context, settings Settings) error
	// Advance records counter for an App Attest key if it is higher than the
	// last one recorded, and reports whether it was. Atomic, so two replicas
	// cannot both accept one assertion.
	Advance(ctx context.Context, keyID string, counter uint32) (bool, error)
	// Counter is the last counter recorded for a key; zero if none.
	Counter(ctx context.Context, keyID string) (uint32, error)
}

// Memory is a Store in memory, for a single replica and for tests.
type Memory struct {
	mu       sync.Mutex
	settings map[string]Settings
	counters map[string]uint32
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{settings: map[string]Settings{}, counters: map[string]uint32{}}
}

func (m *Memory) Get(_ context.Context, tenant string) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings, ok := m.settings[tenant]
	if !ok {
		return Settings{}, ErrNotFound
	}
	return settings, nil
}

func (m *Memory) Put(_ context.Context, settings Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[settings.Tenant] = settings
	return nil
}

func (m *Memory) Advance(_ context.Context, keyID string, counter uint32) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if counter <= m.counters[keyID] {
		return false, nil
	}
	m.counters[keyID] = counter
	return true, nil
}

func (m *Memory) Counter(_ context.Context, keyID string) (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counters[keyID], nil
}
