# Proposals for MoQ Transport draft-23

MOQOM implements Media over QUIC Transport (`draft-ietf-moq-transport`) twice, in a Rust relay
and in a Swift client. These are changes we intend to propose for the next revision, based on
building both.

Status: proposals, not yet submitted. Nothing here is in any draft until the working group
adopts it.

## How this works

`draft-ietf-moq-transport-23` is written by the draft's editors and published by the IETF MoQ
working group. Anyone can contribute to it in three ways:

- File an issue or pull request on the working group repository,
  [github.com/moq-wg/moq-transport](https://github.com/moq-wg/moq-transport).
- Raise it on the MoQ mailing list or at a working group meeting.
- Post an individual Internet-Draft for features that belong in an extension rather than in the
  core protocol.

The working group decides what goes into -23 or later. We file each change below as its own
issue, so it can be accepted or rejected separately.

## 1. A PUBLISH_DONE code for an expired token

**Problem.** In draft-22, `EXPIRED_AUTH_TOKEN` exists as a session termination code, a request
error code and a stream reset code, but not as a Publish Done code (§12.4). When the token that
authorised an established subscription expires, a publisher can only end the subscription with
`UNAUTHORIZED` (0x1) or `INTERNAL_ERROR` (0x0). The subscriber can't tell whether it should
refresh its token or give up.

**Proposal.**

- Add `EXPIRED_AUTH_TOKEN` to the PUBLISH_DONE Codes registry as 0x7. That value is unused
  there, and it's the value `EXPIRED_AUTH_TOKEN` already has as a Stream Reset code.
- In PUBLISH_DONE (§9.9): a publisher SHOULD use this code when it ends a subscription because
  the authorizing token expired. The subscriber can subscribe again with a refreshed token, or
  avoid the interruption by sending a REQUEST_UPDATE with a new AUTHORIZATION_TOKEN before
  expiry.
- The code refers to the subscriber's own token. A relay whose upstream credentials lapse
  doesn't pass it on to downstream subscribers, who can't fix that.

This gives subscriptions the recovery path that PUBLISH_NAMESPACE already has (§4.1).

**Size.** One registry entry, one code definition and a short paragraph. It goes to the
working group as an issue and a pull request against the core draft, kept separate from
proposal 4 so each can be accepted on its own.

## 2. Distinguishing Property types from Message Parameter types

**Problem.** Track Properties and Message Parameters are separate registries, and both use small
numbers. Some values look related but aren't: DEFAULT PUBLISHER PRIORITY is Property Type
`0x0E` (§10.4), while SUBSCRIBER_PRIORITY is Message Parameter `0x20` (§9.20.7). One of our two
implementations sent the property value as the parameter. Under draft-22 the receiver closes the
session with PROTOCOL_VIOLATION (§9.20). That fails loudly, but the error doesn't point at the
cause, and a value that a later draft defines as a parameter would be misread instead.

**Proposal.** This is an editorial change:

- In §9.20.7 and §10.4, state which registry each priority value belongs to.
- Add a short note that Property types and Message Parameter types are separate namespaces, and
  that the same number can mean different things in each.

**Size.** Editorial.

## 3. Shared test vectors

**Problem.** Implementations find wire-format bugs late, usually at interop events. Draft-22
changes the encoding of several messages (LOCATION_FILTER, FILL_PARAMETERS and the Type Flags
bitfields), which makes this more likely.

**Proposal.** We contribute byte-exact encode and decode vectors for every draft-22 control
message and parameter to the working group's interop effort. Our two codecs were written
separately and are checked against each other.

**Size.** No change to the draft text. This would live in the interop repository.

## 4. Loop protection between relays (separate extension)

**Problem.** §4.1 says PUBLISH_NAMESPACE "does not protect against loops", and draft-22 places
relay-to-relay interconnection out of scope (§7). Relays that forward requests to a configured
upstream, as content delivery networks pull from an origin, have no in-protocol way to stop a
request from circling a misconfigured cycle.

The working group has said it sees loop prevention as an extension
([moq-wg/moq-transport#1798](https://github.com/moq-wg/moq-transport/issues/1798)). One
extension already exists: `draft-lcurley-moq-cluster`. It prevents loops in namespace
advertisements by carrying the path each advertisement took. It doesn't cover requests that a
relay forwards without an advertisement, which is the gap this proposal fills.

**Proposal.** An individual Internet-Draft, `draft-elliottwave-moq-hop-limit`
(`docs/ietf/draft-elliottwave-moq-hop-limit.md`), designed to work alongside the cluster extension:

- a `HOP_LIMIT` Message Parameter on SUBSCRIBE, FETCH, TRACK_STATUS, SUBSCRIBE_NAMESPACE and
  SUBSCRIBE_TRACKS, which each relay clamps to its own maximum and decrements when it forwards;
- a `HOP_LIMIT_EXCEEDED` REQUEST_ERROR code for when the limit reaches zero;
- a rule for aggregated requests, so two relays that each attach a request to the other's
  pending one don't wait on each other until a timeout;
- negotiation by a zero-length Setup Option, as §6.3.2 describes.

**Status.** Written and builds with the IETF tools. Not yet posted.

## 5. Editorial fixes found while implementing

- RENDEZVOUS_TIMEOUT (§9.20.6) gives a unit (milliseconds) but not an encoding. We assume a
  varint.
- §3.4 refers to a "zero-length" LOCATION_FILTER inside FILL_PARAMETERS. LOCATION_FILTER has no
  length field of its own, so this probably means Location Filter Type 0x00. The text should say
  so.

**Size.** Editorial.

## 6. Implementation report

Once both codecs speak draft-22, we'll send the working group an implementation report. It will
say which features we implemented and which we didn't, what was unclear, and our interop
results against other implementations.

## Not proposals

These are things we're doing to match the drafts, not changes we're asking for:

- Moving from draft-19 to draft-22. This covers fill fetch streams, LOCATION_FILTER,
  PUBLISH_STATE_NOTIFY and INCLUDE_PROPERTIES.
- Aligning our media metadata with the MoQ media format drafts (LOC) rather than defining our
  own.
- Encoding tokens as Common Access Tokens, as C4M specifies (`draft-ietf-moq-c4m`).
