// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"sort"
	"time"
)

// RoomAssessment is the detector's view of a room: how much of its audience is
// real, and whether its publisher is.
type RoomAssessment struct {
	Tenant, Room string
	At           time.Time
	Participants int
	// Suspected is viewers at or above the suspicion score.
	Suspected int
	// InflationShare is Suspected over viewers: the share of the count that is
	// probably not people.
	InflationShare float64
	Rings          []int
	Bursts         int
	// Publisher is the highest-scoring publisher's assessment, if any.
	Publisher *Assessment
	Reasons   []ReasonCode
}

// AssessRoom summarises a room. suspicion is the per-viewer score above which
// a viewer is counted as suspected; 0 uses 0.85.
func (d *Detector) AssessRoom(tenant, room string, suspicion float64, now time.Time) RoomAssessment {
	if suspicion == 0 {
		suspicion = 0.85
	}
	out := RoomAssessment{Tenant: tenant, Room: room, At: now}
	rings := map[int]bool{}
	viewers := 0
	for _, a := range d.AssessAll(tenant, room, now) {
		out.Participants++
		if a.publisher {
			if out.Publisher == nil || a.Score > out.Publisher.Score {
				copied := a
				out.Publisher = &copied
			}
			continue
		}
		viewers++
		if a.Score >= suspicion {
			out.Suspected++
		}
		if a.Has(ReasonJoinBurst) {
			out.Bursts++
		}
		if ring := d.ringOf(tenant, a.Subject.Account); ring != 0 {
			rings[ring] = true
		}
	}
	for ring := range rings {
		out.Rings = append(out.Rings, ring)
	}
	sort.Ints(out.Rings)
	if viewers > 0 {
		out.InflationShare = float64(out.Suspected) / float64(viewers)
	}
	if out.InflationShare >= 0.2 && out.Suspected >= 5 {
		out.Reasons = append(out.Reasons, "view_inflation")
	}
	if len(out.Rings) > 0 {
		out.Reasons = append(out.Reasons, ReasonRing)
	}
	if out.Publisher != nil && out.Publisher.Score >= suspicion {
		out.Reasons = append(out.Reasons, "suspect_publisher")
	}
	return out
}

// Attach adds externally produced evidence -- media verdicts, liveness
// outcomes -- to a participant. It holds until the profile expires.
func (d *Detector) Attach(subject Subject, reasons []Reason) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.profiles[subject.Key()]
	if p == nil {
		return false
	}
	for _, r := range reasons {
		replaced := false
		for i, existing := range p.external {
			if existing.Code == r.Code {
				p.external[i], replaced = r, true
			}
		}
		if !replaced && len(p.external) < 8 {
			p.external = append(p.external, r)
		}
	}
	return true
}
