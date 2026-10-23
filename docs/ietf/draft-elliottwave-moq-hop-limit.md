---
title: "Hop Limit for Requests Forwarded Between Media over QUIC Relays"
abbrev: "moq-hop-limit"
category: std

docname: draft-elliottwave-moq-hop-limit-latest
submissiontype: IETF
number:
date:
v: 3
area: wit
workgroup: moq
keyword:
 - media over quic
 - relay
 - loop prevention
 - hop limit

author:
 -
    fullname: Justin Elliott Staffer
    organization: Elliott Wave LLC
    email: hello@elliottwave.studio

normative:
  MOQT: I-D.ietf-moq-transport

informative:
  CLUSTER: I-D.lcurley-moq-cluster
  RFC0791:
  RFC8200:
  RFC9110:
  RFC7942:

--- abstract

A Media over QUIC Transport (MOQT) relay that cannot serve a request
locally can forward it to an upstream relay.  MOQT defines no bound on how
many times a request may be forwarded, so a misconfigured set of relays can
forward one request around a cycle until a timeout or resource limit ends
it.  This document defines a HOP_LIMIT Message Parameter, negotiated with a
Setup Option, that each relay decrements when it forwards a request, and a
HOP_LIMIT_EXCEEDED error that a relay returns when the limit is exhausted.
It complements extensions that prevent loops in namespace advertisement by
covering requests that relays forward without an advertisement.

--- note_Note_to_Readers

Discussion of this document takes place on the Media Over QUIC Working
Group mailing list (moq@ietf.org).

This document targets draft-ietf-moq-transport-22.  Section references to
{{MOQT}} are to that version.

--- middle

# Introduction

MOQT relays form distribution trees: a relay that has no local publisher or
cached copy for a requested track forwards the request toward one
({{Section 7 of MOQT}}).  Many deployments choose where to forward by
configuration rather than by advertisement, as content delivery networks
pull from an origin.  An edge relay names a parent for a set of namespaces,
the parent names its own, and so on.

Configuration produces cycles.  If relay A is configured to forward a
namespace to relay B, and B to A, a SUBSCRIBE for a track that neither
holds is forwarded from A to B to A indefinitely.  Each forward consumes a
request stream, Request ID credit, and state on both relays.  Neither relay
can detect the cycle locally: a request that arrives from a peer relay is
the same message, on the same kind of session, as one from an end
subscriber.

{{MOQT}} notes that PUBLISH_NAMESPACE "does not protect against loops"
({{Section 4.1 of MOQT}}) and places relay-to-relay interconnection out of
scope ({{Section 7 of MOQT}}).  This document does not define relay
interconnection or routing.  It adds one loop-safety property to any
interconnection that forwards MOQT requests between relays: a request can
be forwarded through at most a bounded number of relays, after which it
fails with a specific error.  The technique is the Time to Live of
{{RFC0791}}, the Hop Limit of {{RFC8200}}, and the Max-Forwards header of
{{Section 7.6.2 of RFC9110}}, applied to MOQT requests.

## Relationship to the Cluster Extension

{{CLUSTER}} prevents loops in namespace advertisements by carrying the path
an advertisement has traversed, and it selects the upstream for a request
from those advertisements.  That covers meshes in which relays learn routes
from PUBLISH_NAMESPACE and NAMESPACE.

This document covers requests that a relay forwards without an
advertisement to follow: to a configured parent, to an origin chosen by
policy, or after following a REDIRECT.  The two mechanisms are independent
and can be negotiated together.  A relay that negotiates both applies the
path rules of {{CLUSTER}} when it selects an upstream, and the hop limit of
this document when it forwards the request.

# Conventions and Definitions

{::boilerplate bcp14-tagged}

This document uses the terms defined in {{MOQT}}.  In addition:

Forwarded request:
: A request that a relay sends to an upstream peer because it received a
  request from a downstream peer that it could not satisfy from local
  state.  This includes upstream FETCHes that a relay issues to fill gaps
  in a downstream FETCH ({{Section 3.2.3 of MOQT}}).

Covered request:
: A SUBSCRIBE ({{Section 9.6 of MOQT}}), FETCH ({{Section 9.11 of MOQT}}),
  TRACK_STATUS ({{Section 9.13 of MOQT}}), SUBSCRIBE_NAMESPACE
  ({{Section 9.15 of MOQT}}) or SUBSCRIBE_TRACKS ({{Section 9.18 of MOQT}})
  message.  These are the requests a relay forwards upstream on behalf of a
  downstream peer.

