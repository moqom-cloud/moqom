// Copyright 2026 Elliott Wave LLC. Proprietary and confidential; see LICENSING.md.
// CONFIDENTIAL: describes patent-pending methods. Never publish this package,
// in any licence, before the filings in docs/INVENTION-DISCLOSURE-TRUST.md.

// Package trust decides how likely a participant is to be automated, and what
// to do about it.
//
// It is the private half of trust and safety. Every feature, threshold, model
// and score lives here and nowhere else: docs/PATENT-ASSESSMENT.md explains why
// a mechanism that reaches an Apache-2.0 SDK is licensed to everyone, and a
// mechanism a client can read is a mechanism a bot farm can read. The public
// SDKs and the Apache-2.0 contract in proto/ gain nothing from this package.
//
// # Shape
//
// Signals arrive as [Event]s, already pseudonymised: the ingest layer resolves
// an address to a network class and an ASN and then forgets it, so nothing in
// here can hold a raw IP for longer than the call that classified it. Events
// fold into a per-participant [Profile]. A [Detector] scores a profile from
// four independent sources -- known-pattern rules, a per-tenant anomaly model,
// a per-tenant supervised model trained on moderator decisions, and ring
// membership from the coordination graph -- and combines them in log-odds into
// a calibrated [Assessment]. A [Policy] turns an assessment into a graded
// [Action], and everything that acts is written to an [AuditLog].
//
// # What the subject sees
//
// Nothing. Scores, reasons and bands go to the tenant's moderators and to
// MOQOM's own reviewers, never to the participant they describe, and automated
// actions are delayed by a random interval so that a farm probing the system
// cannot line an outcome up with the change that caused it.
//
// Synthetic-media detection is in media.go. It is a different problem with a
// different honesty budget, and the comment there says what it cannot do.
package trust
