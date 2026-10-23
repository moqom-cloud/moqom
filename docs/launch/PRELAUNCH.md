# Prelaunch checklist

Everything between today and posting on Show HN and Product Hunt, in order. Items marked
**(you)** need Elliott's accounts or a decision; the rest can be done by anyone with access.

Already done (22 Oct 2026): pricing at 85% of Agora's price at Agora's volume bands, at least
1¢ under, is in the billing code (`moqom-pq`, branch `oct-2026-pq-signatures`) and on
moqom.cloud and moqom.dev; the sites are in git (`moqom-cloud/moqom-sites`, private); the API
edge has its address (34.127.119.38); the Oregon relay is retired to stay inside the address
quota.

## 1. DNS (you)

- [ ] At Hover, add an A record for `moqom.cloud`: host `api`, value `34.127.119.38`.
- [ ] Wait for it to resolve: `dig +short api.moqom.cloud A` returns `34.127.119.38`.
- [ ] Check the API answers over post-quantum TLS. macOS's built-in curl can't negotiate
      ML-KEM, so use Homebrew's:
      `$(brew --prefix curl)/bin/curl -sS https://api.moqom.cloud/v1/rates | head`
      (or `moqom-cli` from `moqom-engine-pq`).

## 2. Ship the new pricing to the control plane

The live control plane still runs the cost-plus image until this is done. The sites already
quote the new prices, so do this before launch.

- [ ] Merge `oct-2026-pq-signatures` into `main` in `moqom-cloud/moqom` (it already contains
      `main`), and check out `main` in `~/Documents/GitHub/moqom-cloud/moqom`.
- [ ] Check it builds against the post-quantum Go SDK. The `go.mod` replace to `../../moqom-go`
      is missing `adminpb.DirectGrant`; either point it at `moqom-go-pq` or merge that SDK's
      branch first. Locally:
      `GOWORK=/tmp/moqom-pq.work go test ./internal/billing/... ./cmd/moqom-control/...`
- [ ] Build the image (no local Docker needed):
      ```sh
      cd ~/Documents/GitHub/moqom-cloud
      gcloud builds submit . --project=moqom-503700 \
        --config=moqom/deploy/tools/cloudbuild-control.yaml \
        --substitutions=_TAG=dev,_VERSION=$(git -C moqom rev-parse --short HEAD)
      ```
- [ ] Roll it: `kubectl -n moqom rollout restart deploy/moqom-dev-control` and
      `kubectl -n moqom rollout status deploy/moqom-dev-control`. The two new tables
      (`billing_hosted_months`, `billing_hosted_claims`) are created on startup from
      `schema.sql`.
- [ ] Confirm: `GET /v1/rates` shows `"share_basis_points": 8500` and four `volume_bands`, and
      the host HD rate in the first band is `$3.390000`.
- [ ] Remove the old `MOQOM_BILLING_MARKUP_BP` / `MOQOM_BILLING_FEE_PER_GIB` settings from the
      deployment if present; `MOQOM_BILLING_SHARE_BP` (default 8500) replaces them.

## 3. Stripe (you)

- [ ] In the Stripe dashboard (live mode), finish account activation: business details, bank
      account, statement descriptor `MOQOM`.
- [ ] Create a restricted key (`rk_live_…`) with write access to Checkout Sessions, Payment
      Intents, Customers and Refunds, or use the secret key.
- [ ] Add a webhook endpoint `https://api.moqom.cloud/v1/stripe/webhook` for
      `payment_intent.succeeded`, `payment_intent.payment_failed`, `refund.created` and
      `refund.updated`. Copy its signing secret (`whsec_…`).
- [ ] Store both, pasting at the prompt so they don't land in shell history:
      ```sh
      gcloud secrets versions add moqom-dev-stripe-secret-key --data-file=- --project=moqom-503700
      gcloud secrets versions add moqom-dev-stripe-webhook-secret --data-file=- --project=moqom-503700
      ```