Remaining hop limit:
: The number of further relays to which a request may be forwarded,
  computed by a relay as described in {{receiving}}.

MAX_HOP_LIMIT:
: A relay's configured maximum for the remaining hop limit
  ({{choosing}}).

# Negotiation {#negotiation}

{{MOQT}} requires an endpoint to close the session with PROTOCOL_VIOLATION
when it receives a Message Parameter that was not defined in the negotiated
version or negotiated through Setup Options ({{Section 9.20 of MOQT}}).
The HOP_LIMIT parameter is therefore sent only on sessions where this
extension is negotiated.

An endpoint declares support by including the HOP_LIMIT_SUPPORTED Setup
Option in its SETUP message ({{Section 9.1 of MOQT}}):

~~~
HOP_LIMIT_SUPPORTED Setup Option {
  Option Type (vi64) = TBD1,
  Option Length (vi64) = 0,
}
~~~
{: title="HOP_LIMIT_SUPPORTED Setup Option"}

The Option Type is odd, so the option is length-prefixed, and its value is
empty: presence alone declares support, as described in
{{Section 6.3.2 of MOQT}}.  An endpoint that receives HOP_LIMIT_SUPPORTED
with a non-empty value MUST close the session with PROTOCOL_VIOLATION.  An
endpoint MUST NOT include the option more than once.

The extension is negotiated on a session when both endpoints included
HOP_LIMIT_SUPPORTED in SETUP.  Negotiation is per session: a relay MUST NOT
infer that a peer supports the extension on one session because it did on
another.

Relays that forward covered requests SHOULD declare support.  Endpoints
that never forward requests, such as end subscribers and original
publishers, MAY declare support; doing so lets them receive HOP_LIMIT
without a session error ({{non-relays}}).

This extension can be used with draft-ietf-moq-transport-22 and later
versions that keep the extension negotiation of
{{Section 6.3.2 of MOQT}}.

# The HOP_LIMIT Parameter {#parameter}

HOP_LIMIT (Parameter Type TBD2) is a Message Parameter whose value is a
varint ({{Section 9.20 of MOQT}}): the number of further relays to which
the request carrying it may be forwarded.

HOP_LIMIT MAY appear in any covered request.  It MUST NOT appear in any
other message, including REQUEST_UPDATE and the FILL_PARAMETERS parameter
({{Section 9.20.15 of MOQT}}).  A relay's budget for a request is fixed
when the request is made; an update cannot extend it.  An endpoint that
receives HOP_LIMIT in any other message MUST close the session with
PROTOCOL_VIOLATION, as {{Section 9.20.1 of MOQT}} requires.

A covered request MUST NOT contain more than one HOP_LIMIT parameter.

An endpoint MUST NOT send HOP_LIMIT on a session where the extension was
not negotiated, and a receiver of HOP_LIMIT on such a session closes the
session with PROTOCOL_VIOLATION as {{Section 9.20 of MOQT}} requires.

Like every Message Parameter, HOP_LIMIT applies to one hop and is not
copied to the forwarded request ({{Section 9.20.1 of MOQT}}).  A relay
computes the value it sends from the value it received.

# Relay Behavior {#relay}

## Computing the Remaining Hop Limit {#receiving}

When a relay receives a covered request, it computes the remaining hop
limit:

- If the request carries no HOP_LIMIT, the remaining hop limit is
  MAX_HOP_LIMIT.  This is the case for a request from an end subscriber, or
  from a peer that did not negotiate the extension.

- If the request carries HOP_LIMIT, the remaining hop limit is the smaller
  of its value and MAX_HOP_LIMIT.

A relay MUST NOT compute a remaining hop limit greater than MAX_HOP_LIMIT.
Clamping on receipt means a peer cannot raise another relay's bound, and a
cycle through relays with different maximums is bounded by the smallest.

## Serving a Request Without Forwarding

A relay that can satisfy a covered request without forwarding it, from a
local publisher, its cache, or an Established upstream subscription, does
so as {{MOQT}} specifies.  The remaining hop limit is not consumed and has
no effect on the response.

## Forwarding a Request {#forwarding}

A relay that would forward a covered request upstream first checks the
remaining hop limit:

