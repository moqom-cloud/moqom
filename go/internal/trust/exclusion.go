// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"context"
	"strings"
	"sync"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// SubjectFor builds the pseudonymous subject for a participant the control
// plane knows by name. Every caller that crosses from identities into this
// package goes through here, so the scopes cannot drift apart.
func SubjectFor(p *Pseudonymiser, tenant, room, username, device string) Subject {
	return Subject{
		Tenant:       tenant,
		Room:         room,
		Account:      p.Tenant(tenant, "account", username),
		Device:       p.Tenant(tenant, "device", device),
		GlobalDevice: p.Platform("device", device),
	}
}

// Exclusions is who is left out of viewer counts, billable usage and tip
// eligibility. It is the one output of this package that changes numbers a
// tenant sees, which is why it holds subjects and not scores: the decision is
// made once, audited, and then only looked up.
type Exclusions struct {
	mu    sync.RWMutex
	p     *Pseudonymiser
	set   map[string]bool
	rooms map[string]int
}

// NewExclusions uses the same pseudonymiser as ingest.
func NewExclusions(p *Pseudonymiser) *Exclusions {
	return &Exclusions{p: p, set: map[string]bool{}, rooms: map[string]int{}}
}

// Set includes or excludes a subject.
func (x *Exclusions) Set(s Subject, excluded bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key, room := s.Key(), s.Tenant+"\x00"+s.Room
	switch {
	case excluded && !x.set[key]:
		x.set[key] = true
		x.rooms[room]++
	case !excluded && x.set[key]:
		delete(x.set, key)
		x.rooms[room]--
	}
}

// Excluded reports whether a subject is excluded.
func (x *Exclusions) Excluded(s Subject) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.set[s.Key()]
}

// ExcludedIn is how many are excluded in a room.
func (x *Exclusions) ExcludedIn(tenant, room string) int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.rooms[tenant+"\x00"+room]
}

// Countable is the control plane's viewer-count filter: false for a
// participant who should not be counted. Publishers are always counted --
// a flagged host is a moderation question, not an audience one.
func (x *Exclusions) Countable(tenant, room string, participant control.Participant) bool {
	if participant.Grants.Publish != 0 {
		return true
	}
	return !x.Excluded(SubjectFor(x.p, tenant, room, participant.Identity.Username, participant.Identity.Device))
}

// Unbillable is billing's usage filter. participant is "username/device", as
// control.Identity renders it.
func (x *Exclusions) Unbillable(_ context.Context, tenant, room, participant string) (bool, string) {
	username, device, _ := strings.Cut(participant, "/")
	if x.Excluded(SubjectFor(x.p, tenant, room, username, device)) {
		return true, "trust_excluded"
	}
	return false, ""
}
