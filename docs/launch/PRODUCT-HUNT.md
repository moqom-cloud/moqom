# Product Hunt draft

Status: draft for Elliott. Nothing has been scheduled. Same rules as
`SHOW-HN.md`: numbers from `docs/BENCHMARKS-AND-SPECS.md` with the lab caveat
beside them, outcomes only, no customers, partners or quotes that don't exist.

## Listing

**Name:** MOQOM

**Tagline** (54 of 60 characters):

```
Real-time video on Media over QUIC, calls to broadcast
```

**Description** (254 of 260 characters):

```
Real-time video on IETF Media over QUIC: one protocol from two-person calls to audiences of thousands. 56–57 ms median glass-to-glass at 720p30 and 6,000 viewers on one 4-vCPU relay (lab). Post-quantum end-to-end encryption. iOS, macOS, Go and Rust SDKs.
```

**Topics** (pick up to three that exist on Product Hunt at the time): Developer
Tools, Video Streaming, Open Source. Alternates: API, iOS, Mac.

**Links:** website `https://moqom.cloud`; docs `https://moqom.dev`; GitHub
`https://github.com/moqom-cloud/moqom-swift` (only once public; see the org
note in `SHOW-HN.md`).

**Pricing field:** "Paid (prepaid credit)". Not "Free": there is no free tier.
This matches the moqom.cloud pricing FAQ ("Is there a free tier? No.").
<!-- TODO(Elliott): change both if a signup credit (Config.SignupCredit in billing, not wired to an env var yet) is turned on for launch. -->

**Makers:** Elliott. Nobody else is listed, because nobody else built it.

## Maker's first comment

> Hi Product Hunt, I'm Elliott, founder of elliottwave.studio lab.
>
> MOQOM is real-time video built on Media over QUIC, the IETF's new standard
> for live media. The point is one protocol for everything: the stream that
> carries a two-person call can also reach an audience of thousands, without
> bolting a broadcast product onto a calling product.
>
> What you get today:
>
> - A Swift SDK for iOS and macOS, with a sample room app.
> - MOQOM Cloud, hosted relays, or run your own relays and pay a platform fee only ($0.05 per GiB).
> - Go and Rust backend SDKs (Apache-2.0) for tokens, rooms and moderation, and an MCP server so an AI agent can run your tenant.
> - Prepaid, 15% under Agora's price at every tier and volume, always at least one cent under. The rate card is public.
>
> In lab tests, 720p30 runs at 56–57 ms median glass-to-glass (p95 70 ms),
> and one 4-vCPU relay served 6,000 viewers with every frame delivered.\*
> End-to-end encryption uses post-quantum MLS room keys.
>
> What it doesn't do yet: web, Android, desktop and OBS are in progress.
> There's no independent security audit yet. More viewers than one relay can
> carry means more relays; the full test data is at
> moqom.cloud/benchmarks/load-tests.md.
>
> I'd love to hear what you'd build with it, and which platform you need next.
>
> \* Lab measurements: 6,000 viewers on one 4-vCPU Google Cloud relay, every frame delivered, 61 ms median delivery, confirmed by the relay's own counters. Rigs and rerun commands at moqom.cloud/benchmarks.

## Gallery shot list

Product Hunt gallery images are 1270×760 (PNG or JPG); the thumbnail is
240×240. Capture real screens only: no mock-ups of features that don't
ship, no simulated numbers.

1. **Hero card.** MOQOM wordmark, the tagline, and the three headline numbers
   (56–57 ms median at 720p30, 69 ms at 540p60, 6,000 viewers on one 4-vCPU
   relay) with the footnote in full at the bottom. Dark background (#0A0B0D),
   cyan accent (#22D3EE).
2. **Demo video, 60–90 s.** Two iPhones side by side in a live call through the
   `MoqomRoom` sample, then a third device joining as a viewer. Unedited
   between cuts, real network; say on screen where the relay is.
3. **Glass-to-glass, filmed.** If you can: one camera filming both phones and a
   millisecond clock, so viewers see the delay themselves. Label it as a single
   run, not a benchmark.
4. **macOS.** The same room on a Mac.
5. **Code.** About ten lines of Swift to join a room, beside the Go call that
   mints the join token. Real code from the quickstart, syntax-highlighted.
6. **Pricing.** The public `/v1/rates` response rendered as a card: Agora's list price,
   the 15% cut and the volume bands per tier.
7. **Fan-out chart.** The 4,000–6,000 viewer latency chart from
   moqom.cloud/benchmarks, every frame delivered, with the footnote.
8. **Platforms.** iOS and macOS available; web, Android, desktop, OBS "In
   progress".
9. **MCP (optional).** An AI assistant listing rooms and muting a participant
   through the MCP server.

Thumbnail: the MOQOM mark on #0A0B0D. <!-- TODO(Elliott): SVG logo does not exist yet; see sites/elliottwave-studio/src/data/press.ts. -->

## Launch-day checklist

Before the day:

- [ ] Every item under "Before posting" in `SHOW-HN.md` is done (public repos, working browser signup, rates on the public URL, quickstart tested by someone else).
- [ ] Choose the GitHub org (moqom-cloud vs elliott-wave-labs) and make every link agree.
- [ ] Create the `press@elliottwave.studio` mailbox; check that `hello@` and `security@moqom.cloud` deliver.
- [ ] Gallery images, video and thumbnail exported at the sizes above.
- [ ] Schedule the launch in Product Hunt for 12:01 am Pacific (the day resets then). Pick a different day from Show HN.
- [ ] Make sure an outage reaches you: `notification_emails` is empty in `deploy/envs/dev/terraform.tfvars`, so alerts currently go nowhere.

On the day:

- [ ] Post the maker comment the minute the launch goes live.
- [ ] Answer every comment yourself through the day. Plain answers, same caveats.
- [ ] Share the link where you normally post, as news ("MOQOM is live on Product Hunt"). Don't ask anyone for upvotes: Product Hunt's rules forbid it and it penalises launches that do. No new or borrowed accounts.
- [ ] Put the launch URL in `LAUNCH.productHunt` in `sites/elliottwave-studio/src/data/press.ts` and rebuild, so /press stops saying "Coming soon".
- [ ] Watch signups and errors (Stripe dashboard, control plane logs). The public signup counter shows the real number, whatever it is.

After:

- [ ] Any new load-test results go on moqom.cloud/benchmarks and load-tests.md first, then as a comment on both launches, good or bad.