- If the remaining hop limit is zero, the relay MUST NOT forward the
  request.  It MUST respond to the downstream request with REQUEST_ERROR
  and error code HOP_LIMIT_EXCEEDED ({{errors}}).

- Otherwise, the relay MAY forward the request.  If the upstream session
  negotiated the extension, the forwarded request MUST carry HOP_LIMIT with
  a value one less than the remaining hop limit.

Every upstream request that a relay issues to satisfy one downstream
request carries the same value.  This includes the several upstream
FETCHes that one downstream FETCH can generate
({{Section 3.2.3 of MOQT}}), and requests retried to a different upstream
after a failure.

A relay MUST NOT send a HOP_LIMIT value greater than or equal to the
remaining hop limit of the downstream request that caused it.  The value
strictly decreases with each forward, so a request cannot be forwarded more
than MAX_HOP_LIMIT times through relays that implement this extension,
however they are connected.

## Aggregated Requests {#aggregation}

A relay MAY satisfy a new downstream request by attaching it to an upstream
request it already sent for the same track or namespace prefix
({{Section 7.4 of MOQT}}).  Attaching consumes no hop limit, but it can
create a cycle that the hop limit alone does not end.  Suppose relay A
forwards a SUBSCRIBE to B and is waiting for the reply.  B forwards it back
to A, and A attaches it to its own pending upstream request.  A now waits
for B and B waits for A.  A relay sends SUBSCRIBE_OK only once its upstream
subscription is Established ({{Section 7.4 of MOQT}}), so both wait until a
timeout ends the request.

To prevent this, a relay MUST NOT attach a downstream covered request to a
pending upstream request if the downstream request's remaining hop limit
is smaller than the remaining hop limit of the downstream request that
caused the pending one.  A request that has gone around a cycle always
returns with a smaller remaining hop limit than it left with, so it is
forwarded again rather than attached, and it fails with HOP_LIMIT_EXCEEDED
after a bounded number of forwards.  That error then completes the pending
requests in the cycle, as {{errors}} describes.

A pending upstream request is one that has been sent and has not received
SUBSCRIBE_OK, FETCH_OK, REQUEST_OK, TRACK_STATUS_OK, or REQUEST_ERROR.  A
relay MAY attach any downstream request to an Established upstream
subscription.  An Established subscription means a publisher was found, so
there is no cycle to break.

When relays at different depths request the same track at the same time,
this rule can make a relay send two upstream requests where it would have
sent one.  The duplicate lasts only while the first request is pending.

## Following Redirects {#redirects}

A relay that follows a REDIRECT ({{Section 12.3 of MOQT}}) from upstream is
still forwarding the original downstream request.  The new request MUST
carry the same HOP_LIMIT value as the request that was redirected, and the
relay MUST NOT reset the remaining hop limit to MAX_HOP_LIMIT.  A relay
SHOULD bound the number of redirects it follows for one downstream request,
because a redirect does not consume hop limit at the relay that follows it.

## Upstream Peers Without the Extension {#partial}

A relay MAY forward a covered request to an upstream peer that did not
negotiate the extension.  It sends the request without HOP_LIMIT, and that
peer computes MAX_HOP_LIMIT for the request.  The bound therefore holds
across each run of consecutive relays that implement this extension, and a
cycle passes through a relay that does not implement it starts over at
that relay's MAX_HOP_LIMIT.

A cycle made only of relays that implement this extension is always
bounded.  Operators that need the bound across their whole topology deploy
the extension on every relay that can forward requests.

## Endpoints That Are Not Relays {#non-relays}

An end subscriber SHOULD NOT send HOP_LIMIT.  It has forwarded nothing, and
a relay treats an absent HOP_LIMIT as MAX_HOP_LIMIT.

An original publisher, or any endpoint that serves a covered request
without forwarding it, ignores a received HOP_LIMIT, provided the extension
was negotiated on that session.

## Choosing MAX_HOP_LIMIT {#choosing}

MAX_HOP_LIMIT SHOULD be configurable.  It should be at least the number of
upstream relays on the longest legitimate path from an edge relay to an
original publisher.  For example, a topology with an edge tier, one
interior tier and an origin forwards a request at most twice, from the edge
to the interior relay and from the interior relay to the origin.  It needs
a MAX_HOP_LIMIT of 2.

Fan-out grows multiplicatively with each tier while latency grows
additively, so deep relay trees are uncommon.  Implementations SHOULD use a
small default; this document RECOMMENDS a default of 3.  A larger value
lengthens the work a cycle does before it fails, and does nothing for
topologies without cycles.

