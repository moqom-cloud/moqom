# Operator console (admin.moqom.cloud)

The operator console is a static app (`sites/moqom-admin`) for MOQOM staff. It calls
`/v1/operator/*` on the control plane's public billing listener (`api.moqom.cloud`).

## How access is decided

Two independent gates. Either one alone keeps tenants out.

1. **IAP** in front of `admin.moqom.cloud` decides who can load the app at all.
2. **The control plane** decides whether a request is an operator's. Every `/v1/operator/*` request needs
   `Authorization: Bearer <Google ID token>`. The token is RS256, verified against Google's published keys, and must
   meet all of these:
   - `aud` is exactly `MOQOM_OPERATOR_GOOGLE_CLIENT_ID`, with a single audience;
   - `iss` is Google;
   - the token is unexpired;
   - `email_verified` is true;
   - `email` is in `MOQOM_OPERATOR_EMAILS`;
   - `hd` is `MOQOM_OPERATOR_HOSTED_DOMAIN`, when that is set.

   Tenant API keys (`mqk_…`) can never pass, because they are not JWTs. No long-lived operator secret exists to leak.

Other properties:

- **Rate limits:** 120 requests a minute per address, and 30 changes an hour per operator.
- **CORS:** the operator routes answer only `MOQOM_OPERATOR_ORIGINS`. The tenant API's CORS origins never reach them.
- **Audit:** every authenticated request first writes a `requested` row to `billing_operator_audit`. If that write
  fails, the request answers 503 and does nothing. Changes then add an `ok` or `failed: …` row. Refused requests
  write no row (they are unauthenticated) and are logged as `event=operator_refused`.
- **Credit adjustments** require a reason (4–500 characters, shown to the tenant in their ledger) and an idempotency
  key. A retry with the same key applies once. The same key with different values answers 409.

The operator API is **off** unless both `MOQOM_OPERATOR_GOOGLE_CLIENT_ID` and `MOQOM_OPERATOR_EMAILS` are set. Setting
only one of them refuses to start.

## Deploy steps

Nothing below has been run. The OAuth client ID is not a secret: it appears in the built page. No Secret Manager
entry is needed.

### 1. OAuth client

Do this in Google Cloud console → APIs & Services, in project `moqom-503700`.

1. Set the OAuth consent screen's user type to **Internal**, so only `moqom.cloud` Workspace accounts can sign in.
2. Under Credentials, create an OAuth client ID of type **Web application**, named `moqom-admin`:
   - Authorized JavaScript origins: `https://admin.moqom.cloud`
   - Authorized redirect URIs: `https://admin.moqom.cloud/` (with the trailing slash, exactly)
3. Note the client ID (`…apps.googleusercontent.com`). Steps 2 and 3 both use it.

### 2. Control plane environment

Add the following to the billing environment in `deploy/envs/<env>/main.tf`, inside `billing_env`, then apply:

```hcl
MOQOM_OPERATOR_GOOGLE_CLIENT_ID = var.operator_google_client_id   # from step 1
MOQOM_OPERATOR_EMAILS           = "elliott@moqom.cloud"           # comma-separated
MOQOM_OPERATOR_HOSTED_DOMAIN    = "moqom.cloud"
MOQOM_OPERATOR_ORIGINS          = "https://admin.moqom.cloud"
```

Also make sure `MOQOM_BILLING_CORS_ORIGINS` (`var.billing_cors_origins`) includes `https://moqom.cloud`. The client
console at `/account/` calls `/v1/billing/*`, `/v1/billing/ledger` and `/v1/rooms*` from that origin.

The control plane creates `billing_operator_audit` from `internal/billing/postgres/schema.sql` at start-up. To make
the table append-only even for the control plane's own role, run this once as the database owner:

```sql
REVOKE UPDATE, DELETE, TRUNCATE ON billing_operator_audit FROM "<control plane role>";
```

### 3. Build and deploy the app on Cloud Run

Build from the `sites/` directory, because the app reuses the public site's styles:

```sh
cd sites
IMAGE=us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-admin:$(date +%Y%m%d%H%M)
docker build --platform=linux/amd64 -f moqom-admin/Dockerfile \
  --build-arg MOQOM_ADMIN_GOOGLE_CLIENT_ID=<client ID from step 1> \
  --build-arg MOQOM_ADMIN_API=https://api.moqom.cloud \
  -t "$IMAGE" .
docker push "$IMAGE"
```

