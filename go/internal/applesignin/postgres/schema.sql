-- Apple sign-in hosted by MOQOM: each tenant's settings, and the last App
-- Attest counter seen for each attested key.
CREATE TABLE IF NOT EXISTS apple_sign_in (
    tenant            text PRIMARY KEY,
    team_id           text NOT NULL,
    bundle_ids        text[] NOT NULL,
    rooms             text[] NOT NULL,
    publish           smallint NOT NULL,
    allow_development boolean NOT NULL DEFAULT false,
    updated_at        timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS apple_attest_counters (
    key_id  text PRIMARY KEY,
    counter bigint NOT NULL
);
