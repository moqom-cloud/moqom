// Package moqom holds the MOQOM control plane.
//
// The split against the Rust side is deliberate and stable: Go owns everything
// that is a request/response over Postgres — identity, device registration,
// token minting, room records, moderation state, billing — and Rust owns
// everything on the media path. Nothing in this module sits between a
// publisher and a subscriber.
//
// Planned layout:
//
//	cmd/moqom-control  control plane API server
//	cmd/moqom-director client steering service
//	internal/identity  string user IDs, device JWK thumbprints
//	internal/token     CAT/CWT minting, DPoP verification, moqt-reval
//	internal/rooms     room records and participant state
//	internal/safety    policy engine and moderation actions
//
// Empty until Phase 3; the module exists now so the import path is settled
// before anything depends on it.
package moqom
