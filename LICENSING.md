# Licensing

MOQOM is split in two. The split follows what each part is *for*: the pieces
developers integrate against are permissive open source, and the service that
constitutes the product is proprietary and ships only as binaries.

| Component | License |
|---|---|
| Protocol, client core, SDKs, schemas, samples, agent protocol | Apache-2.0 |
| Relay, control plane, director, ingest, recorder, `moqom-cc` | Proprietary ([`LicenseRef-MOQOM-Proprietary`](LICENSES/LicenseRef-MOQOM-Proprietary.txt)), binary only |
| Host plugins that link a GPL host (OBS) | GPL, in their own repos |

Full texts are in [`LICENSES/`](LICENSES/). The split is by role rather than by
repository, so knowing where a file lives does not tell you which half it is in:
[`moqom-engine`](https://github.com/moqom-cloud/moqom-engine) holds the
Apache-2.0 protocol and transport beside the proprietary relay, and this repository
holds the Apache-2.0 contract in `proto/` beside the proprietary control plane in
`go/`. Every crate declares its own `license` field; **a crate without one is a
bug**, since there is no safe default to fall back on. The engine's own
[LICENSING.md](https://github.com/moqom-cloud/moqom-engine/blob/main/LICENSING.md)
maps its crates and explains why both halves share a workspace.

"SDKs" includes the server-side ones. `moqom-go` and `moqom-rust` are Apache-2.0
even though the control plane they call is proprietary, and the line between them
is the gRPC contract in `proto/`: an SDK holds generated stubs and the wire
vocabulary, never any part of the server. That is what keeps a customer's
backend free of a proprietary dependency for doing something as ordinary as
kicking somebody out of a room — and it is why neither SDK may grow a shortcut
that imports from `go/internal` or links the relay.

## Apache-2.0, and why not MIT

The SDK half has to be permissive — anything with copyleft obligations makes a
mobile SDK unadoptable. Between the permissive options, Apache-2.0 was chosen
over MIT specifically for its patent provisions, which MIT lacks entirely:

- **Section 3 grants a patent license** from every contributor covering their
  contribution. Without it, someone can contribute code, watch it ship, then
  assert a patent reading on it against our users. Copyright permission and
  patent permission are separate things, and MIT grants only the first.
- **Section 3 also terminates** a party's patent license if they sue anyone
  claiming the work infringes their patent. A forker who patents an improvement
  and comes after us loses their right to the code they depend on.

Neither provision stops anyone from *filing* a patent — no copyright license
can. What blocks that is prior art, which we get from publishing at all.

Publishing is not free once we hold patent applications of our own, which
Elliott Wave LLC now does. Two consequences, both in
[`docs/PATENT-ASSESSMENT.md`](docs/PATENT-ASSESSMENT.md):

- **Section 3 licenses our own patents too.** Every claim necessarily infringed
  by Apache-2.0 code we publish is licensed royalty-free to everyone who uses
  that code. A patent reading on an SDK is defensive; one reading on the relay
  or control plane is not.
- **Publication before filing forfeits most of the world.** The US allows a
  one-year grace period after our own disclosure; Europe, China and most
  others do not. Nothing describing an invention goes public — code, design
  document, or IETF draft — until an application covering it is on file.

## Proprietary on the service

The relay, control plane, director, ingest, recorder and `moqom-cc` are
proprietary, under [`LicenseRef-MOQOM-Proprietary`](LICENSES/LicenseRef-MOQOM-Proprietary.txt).
Their source is a trade secret: it is not published, not distributed to
customers, and never converts to an open licence. MOQOM Cloud runs them for
customers; self-hosting (BYOC) customers receive signed binaries under a written
licence that permits self-hosting for their own applications and end users and
prohibits offering the service to third parties.

This replaces BUSL-1.1, which was chosen so BYOC customers could audit the
relay's source. Two things changed. Every BUSL release would have become
Apache-2.0 four years later, publishing the design we most want to keep. And
auditability no longer needs the relay's source: media is end-to-end encrypted
before it reaches a relay, and the cryptography that protects it is open source
in [`moqom-security`](https://github.com/moqom-cloud/moqom-security), so a
customer can verify what protects their media without seeing how the service
works.

## `moqom-cc` is the awkward case

Congestion control runs on the sender, so `moqom-cc` ships inside the client
SDK — the Apache-2.0 half — while being the component whose tuning is most
worth protecting and least reverse-engineerable from observed traffic.

**Resolution: `moqom-cc` is proprietary and distributed to SDK consumers as a
prebuilt binary**, not source. The Apache-licensed client links it. This
preserves it as a trade secret, which a patent could not do, since patents
publish.

Two consequences to design around rather than discover later:

1. The crate must stay behind a clean interface — `quinn`'s `Controller` trait —
   so an Apache-2.0-only build can substitute a stock controller and still
   function. An SDK that cannot build without a proprietary binary is not
   meaningfully open.
2. Prebuilt binaries are needed for every client target: iOS, iOS simulator,
   macOS, Android on four ABIs, WASM, and desktop. That is a real release-
   engineering burden and it starts the day the crate exists.

## The GPL boundary: plugins for hosts we do not own

An OBS plugin links `libobs`. The OBS Project's position is that this makes the
plugin a combined work under the GPL, so distributing one obliges us to publish
its source under a GPL-compatible license. `libobs` is GPL-2.0-**or-later** with
no linking exception.

That has three consequences, and the first two are hard constraints rather than
preferences:

1. **The plugin lives in its own public repository.** GPL source availability is
   an obligation to everyone who receives a binary, which a private monorepo
   cannot satisfy — and this repository's other half is proprietary, which is
   not a free license and must never end up inside a GPL work.
2. **Only Apache-2.0 MOQOM code may cross the boundary.** The plugin links the
   C ABI of `moqom-client`, which is Apache-2.0. It must *not* link `moqom-cc`,
   whose whole point is being a proprietary prebuilt binary. This is exactly the case
   that consequence 1 above was written for: the plugin is the Apache-2.0-only
   build with a stock controller substituted behind `quinn`'s `Controller`
   trait. The substitutability requirement stops being hypothetical here.
3. **The distributed binary is likely GPLv3, not GPLv2.** Apache-2.0 is
   compatible with GPLv3 and is *not* compatible with GPLv2, because of its
   patent-termination and indemnity terms. Since `libobs` is v2-or-later it can
   be taken as v3, so the combination resolves — but at v3. Counsel should
   confirm this before first release rather than after.

The strategic cost is worth naming plainly, because it is easy to discover late:
OBS is the widest distribution channel in the plan, and under these rules it
ships without the controller that is supposed to differentiate us. The ways out
are to relicense or dual-license `moqom-cc`, or to accept that OBS gets stock
congestion control. Deciding that is not a licensing question.

None of the above applies to a host with a permissive license. The rule is not
"plugins are GPL" — it is that a plugin inherits the license of whatever it
links, and we do not choose that.

## Contributor License Agreement

A CLA, not a DCO. Both relicensing and acquisition require the ability to change
terms, and retrofitting a CLA means tracking down every past contributor — a
cost that grows with every merged pull request.

Elliott Wave LLC owns all intellectual property in MOQOM, including the
open-source components; the open-source licences grant use, not ownership. To
keep ownership in one place, [`.github/CLA.md`](.github/CLA.md) is a copyright
assignment: contributors assign the copyright in their contributions to
Elliott Wave LLC and receive a perpetual licence back to use their own work for
any purpose. Section 5 states plainly that contributions may be relicensed,
sold and offered commercially — burying that would be a bad way to treat
contributors. We have contacted legal counsel to review the agreement.

Enforcement runs through `contributor-assistant/github-action` on every pull
request.

## Open items

Blocking public distribution:

- [x] **Legal entity.** Elliott Wave LLC is the licensor in both
      `LICENSES/LicenseRef-MOQOM-Proprietary.txt` files and the Maintainer in
      `.github/CLA.md`.
- [ ] **IP assignment to the LLC.** Copyright and inventions vest in the person
      who wrote them unless assigned in writing. A signed assignment from
      Elliott to Elliott Wave LLC, covering work since the first commit, is
      what makes the copyright lines and the patent applications the LLC's.
- [ ] **Counsel review** (counsel contacted) of the proprietary licence text,
      the BYOC licence agreement and the copyright assignment CLA, including
      whether a PR-comment signature satisfies 17 U.S.C. § 204(a) for a
      transfer of copyright.
- [ ] **`legal@moqom.cloud`** must exist; the proprietary licence names it as
      the contact for licensing.
- [ ] **`CLA_SIGNATURES_TOKEN`** repository secret — a PAT with `repo` scope,
      required before the CLA workflow can record signatures.

Before the first external release:

- [ ] Per-crate `LICENSE` files and SPDX headers across both halves.
- [ ] Decide whether Apache-half crates publish to crates.io, and reserve the
      `moqom-*` names if so.

## Unrelated, but frequently confused with this

Codec patent exposure is a separate problem. HEVC pool licensing on iOS capture
is unaffected by anything above, and AV1 being royalty-free does not avoid it,
because Apple silicon has no AV1 hardware encoder.