All relays in a deployment SHOULD use the same MAX_HOP_LIMIT.  Because each
relay clamps on receipt, a relay with a smaller value shortens the paths
that pass through it.

# Error Handling {#errors}

A relay that refuses to forward a request because its remaining hop limit
is zero responds with REQUEST_ERROR ({{Section 9.4 of MOQT}}) and the error
code HOP_LIMIT_EXCEEDED (TBD3).  The relay SHOULD set Retry Interval to 0.
Retrying the same request through the same topology fails the same way.

HOP_LIMIT_EXCEEDED indicates a configuration problem, not a protocol
violation, and MUST NOT cause either endpoint to close the session.

A relay that receives HOP_LIMIT_EXCEEDED for an upstream request SHOULD
respond with HOP_LIMIT_EXCEEDED to every downstream request that is waiting
for it, including requests attached under {{aggregation}}.  New error codes
can be used without negotiation, and a receiver that does not recognize
the code treats it as INTERNAL_ERROR ({{Section 13 of MOQT}}).  A relay can
therefore send HOP_LIMIT_EXCEEDED to a downstream peer whether or not that
peer negotiated this extension.

A relay MAY cache a HOP_LIMIT_EXCEEDED response and use it for matching
requests that arrive with the same or a smaller remaining hop limit.  It
SHOULD NOT keep the cached response longer than a short, configured
interval, because the misconfiguration that caused it may be corrected at
any time.

A HOP_LIMIT value that is not a valid varint is a malformed control
message, which {{MOQT}} treats as a PROTOCOL_VIOLATION.

# Operational Considerations

A HOP_LIMIT_EXCEEDED response is the first sign of a cycle or of a path
deeper than MAX_HOP_LIMIT.  Relays SHOULD log it with the track or
namespace and the upstream peer, and SHOULD count it in their metrics, so
that operators can find the misconfiguration.

The extension bounds the work a cycle causes but does not repair the
cycle.  Subscribers to an affected namespace receive an error until the
configuration is corrected.

# Security Considerations

Without a hop limit, one downstream request into a cycle of relays causes
an unbounded number of forwarded requests, each holding a request stream
and state on two relays.  With this extension, one downstream request
causes at most MAX_HOP_LIMIT forwards through relays that implement it,
multiplied by any retries and redirects each relay follows
({{forwarding}}, {{redirects}}).  Implementations SHOULD bound retries and
redirects for this reason.

A peer can send a smaller HOP_LIMIT than it received, which only makes its
own requests fail sooner.  It cannot raise another relay's bound, because
each relay clamps the value on receipt ({{receiving}}).  A malicious relay
can always issue new requests of its own, so this extension protects
against misconfiguration and limits amplification through honest relays.
It does not protect against a hostile relay.

The HOP_LIMIT value tells the upstream peer how many more forwards a
request may take, which hints at the downstream peer's position in a
topology.  A relay that considers this sensitive can send the same value on
every request it forwards across a trust boundary, at the cost of a weaker
bound for cycles that cross the boundary.

The considerations of {{Section 15 of MOQT}} for relays also apply.

# IANA Considerations

## MOQT Setup Options

This document requests a registration in the "MOQT Setup Options" registry
({{Section 16.4 of MOQT}}), whose policy is Specification Required.  Until
a permanent value is assigned, implementations use the provisional value
below.

| Type | Name | Specification |
|:-----|:-----|:--------------|
| TBD1 (provisional 0x40C51) | HOP_LIMIT_SUPPORTED | {{negotiation}} |

## MOQT Message Parameters

This document requests a registration in the "MOQT Message Parameters"
registry ({{Section 16.7 of MOQT}}).

| Parameter Type | Parameter Name | Specification |
|:---------------|:---------------|:--------------|
| TBD2 (provisional 0x40C52) | HOP_LIMIT | {{parameter}} |

HOP_LIMIT may appear in SUBSCRIBE, FETCH, TRACK_STATUS,
SUBSCRIBE_NAMESPACE and SUBSCRIBE_TRACKS, at most once per message.

## MOQT REQUEST_ERROR Codes

This document requests a registration in the "REQUEST_ERROR Codes" registry
({{Section 16.11.2 of MOQT}}).

| Name | Code | Specification |
|:-----|:-----|:--------------|
| HOP_LIMIT_EXCEEDED | TBD3 (suggested 0x37) | {{errors}} |

