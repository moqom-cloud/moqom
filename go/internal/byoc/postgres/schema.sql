-- Customer relay enrolments: which overlay identity serves which tenant.
--
-- identity_name is unique across revoked rows too, so a name once handed to a
-- tenant is never handed to another. Rows are never deleted: a revoked one is
-- the record of when a relay stopped being trusted.
CREATE TABLE IF NOT EXISTS byoc_enrollments (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    name          text NOT NULL,
    identity_id   text NOT NULL,
    identity_name text NOT NULL,
    created_by    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    CONSTRAINT byoc_enrollments_identity_name UNIQUE (identity_name)
);

CREATE INDEX IF NOT EXISTS byoc_enrollments_by_tenant ON byoc_enrollments (tenant, created_at DESC);

-- Relay pools arrived after the table existed. '' is a relay in no pool.
ALTER TABLE byoc_enrollments ADD COLUMN IF NOT EXISTS pool text NOT NULL DEFAULT '';
