> **DRAFT, NOT SUBMITTED. Elliott Wave LLC.** Do not post to the MoQ list or
> the datatracker until the owner approves. Before submission: confirm the
> filings in `PATENT-ASSESSMENT.md` rows 3, 4 and 5 are on file (this draft
> touches none of them, but submission starts IETF participation and BCP 79
> disclosure obligations), re-check every section reference against the
> current transport draft (-22 as of 1 October 2026; the working group is past
> -20), and convert to kramdown-rfc or xml2rfc for the datatracker.

---
title: A Hop Limit for Relay-to-Relay Subscriptions in Media over QUIC Transport
abbrev: MOQT Relay Hops
docname: draft-elliottwave-moq-relay-hops-00
category: std
ipr: trust200902
area: Web and Internet Transport
workgroup: Media Over QUIC
keyword: [moq, moqt, relay, loop]
author:
  - name: Elliott
    org: Elliott Wave LLC
normative:
  MOQT: I-D.ietf-moq-transport
  RFC2119:
  RFC8174:
---

## Abstract

Media over QUIC Transport (MOQT) relays can forward a SUBSCRIBE to another
relay when they cannot serve a track themselves. MOQT notes that namespace
publication "does not protect against loops" and defines no hop limit, so two
relays that each treat the other as upstream can forward the same subscription
indefinitely. This document defines a RELAY_HOPS Message Parameter, negotiated
by a Setup Option, that bounds how many relays a subscription may traverse.

## 1. Introduction

Relay fleets are configured by hand and by automation, and either can produce
a cycle: relay A names B as its upstream for a namespace and B names A. A
SUBSCRIBE for a track neither holds then circulates until a timeout or
resource limit ends it, consuming a session slot and upstream bandwidth on
every relay in the cycle.

A relay cannot detect such a cycle locally, because a SUBSCRIBE from a peer
relay is indistinguishable from one from an end client. This document carries
a decreasing counter on the subscription itself, so a cycle ends in a refusal
after a bounded number of forwards. The technique is the IP TTL and HTTP
Max-Forwards, applied to MOQT.

## 2. Conventions

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD",
"SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this
document are to be interpreted as described in BCP 14 [RFC2119] [RFC8174]
when, and only when, they appear in all capitals, as shown here.

## 3. Negotiation

MOQT requires a receiver to close the session with PROTOCOL_VIOLATION on an
unknown Message Parameter, so RELAY_HOPS MUST NOT be sent unless the peer has
indicated support.

An endpoint indicates support by including the RELAY_HOPS_SUPPORTED Setup
Option (TBD1) with no value in its SETUP. A relay MUST NOT include RELAY_HOPS
on any message to a peer that did not include RELAY_HOPS_SUPPORTED.

## 4. The RELAY_HOPS Parameter

RELAY_HOPS (TBD2) is a Message Parameter on SUBSCRIBE and FETCH whose value is
a variable-length integer: the number of further relays the request may be
forwarded through.

Each relay keeps a configured maximum, MAX_HOPS. The RECOMMENDED default is 3,
which covers an origin, one interior tier and an edge; fan-out grows
multiplicatively with depth, so deeper trees are rarely needed and add latency
to every subscriber.

On receiving a SUBSCRIBE or FETCH, a relay computes its remaining budget:

- If RELAY_HOPS is absent, the remaining budget is MAX_HOPS.
- If present, the remaining budget is the smaller of its value and MAX_HOPS.

If the relay can serve the request locally, it does so and the budget is not
used.

Otherwise, if the remaining budget is zero, the relay MUST NOT forward the
request and MUST respond with an error (Section 5).

Otherwise the relay forwards the request upstream. If the upstream peer
negotiated support, the relay MUST include RELAY_HOPS with a value one less
than its remaining budget. If the upstream peer did not negotiate support, the
relay forwards without the parameter, and the bound holds only across the
relays that support it.

A relay MUST NOT increase the value it received when forwarding. An end client
SHOULD NOT send RELAY_HOPS.

## 5. Error Handling

A relay that refuses to forward because the budget is exhausted responds with
REQUEST_ERROR using error code HOP_LIMIT_EXCEEDED (TBD3). This is not a
protocol violation and MUST NOT close the session; the subscriber sees an
unresolved track, which is the visible, cheap outcome of a misconfigured
topology.

A value that is not a valid variable-length integer is a PROTOCOL_VIOLATION.

## 6. Security Considerations

RELAY_HOPS reduces the amplification available from a misconfigured or
malicious topology: one SUBSCRIBE can cause at most MAX_HOPS upstream
requests. A peer can lower the budget it sends, which only causes its own
requests to fail sooner. Clamping on receipt prevents a peer from raising
another relay's budget above its configured maximum.

The parameter reveals at most that a request came through a relay and how
many hops remain; deployments concerned about topology disclosure can send
the same value on every forward from a given tier.

## 7. IANA Considerations

This document requests:

- a Setup Option, RELAY_HOPS_SUPPORTED (TBD1), in the MOQT Setup Options
  registry;
- a Message Parameter, RELAY_HOPS (TBD2), even type (variable-length integer),
  in the MOQT Message Parameters registry;
- an error code, HOP_LIMIT_EXCEEDED (TBD3), in the MOQT REQUEST_ERROR codes
  registry.

The MOQOM implementation uses `0x4D50` for RELAY_HOPS while unregistered and
will move to the assigned codepoints.

## 8. Implementation Status

MOQOM's relay (Rust) implements the parameter with MAX_HOPS = 3, clamping on
receipt and refusal at zero, with tests for each case. It does not yet
negotiate support via a Setup Option (Section 3); that change ships before
this draft is submitted.

## Acknowledgments

TBD.