The provisional values were chosen not to collide with the provisional
registrations of {{CLUSTER}} or with the reserved greasing values of
{{Section 13 of MOQT}}.

# Implementation Status

Note to the RFC Editor: please remove this section before publication.

This section records the status of known implementations at the time of
writing, as described in {{RFC7942}}.

MOQOM relay (Elliott Wave LLC), Rust:
: Implements this document for SUBSCRIBE on draft-ietf-moq-transport-22
  sessions, using the provisional values 0x40C51, 0x40C52 and 0x37.  It
  sends HOP_LIMIT_SUPPORTED in its own SETUP and uses HOP_LIMIT only on a
  session where the peer sent it too.  It closes the session with
  PROTOCOL_VIOLATION if HOP_LIMIT_SUPPORTED carries a value, or if HOP_LIMIT
  appears on a session that did not negotiate it.  MAX_HOP_LIMIT is
  configurable and defaults to 3.  An absent HOP_LIMIT means MAX_HOP_LIMIT,
  and a larger value is clamped to it.  The relay refuses to forward at zero
  with HOP_LIMIT_EXCEEDED and a Retry Interval of 0, and it logs and counts
  the refusal.  Each forward carries one less than the remaining hop limit.
  A HOP_LIMIT_EXCEEDED from upstream is passed to every downstream
  subscriber waiting on that request.  The relay applies the aggregation
  rule of {{aggregation}} to pending upstream subscriptions.  It also
  carries the remaining hop limit in a vendor parameter, 0x4D50, which it
  uses on draft-ietf-moq-transport-19 sessions and towards peers that did
  not negotiate this extension but did enable MOQOM's vendor parameters.  When
  a request carries both parameters, the smaller value is used.  On
  draft-ietf-moq-transport-19 sessions an exhausted limit is answered with
  DOES_NOT_EXIST, because that version has no HOP_LIMIT_EXCEEDED code.  The
  relay forwards only SUBSCRIBE between relays: it serves FETCH from its own
  cache and does not forward TRACK_STATUS, SUBSCRIBE_NAMESPACE or
  SUBSCRIBE_TRACKS.  It does not follow REDIRECT; it passes the response to
  the downstream subscriber.  It caches no HOP_LIMIT_EXCEEDED response.
  Integration tests run chains of three relays and a cycle of two.  They
  show that a request is served with enough hop limit remaining, and that
  HOP_LIMIT_EXCEEDED is returned at the relay where the limit reaches zero
  and propagated to the subscriber.  They also show that the session stays
  open after the error, and that a cycle ends in HOP_LIMIT_EXCEEDED.

MOQOM client (Elliott Wave LLC), Swift:
: Does not offer HOP_LIMIT_SUPPORTED, so relays never send it HOP_LIMIT.
  It accepts the vendor parameter 0x4D50 as an original publisher and never
  sends it.

--- back

# Example

Relays A and B are each configured to forward the namespace "live" to the
other, and both use MAX_HOP_LIMIT 3.  No publisher of "live" is connected.
A subscriber sends SUBSCRIBE for ("live", "cam") to A without HOP_LIMIT.

~~~
Subscriber        Relay A              Relay B
    |                |                    |
    |-- SUBSCRIBE -->| remaining = 3      |
    |                |-- SUBSCRIBE ------>| remaining = 2
    |                |   HOP_LIMIT = 2    |
    |                |<-- SUBSCRIBE ------|
    |                |   HOP_LIMIT = 1    |
    | remaining = 1 < 3 (pending), so A forwards; it does not attach
    |                |-- SUBSCRIBE ------>| remaining = 0
    |                |   HOP_LIMIT = 0    |
    |                |<- REQUEST_ERROR ---| HOP_LIMIT_EXCEEDED
    |                |-- REQUEST_ERROR -->| (completes B's pending
    |                |                    |  request)
    |                |<- REQUEST_ERROR ---|
    |<-REQUEST_ERROR-|                    |
    |  HOP_LIMIT_EXCEEDED                 |
~~~
{: title="A two-relay cycle ending after three forwards"}

Without this extension, A and B forward the request to each other
indefinitely, or, if they aggregate, wait on each other until a timeout.

# Acknowledgments
{:numbered="false"}

The approach follows the discussion of loop prevention between relays in
the MoQ working group, including the work on {{CLUSTER}}.
