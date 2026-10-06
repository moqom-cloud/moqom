CREATE TABLE IF NOT EXISTS billing_tenants (
    id                   text PRIMARY KEY,
    name                 text NOT NULL,
    email                text NOT NULL,
    status               text NOT NULL,
    stripe_customer      text NOT NULL DEFAULT '',
    payment_method       text NOT NULL DEFAULT '',
    auto_top_up_enabled  boolean NOT NULL DEFAULT false,
    auto_top_up_below    bigint NOT NULL DEFAULT 0,
    auto_top_up_amount   bigint NOT NULL DEFAULT 0,
    top_up_pending_since timestamptz,
    last_top_up_error    text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS billing_tenants_stripe_customer
    ON billing_tenants (stripe_customer) WHERE stripe_customer <> '';

-- Append-only. Corrections are new entries, never updates, so the ledger is
-- its own audit trail and a balance is always the sum of what happened.
CREATE TABLE IF NOT EXISTS billing_ledger (
    seq             bigserial PRIMARY KEY,
    id              text NOT NULL,
    tenant          text NOT NULL REFERENCES billing_tenants (id),
    kind            text NOT NULL,
    amount          bigint NOT NULL,
    cost            bigint NOT NULL DEFAULT 0,
    capped          boolean NOT NULL DEFAULT false,
    idempotency_key text NOT NULL UNIQUE,
    reference       text NOT NULL DEFAULT '',
    detail          text NOT NULL DEFAULT '',
    at              timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS billing_ledger_tenant ON billing_ledger (tenant, seq DESC);

-- A month's spend is a range sum over one tenant's usage, asked at every
-- admission of a tenant with a spend limit.
CREATE INDEX IF NOT EXISTS billing_ledger_tenant_usage
    ON billing_ledger (tenant, at) INCLUDE (amount) WHERE kind = 'usage';

-- Added after the table, so a deployment that already has it migrates in place.
ALTER TABLE billing_tenants
    ADD COLUMN IF NOT EXISTS spend_limit      bigint   NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS spend_alerts     bigint[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS spend_alerts_off boolean  NOT NULL DEFAULT false;

-- One row per threshold that has alerted in a month. The primary key is the
-- claim: two replicas inserting the same row at once send one notice.
CREATE TABLE IF NOT EXISTS billing_spend_alerts (
    tenant    text   NOT NULL REFERENCES billing_tenants (id),
    month     date   NOT NULL,
    threshold bigint NOT NULL,
    fired_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, month, threshold)
);

CREATE TABLE IF NOT EXISTS billing_keys (
    id         text PRIMARY KEY,
    tenant     text NOT NULL REFERENCES billing_tenants (id),
    name       text NOT NULL,
    kind       text NOT NULL,
    hash       text NOT NULL,
    created_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX IF NOT EXISTS billing_keys_tenant ON billing_keys (tenant, created_at);

-- Hardening, added in place. pepper_version '' is a bare SHA-256 from before
-- peppering, rehashed at the key's next use.
ALTER TABLE billing_keys
    ADD COLUMN IF NOT EXISTS pepper_version   text        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS expires_at       timestamptz,
    ADD COLUMN IF NOT EXISTS allowed_networks text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS rate_limit       integer     NOT NULL DEFAULT 0;

-- Key use, counted per minute per operation, address and outcome, so its size
-- follows how varied use is rather than how heavy. No foreign key to
-- billing_keys: the retention sweep prunes the two on their own schedules.
-- Never holds a secret.
CREATE TABLE IF NOT EXISTS billing_key_use (
    key_id    text        NOT NULL,
    tenant    text        NOT NULL,
    minute    timestamptz NOT NULL,
    operation text        NOT NULL,
    address   text        NOT NULL,
    outcome   text        NOT NULL,
    detail    text        NOT NULL DEFAULT '',
    count     bigint      NOT NULL,
    first_at  timestamptz NOT NULL,
    last_at   timestamptz NOT NULL,
    PRIMARY KEY (key_id, minute, operation, address, outcome, detail)
);

CREATE INDEX IF NOT EXISTS billing_key_use_tenant ON billing_key_use (tenant, last_at DESC);
CREATE INDEX IF NOT EXISTS billing_key_use_last_at ON billing_key_use (last_at);

CREATE TABLE IF NOT EXISTS billing_jobs (
    id           text PRIMARY KEY,
    tenant       text NOT NULL,
    status       text NOT NULL,
    current_step text NOT NULL DEFAULT '',
    completed    text[] NOT NULL DEFAULT '{}',
    error        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL,
    completed_at timestamptz
);

-- What MOQOM staff did and looked at through the operator API. Append-only,
-- like the ledger: nothing in the application updates or deletes a row.
CREATE TABLE IF NOT EXISTS billing_operator_audit (
    seq      bigserial PRIMARY KEY,
    id       text NOT NULL UNIQUE,
    at       timestamptz NOT NULL,
    operator text NOT NULL,
    subject  text NOT NULL DEFAULT '',
    address  text NOT NULL DEFAULT '',
    action   text NOT NULL,
    tenant   text NOT NULL DEFAULT '',
    detail   text NOT NULL DEFAULT '',
    outcome  text NOT NULL
);

CREATE INDEX IF NOT EXISTS billing_operator_audit_tenant ON billing_operator_audit (tenant, seq DESC);

-- Direct participant-seconds each tenant has used in a calendar month (UTC),
-- which is what the free allowance is measured against.
CREATE TABLE IF NOT EXISTS billing_direct_months (
    tenant  text   NOT NULL,
    month   date   NOT NULL,
    seconds bigint NOT NULL,
    PRIMARY KEY (tenant, month)
);

-- Each usage record's claim on that total, under the record's ledger
-- idempotency key, so a re-sent record is priced from where it was the first
-- time rather than counted again.
CREATE TABLE IF NOT EXISTS billing_direct_claims (
    idempotency_key text        PRIMARY KEY,
    tenant          text        NOT NULL,
    month           date        NOT NULL,
    seconds         bigint      NOT NULL,
    prior           bigint      NOT NULL,
    at              timestamptz NOT NULL DEFAULT now()
);