- [ ] Run `deploy/tools/sync-secrets.sh`; it should print "including the Stripe keys". Then
      restart control as in step 2.
- [ ] Make a real $10 top-up from a fresh account on moqom.cloud, confirm the balance appears,
      then refund it from the dashboard and confirm the balance drops.

## 4. Infrastructure

- [ ] Reconcile Terraform in `moqom-pq/deploy/envs/dev` before anyone runs `apply`: a plan
      today would destroy `api_edge` and recreate the Oregon forwarding rule. Encode both
      (Oregon at 0 with no forwarding rule; the api-edge Service) and get a clean plan.
- [ ] Optional: when the address quota (`IN_USE_ADDRESSES`, global, now 4) is raised, bring
      Oregon back:
      `gcloud compute forwarding-rules create moqom-dev-relay-us-west1-quic --region us-west1 --load-balancing-scheme EXTERNAL --ip-protocol UDP --ports 443 --address moqom-dev-relay-us-west1 --backend-service moqom-dev-relay-us-west1`,
      then turn the MIG autoscaler back on (min 1, max 3, CPU 0.5).
- [ ] Check alerts still reach elliott@moqom.cloud: the nine compliance policies are wired;
      send a test notification from one.

## 5. Mail (you)

- [ ] Send a message to hello@moqom.cloud and security@moqom.cloud from an outside address and
      confirm both arrive.
- [ ] `security.txt` and the security page point to security@ and the key that's published.

## 6. Legal and IP (you, with counsel)

- [ ] IP assignment from every contributor to the company is signed.
- [ ] Decide on patent filings before anything below goes public. `docs/PATENT-ASSESSMENT.md`
      and `docs/INVENTION-DISCLOSURE-TRUST.md` list the candidates; nothing is filed yet.
      A public post or open-sourced code starts the one-year US grace period and ends novelty
      in most other countries.
- [ ] Counsel reviews the terms of service, privacy policy and the pricing claims ("15% under
      Agora", "at least 1¢ under"); the comparison is against Agora's public list price, so
      keep a dated copy of that page.

## 7. Open source (you)

- [ ] Decide which repos open at launch: the Swift client shell (`moqom-swift-public` →
      `moqom-swift`), Go and Rust backend SDKs, the MCP server. The media engine stays
      private.
- [ ] Pick the org they live under, add the licences and CLA, and check each for secrets and
      internal URLs before flipping visibility.
- [ ] Every link on moqom.cloud and moqom.dev to a GitHub repo resolves for a logged-out user.

## 8. Fresh-machine run

- [ ] Someone who hasn't seen the code follows the moqom.dev quickstart on a clean Mac:
      sign up, top up, mint a token, join a room from the sample app, see usage and a charge.
      Fix every place they get stuck.
- [ ] Self-hosted path: run a relay with a relay key, report usage, see the $0.05/GiB charge.

## 9. Launch assets

- [ ] Product Hunt: thumbnail 240×240, gallery images 1270×760 (at least three: the room UI,
      glass-to-glass latency, pricing), a 60–90 s video, the SVG logo.
- [ ] Re-read `SHOW-HN.md` and `PRODUCT-HUNT.md` against the live sites on the day; every number
      should match moqom.cloud/benchmarks and /pricing.
- [ ] A short demo room link that works without an account, rate-limited.

## 10. Launch days

- [ ] Show HN and Product Hunt on different days. Show HN on a weekday morning US Eastern; Product
      Hunt goes live at 12:01 am Pacific.
- [ ] Be at the keyboard for the first six hours of each to answer comments.
- [ ] Watch: control and relay dashboards, Stripe payments, sign-up errors, the status page.
- [ ] Have a rollback ready: the previous control image digest
      (`sha256:a07c8df9521af0cfc8701587669d86461c3377d93a09eff170b9518aa34d2513`) and
      `kubectl -n moqom set image deploy/moqom-dev-control moqom-dev-control=us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-control@<digest>`.
