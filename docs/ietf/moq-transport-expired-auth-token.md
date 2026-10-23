# Issue and pull request: EXPIRED_AUTH_TOKEN as a PUBLISH_DONE code

**Status:** filed 7 October 2026 as
[issue #1969](https://github.com/moq-wg/moq-transport/issues/1969) and
[pull request #1970](https://github.com/moq-wg/moq-transport/pull/1970).

For [moq-wg/moq-transport](https://github.com/moq-wg/moq-transport). This is a small change to the
core draft, so it goes in as an issue plus a pull request, not as a separate Internet-Draft. The
patch is `moq-transport-expired-auth-token.patch`, made against `main` at `98b0a3cf563d`
(1 October 2026, the draft-22 text). Searches of the repository on 6 October 2026 found no
existing issue or pull request for this.

To file it:

1. Open the issue below.
2. Fork the repository and apply the patch: `git apply moq-transport-expired-auth-token.patch`.
3. Run `make` to check that the draft builds, then open a pull request that says "Fixes #<issue>".

The contributor's own account needs to file it. Anything submitted to the IETF is a Contribution
under BCP 78 and BCP 79 (see [the IPR note](#ipr)).

---

## Issue

**Title:** No PUBLISH_DONE code for an expired authorization token

`EXPIRED_AUTH_TOKEN` is defined as a Session Termination code (0x18), a REQUEST_ERROR code (0x5)
and a Stream Reset code (0x7), but not as a PUBLISH_DONE code.

When the token that authorized an established subscription expires, the publisher can only end
the subscription with `UNAUTHORIZED` (0x1) or `INTERNAL_ERROR` (0x0). A subscriber can't tell
"your token expired, refresh it and subscribe again" from "you're not allowed to subscribe to
this track". The first is routine for long-lived subscriptions such as live events. The second
should not be retried.

PUBLISH_NAMESPACE already has guidance for this case: a publisher can send it again "with
refreshed authorization". Subscriptions have no equivalent.

**Proposal:**

- Add `EXPIRED_AUTH_TOKEN` (0x7) to the PUBLISH_DONE Codes registry. 0x7 is unused there, and it
  matches the Stream Reset code that has the same meaning.
- In the PUBLISH_DONE section, say a publisher SHOULD use it when it ends a subscription because
  the authorizing token expired, and the subscriber can subscribe again with a refreshed token,
  or avoid the interruption by sending a REQUEST_UPDATE with a new AUTHORIZATION_TOKEN before
  expiry.
- Say the code refers to the subscriber's token, so a relay whose own upstream credentials lapse
  doesn't pass it on to downstream subscribers, who can't fix that.

This doesn't change behaviour: a receiver that doesn't know the code treats it as
`INTERNAL_ERROR` (§13).

---

## Pull request

**Title:** Add EXPIRED_AUTH_TOKEN to PUBLISH_DONE codes

**Body:**

> Fixes #<issue>.
>
> Adds `EXPIRED_AUTH_TOKEN` (0x7) to the PUBLISH_DONE Codes registry, matching its Stream Reset
> code, and two sentences in PUBLISH_DONE on when to use it and how a subscriber recovers.

**Diff** (`moq-transport-expired-auth-token.patch`):

```diff
@@ PUBLISH_DONE @@
 subscriber SHOULD send STOP_SENDING on all streams related to the subscription
 when it deletes subscription state.

+A publisher that ends a subscription because the authorization token that
+authorized it has expired SHOULD send PUBLISH_DONE with status code
+`EXPIRED_AUTH_TOKEN`. A subscriber that receives it can subscribe again with
+a refreshed token. To avoid the interruption, a subscriber can instead send a
+REQUEST_UPDATE carrying a new AUTHORIZATION_TOKEN before the token expires.
+`EXPIRED_AUTH_TOKEN` refers to the token presented by the subscriber; a relay
+whose own upstream authorization expires and cannot be refreshed uses a
+different status code.
+
@@ Publish Done Codes @@
 EXPIRED (0x6):
 : The publisher reached the timeout specified in SUBSCRIBE_OK.

+EXPIRED_AUTH_TOKEN (0x7):
+: The authorization token that authorized the subscription has expired
+  (see {{message-publish-done}}).
+
@@ IANA: PUBLISH_DONE Codes @@
 | EXPIRED            | 0x6  | {{publish-done-codes}} |
+| EXPIRED_AUTH_TOKEN | 0x7  | {{publish-done-codes}} |
```

---

## IPR {#ipr}

Under BCP 79, a contributor who knows of a patent or patent application that they or their
employer own, and that covers a Contribution, must file an IPR disclosure with the IETF. A search
of Elliott Wave's pending applications found none that cover this change or the hop-limit draft.
Counsel should confirm this before either is submitted.
