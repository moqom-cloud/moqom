# Show HN draft

Status: draft for Elliott. Nothing here has been posted. Every number below is
from `docs/BENCHMARKS-AND-SPECS.md`; re-check it against that file the day you
post. Outcomes only: this copy explains what MOQOM does and why Media over QUIC,
never how MOQOM's own parts work (patents pending).

## Title

```
Show HN: MOQOM – Real-time video on IETF Media over QUIC (MoQ)
```

62 characters. Alternative, 74:
`Show HN: MOQOM – Calls and broadcast on one protocol, IETF Media over QUIC`

The URL field: `https://moqom.cloud` (or the `moqom-swift` repository once it is
public, if you would rather people land on code).

## First comment

Post this as the first comment straight after submitting.

> Hi HN, I'm Elliott. MOQOM is a real-time video platform built on Media over
> QUIC Transport, the IETF draft for live media (we speak drafts 22 and 19 of
> draft-ietf-moq-transport). I built it on my own, under my company Elliott
> Wave LLC. The work was done about a month ago; my day job is Member of
> Technical Staff at Favorited, and our native app launch came first, so the
> release waited until now.
>
> **What it is.** An SDK for iOS and macOS, a hosted relay network (MOQOM
> Cloud), backend SDKs in Go and Rust to mint join tokens and manage and
> moderate rooms, and an MCP server so an AI agent can operate a tenant. You
> can also run the relays yourself.
>
> **Why MoQ.** Today a product that does both calls and broadcast usually runs
> two stacks: WebRTC for the call, HLS or LL-HLS for the audience, and a
> transcoding hop between them. MoQ is publish/subscribe over QUIC: a publisher
> sends named tracks, relays fan them out, and a subscriber asks for what it
> wants. The same track that carries a two-person call can reach thousands of
> viewers through relays, with no second protocol. QUIC gives independent
> streams, so one lost packet doesn't stall everything behind it, and TLS 1.3
> on every connection. It is also an open standard in progress, not a
> vendor's protocol.
>
> **Numbers, with the caveat first.** These are lab measurements, with the
> rigs on the page. Full data and how to rerun the load tests:
> https://moqom.cloud/benchmarks/load-tests.md
>
> - 720p30 at 1.25 Mbit/s HEVC, two iPhones on home Wi-Fi, relay in Los Angeles, both directions
>   at once, 90 s: 56–57 ms median glass-to-glass (camera light to screen),
>   p95 70 ms, every frame delivered.
> - 540p60 at 600 kbps on a live co-hosted call (each phone publishing while
>   decoding the other host), same relay: 69 ms median.
> - Fan-out: 6,000 viewers on one 4-vCPU relay (GCP n2-standard-4), every
>   frame delivered, 61 ms median delivery, and the relay's own counters
>   agree. Past that the relay's CPU is the limit; more viewers means more
>   relays.
> - The same 6,000-viewer test through Cloudflare's MoQ beta relay (draft-16)
>   is on the page too: faster median, but 241 viewers never received video
>   and there are no relay counters to check delivery against.
>
> Why it holds at audience scale: WebRTC was built for calls, so big
> audiences usually get moved to a slower mode. MoQ is publish/subscribe over
> QUIC: relays forward the same media to every subscriber and chain like a CDN.
>
> The relay is Rust and the control plane is Go, and both scale out
> horizontally. 1080p and 4K glass-to-glass are not measured yet; they're next.
>
> **Open source vs. not.** This is open core, and I'd rather say so up front.
> Apache-2.0: the Swift package (a thin public shell plus the `MoqomRoom`
> sample app), the Go and Rust backend SDKs, and the MCP server. Closed: the
> client media core, which ships as signed binary frameworks, plus the relay
> and the control plane. You can read and build everything you integrate
> with; you can't rebuild the media engine.
>
> **Pricing.** Prepaid credit, metered on delivery. Hosted usage is 85% of
> Agora's price per 1,000 participant-minutes at every tier, after Agora's own
> volume discounts, and always at least one cent under. Relays you run
> yourself pay only a platform fee, $0.05 per GiB of egress, never more than
> the hosted rate. The live rate
> card is public JSON at `GET /v1/rates`. Launch pricing; it may change before
> general availability.
>
> **Limitations, honestly.**
>
> - iOS and macOS only today. Web, Android, desktop and an OBS plugin are
>   in progress.
> - The numbers above are lab numbers: tested to 6,000 viewers on one relay,
>   not yet at million-viewer scale.
> - No independent security audit yet.
> - MoQ is still an Internet-Draft. The wire format will change as the draft
>   does, and I'll track it.
> - It's one person. Support is me.
>
> **Try it.** <!-- TODO(Elliott): confirm every link below resolves publicly before posting. -->
> Sign up at https://moqom.cloud/signup, then follow the quickstart at
> https://moqom.dev/getting-started/quickstart/. The Swift package is
> `https://github.com/moqom-cloud/moqom-swift` and the sample app is in
> `Samples/MoqomRoom`. Backend SDKs: `github.com/moqom-cloud/moqom-go`,
> `github.com/moqom-cloud/moqom-rust`.
>
> **What I'd like feedback on.** Whether the open-core split is one you could
> live with; what you'd need from the web SDK first; and if you've run WebRTC
> or LL-HLS in production, where you'd expect this to fall over. Happy to go
> into MoQ itself, the measurement method, or pricing.

Word count is about 580. HN readers do read long first comments if they are
specific; cut the "Why MoQ" paragraph first if it needs to be shorter.

## Before posting

- [ ] Every repository linked above is public, and its README matches this comment. The repositories are under `github.com/moqom-cloud` (per `go.mod` and the git remotes); some copy says `github.com/elliott-wave-labs`. Pick one before posting.
- [ ] Signup works end to end from a browser at moqom.cloud, including Stripe Checkout. The billing API is not on a browser-trusted host yet (see `sites/_shared/stats/README.md`).
- [ ] `GET /v1/rates` answers on the public URL the comment implies.
- [ ] The fan-out and Cloudflare numbers above still match https://moqom.cloud/benchmarks.
- [ ] Someone other than you has followed the quickstart from scratch.

## Posting tips

From the HN guidelines and Show HN rules, plus general practice:

- **Never ask for votes or comments.** Not friends, not colleagues, not
  Slack, not social media posts that say "upvote". HN detects voting rings
  and penalises or kills the post. Sharing the link is fine; asking for
  upvotes is not.
- **Show HN has to be something people can try.** Signup must work and the
  quickstart must hold before you post. A sign-up wall with nothing behind it
  gets flagged.
- **Plain title.** No superlatives, no "fastest", no exclamation marks. The
  mods will rewrite a baity title, and the crowd will hold it against you.
- **Timing.** A common heuristic, not a rule: a weekday morning, US Pacific
  (roughly 7–10 am PT, Tuesday to Thursday). Avoid the same day as the
  Product Hunt launch so you can be present for both.
- **Stay for the first few hours.** Answer every substantive comment
  yourself, plainly. Agree with fair criticism. Don't argue about tone.
- **Don't** edit the title after posting, delete and repost, or post from a
  second account.
- **If it sinks without comments**, one repost after a few days is accepted;
  or email hn@ycombinator.com and ask about the second-chance pool.
- **Disclose.** You are the founder; the first line of the comment says so.
