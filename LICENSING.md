# Licensing

MOQOM is dual-licensed. The split follows what each part is *for*: the pieces
developers integrate against are permissive, and the server that constitutes the
product is source-available.

| Component | License |
|---|---|
| Protocol, client core, SDKs, schemas, samples, agent protocol | Apache-2.0 |
| Relay, control plane, director, ingest, `moqom-cc` | BUSL-1.1 → Apache-2.0 |

Full texts are in [`LICENSES/`](LICENSES/). Every crate declares its own
`license` field; **a crate without one is a bug**, since the workspace spans
both halves and there is no safe default to fall back on.

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
Publishing design documents alongside the code extends the same protection to
ideas we have not yet shipped, and costs nothing.

## BUSL-1.1 on the relay

Source-available rather than closed, because asking security-conscious BYOC
customers to run an opaque binary while claiming auditability is a
contradiction. The [Additional Use Grant](LICENSES/BUSL-1.1.txt) permits
self-hosting for your own applications and end users, including commercially,
and prohibits only offering it to third parties as a competing hosted service.
Each version converts to Apache-2.0 four years after release.

BUSL is not an OSI-approved open source license, and we should not describe it
as one.

## `moqom-cc` is the awkward case

Congestion control runs on the sender, so `moqom-cc` ships inside the client
SDK — the Apache-2.0 half — while being the component whose tuning is most
worth protecting and least reverse-engineerable from observed traffic.

**Resolution: `moqom-cc` is BUSL-licensed and distributed to SDK consumers as a
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

## Contributor License Agreement

A CLA, not a DCO. Both relicensing and acquisition require the ability to change
terms, and retrofitting a CLA means tracking down every past contributor — a
cost that grows with every merged pull request.

[`.github/CLA.md`](.github/CLA.md) is adapted from the Apache ICLA v2.2, with
one substantive addition: Section 4 states plainly that contributions may be
relicensed and offered commercially. Foundation CLAs do not need that clause;
commercial ones do, and burying it would be a bad way to treat contributors.

Enforcement runs through `contributor-assistant/github-action` on every pull
request.

## Open items

Blocking public distribution:

- [ ] **Legal entity.** `LICENSES/BUSL-1.1.txt` and `.github/CLA.md` both carry
      a `[LEGAL ENTITY]` placeholder. A BSL naming a nonexistent licensor is
      defective, so this blocks any public release.
- [ ] **Counsel review** of the Additional Use Grant and the CLA. The Additional
      Use Grant is the single most commercially consequential sentence in the
      repository — it defines what a competitor may do.
- [ ] **`legal@moqom.com`** must exist; the BSL names it as the contact for
      alternative licensing.
- [ ] **`CLA_SIGNATURES_TOKEN`** repository secret — a PAT with `repo` scope,
      required before the CLA workflow can record signatures.

Before the first external release:

- [ ] Per-crate `LICENSE` files and SPDX headers across both halves.
- [ ] Decide whether Apache-half crates publish to crates.io, and reserve the
      `moqom-*` names if so.
- [ ] Trademark: "Business Source License" is MariaDB's mark and its use is
      conditioned on the covenants in the license text. Do not modify that text.

## Unrelated, but frequently confused with this

Codec patent exposure is a separate problem. HEVC pool licensing on iOS capture
is unaffected by anything above, and AV1 being royalty-free does not avoid it,
because Apple silicon has no AV1 hardware encoder.
