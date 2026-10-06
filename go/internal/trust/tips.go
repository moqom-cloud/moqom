// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"context"
	"time"
)

// The boundary with MOTOM, the transaction layer.
//
// MOTOM owns money: it decides whether a tip settles, holds, or is refunded.
// This package owns the opinion about whether the people involved are real.
// The contract between them is these types and nothing else -- MOTOM never
// sees a score, a feature or a rule, and this package never sees an amount
// beyond its order of magnitude. Implemented on MOTOM's side; nothing here
// imports it.

// TipObservation is what MOTOM tells trust about a tip before settling it.
type TipObservation struct {
	TipID string
	// From and To are participants, as MOTOM knows them.
	Tenant, Room             string
	FromUsername, FromDevice string
	ToUsername               string
	// AmountBand is floor(log10(amount in micros)).
	AmountBand int
	At         time.Time
}

// TipVerdict is trust's opinion of one tip.
type TipVerdict struct {
	TipID string
	// Band is the sender's risk band at the time of the tip.
	Band Band
	// Hold recommends MOTOM delay settlement for review. MOTOM decides.
	Hold bool
	// Reasons are codes for MOTOM's reviewers, never for either party.
	Reasons []ReasonCode
	Model   string
}

// TipRiskSink is implemented by MOTOM to receive verdicts asynchronously, for
// tips whose risk changes after they were scored (a sender later found to be
// part of a ring).
type TipRiskSink interface {
	FlagTip(ctx context.Context, verdict TipVerdict) error
}

// TipScorer answers MOTOM synchronously.
type TipScorer struct {
	Detector   *Detector
	Pseudonyms *Pseudonymiser
	Exclusions *Exclusions
	// HoldFrom is the least band whose tips are held. Default BandHigh.
	HoldFrom Band
}

var bandRank = map[Band]int{BandLow: 0, BandElevated: 1, BandHigh: 2, BandCritical: 3}

// Score rates a tip and records it as a cadence signal. A sender who is
// excluded, or in the same ring as the recipient -- the shape of a farm
// tipping its own streamer to launder money or game a leaderboard -- is held
// whatever their band.
func (s *TipScorer) Score(_ context.Context, tip TipObservation) TipVerdict {
	from := SubjectFor(s.Pseudonyms, tip.Tenant, tip.Room, tip.FromUsername, tip.FromDevice)
	s.Detector.Observe(Event{At: tip.At, Kind: KindTip, Source: SourceControl, Subject: from, AmountBand: tip.AmountBand})
	verdict := TipVerdict{TipID: tip.TipID, Band: BandLow}
	a, ok := s.Detector.Assess(from, tip.At)
	if ok {
		verdict.Band, verdict.Model = a.Band, a.Model
		for _, r := range a.Reasons {
			verdict.Reasons = append(verdict.Reasons, r.Code)
		}
	}
	hold := s.HoldFrom
	if hold == "" {
		hold = BandHigh
	}
	verdict.Hold = bandRank[verdict.Band] >= bandRank[hold]
	if s.Exclusions != nil && s.Exclusions.Excluded(from) {
		verdict.Hold = true
	}
	to := s.Pseudonyms.Tenant(tip.Tenant, "account", tip.ToUsername)
	if ring := s.Detector.ringOf(tip.Tenant, from.Account); ring != 0 && ring == s.Detector.ringOf(tip.Tenant, to) {
		verdict.Hold = true
		verdict.Reasons = append(verdict.Reasons, ReasonRing)
	}
	return verdict
}

func (d *Detector) ringOf(tenant string, account Pseudonym) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tenant(tenant).graph.RingOf(account)
}
