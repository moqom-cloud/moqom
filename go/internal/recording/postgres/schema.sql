-- Hosted recordings. See internal/recording.
--
-- The record is a JSON document, with the columns a query selects on copied
-- out beside it. Nothing reads a recording in SQL except to find it, and the
-- document changes shape with the recorder far more often than any of these
-- columns will.

CREATE TABLE IF NOT EXISTS recordings (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    room        text NOT NULL,
    state       text NOT NULL,
    created_at  timestamptz NOT NULL,
    worker      text NOT NULL DEFAULT '',
    lease_until timestamptz,
    doc         jsonb NOT NULL
);

-- One recording at a time per room, held by the database rather than by a
-- check that two replicas could both pass.
CREATE UNIQUE INDEX IF NOT EXISTS recordings_one_active
    ON recordings (tenant, room)
    WHERE state IN ('pending', 'recording', 'stopping');

CREATE INDEX IF NOT EXISTS recordings_claimable
    ON recordings (created_at)
    WHERE state IN ('pending', 'recording', 'stopping');

CREATE INDEX IF NOT EXISTS recordings_listing
    ON recordings (tenant, created_at DESC, id DESC);
