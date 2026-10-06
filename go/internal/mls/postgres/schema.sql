-- The MLS delivery service's tables.
--
-- Applied at startup, idempotently, like the control plane's own schema. None
-- of it is secret: key packages are signed public keys, and commits and
-- Welcomes carry their secrets HPKE-sealed to members. Losing all of it costs
-- every live room one re-founding of its group, which is why it shares the
-- control plane's database rather than demanding stronger guarantees of its own.

-- One row per room that has ever had a group. Kept when a group is dropped,
-- so `last_seq` keeps rising and a device's cursor from the old group can
-- never land inside the new one's log.
CREATE TABLE IF NOT EXISTS mls_rooms (
    tenant      text        NOT NULL,
    room        text        NOT NULL,
    group_id    bytea,
    epoch       bigint      NOT NULL DEFAULT 0,
    last_seq    bigint      NOT NULL DEFAULT 0,
    founded_at  timestamptz,
    PRIMARY KEY (tenant, room)
);

CREATE TABLE IF NOT EXISTS mls_messages (
    tenant      text        NOT NULL,
    room        text        NOT NULL,
    seq         bigint      NOT NULL,
    kind        text        NOT NULL,
    epoch       bigint      NOT NULL,
    sender      text        NOT NULL,
    -- Who a Welcome is for. Empty for a commit, which is for everybody.
    recipients  text[]      NOT NULL DEFAULT '{}',
    data        bytea       NOT NULL,
    at          timestamptz NOT NULL,
    PRIMARY KEY (tenant, room, seq)
);

CREATE TABLE IF NOT EXISTS mls_devices (
    tenant      text        NOT NULL,
    room        text        NOT NULL,
    device      text        NOT NULL,
    member      boolean     NOT NULL DEFAULT false,
    last_seen   timestamptz NOT NULL,
    PRIMARY KEY (tenant, room, device)
);

CREATE TABLE IF NOT EXISTS mls_key_packages (
    id          bigserial   PRIMARY KEY,
    tenant      text        NOT NULL,
    room        text        NOT NULL,
    device      text        NOT NULL,
    last_resort boolean     NOT NULL,
    data        bytea       NOT NULL
);

CREATE INDEX IF NOT EXISTS mls_key_packages_device
    ON mls_key_packages (tenant, room, device, last_resort, id);