Then deploy it, without allowing unauthenticated access:

```sh
gcloud run deploy moqom-admin --project=moqom-503700 --region=us-west1 \
  --image=us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-admin:<tag> \
  --port=8080 --no-allow-unauthenticated --ingress=internal-and-cloud-load-balancing \
  --cpu=1 --memory=128Mi --min-instances=0 --max-instances=2
```

### 4. IAP and the domain

The supported path is a global external HTTPS load balancer with a serverless NEG and IAP on the backend service:

```sh
P=moqom-503700; R=us-west1
gcloud compute network-endpoint-groups create moqom-admin-neg --project=$P --region=$R \
  --network-endpoint-type=serverless --cloud-run-service=moqom-admin
gcloud compute backend-services create moqom-admin-backend --project=$P --global \
  --load-balancing-scheme=EXTERNAL_MANAGED
gcloud compute backend-services add-backend moqom-admin-backend --project=$P --global \
  --network-endpoint-group=moqom-admin-neg --network-endpoint-group-region=$R
gcloud compute backend-services update moqom-admin-backend --project=$P --global --iap=enabled
gcloud compute url-maps create moqom-admin-map --project=$P --default-service=moqom-admin-backend
gcloud compute ssl-certificates create moqom-admin-cert --project=$P --global --domains=admin.moqom.cloud
gcloud compute target-https-proxies create moqom-admin-proxy --project=$P \
  --url-map=moqom-admin-map --ssl-certificates=moqom-admin-cert
gcloud compute addresses create moqom-admin-ip --project=$P --global
gcloud compute forwarding-rules create moqom-admin-https --project=$P --global \
  --load-balancing-scheme=EXTERNAL_MANAGED --address=moqom-admin-ip \
  --target-https-proxy=moqom-admin-proxy --ports=443

# Who may pass IAP.
gcloud iap web add-iam-policy-binding --project=$P --resource-type=backend-services \
  --service=moqom-admin-backend --member=user:elliott@moqom.cloud --role=roles/iap.httpsResourceAccessor

# IAP's service agent must be able to invoke the service.
gcloud run services add-iam-policy-binding moqom-admin --project=$P --region=$R \
  --member=serviceAccount:service-<project number>@gcp-sa-iap.iam.gserviceaccount.com --role=roles/run.invoker
```

**DNS:** add an `A` record for `admin.moqom.cloud` pointing at the `moqom-admin-ip` address
(`gcloud compute addresses describe moqom-admin-ip --global --format='value(address)'`). The managed certificate
becomes active once the record resolves, which can take up to an hour.

### 5. Check

These checks need someone signed in, so they are a manual step. Each should give the stated result:

- Open `https://admin.moqom.cloud` in a private window. IAP should ask for Google sign-in.
- Sign in with the console's "Sign in with Google". The header should show your email.
- Request `/v1/operator/whoami` with a tenant key, for example
  `curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer mqk_…" https://api.moqom.cloud/v1/operator/whoami`.
  It should answer `401`.
- Open the Audit log tab. It should show the `whoami` and `tenants` requests you just made.

## Local development

```sh
# Control plane with billing on a laptop (memory store), operator API on:
MOQOM_BILLING=on MOQOM_BILLING_ALLOW_MEMORY=true MOQOM_BILLING_CORS_ORIGINS=http://localhost:4321 \
MOQOM_OPERATOR_GOOGLE_CLIENT_ID=<client ID> MOQOM_OPERATOR_EMAILS=you@moqom.cloud \
MOQOM_OPERATOR_ORIGINS=http://localhost:4322 MOQOM_CONTROL_ALLOW_PLAINTEXT=true … go run ./cmd/moqom-control

# Console against it, on http://localhost:4322:
cd sites/moqom-admin && MOQOM_ADMIN_API=http://localhost:8090 npm run dev
```

To sign in locally with Google, use a separate development OAuth client with origin `http://localhost:4322` and
redirect URI `http://localhost:4322/`, and pass its ID as `MOQOM_ADMIN_GOOGLE_CLIENT_ID` to both processes. The dev
build (`MOQOM_ADMIN_DEV=1`) also allows an `http://localhost` API and adds a field for pasting an ID token, for tests
against a control plane given a local key set. Deployable builds allow neither, and refuse to build without a
client ID.
