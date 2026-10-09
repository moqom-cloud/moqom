-- The control plane's tables.
--
-- Applied at startup, and written to be applied to a database that already has
-- them. That is the whole of the migration story today and it is enough for
-- exactly as long as no column ever changes: `IF NOT EXISTS` creates what is
-- missing and is silent about a table whose shape has moved on. Before the
-- second deployment that alters anything, this needs a versioned migration
-- tool. Said here rather than discovered later.
--
-- Two conventions run through it. Times are `timestamptz`, because a moderation
-- log without an offset is evidence nobody can place. And a scope that is
-- "everywhere" is the empty string rather than NULL -- a tenant-wide ban has
-- room = '' -- so that a unique index over the subject actually constrains it:
-- in SQL, two NULLs are not equal, and a nullable room would let one subject be
-- banned tenant-wide twice.

CREATE TABLE IF NOT EXISTS rooms (
    tenant              text        NOT NULL,
    room                text        NOT NULL,
    lifecycle           text        NOT NULL,
    owner               text        NOT NULL DEFAULT '',
    acting_host_user    text        NOT NULL DEFAULT '',
    acting_host_device  text        NOT NULL DEFAULT '',
    starts_at           timestamptz,
    created_at          timestamptz NOT NULL,
    ended_at            timestamptz,

    age_minimum         bigint      NOT NULL DEFAULT 0,
    age_verified        boolean     NOT NULL DEFAULT false,
    recording           text        NOT NULL DEFAULT 'allowed',
    -- Immutable once set, enforced above this layer, and the reason a room's
    -- data cannot be moved by editing a field.
    residency_zone      text        NOT NULL DEFAULT '',
    max_participants    bigint      NOT NULL DEFAULT 0,
    stage_slots         bigint      NOT NULL DEFAULT 0,
    watchdog_limit      bigint      NOT NULL DEFAULT 0,

    PRIMARY KEY (tenant, room)
);

-- Soft state: losing all of it loses nobody's room, which is what makes a relay
-- restart survivable. Cascading is right for the same reason -- a deleted room
-- has no participants to remember -- and the foreign key is what makes saving a
-- participant into a room that does not exist an error rather than an orphan.
CREATE TABLE IF NOT EXISTS participants (
    tenant           text        NOT NULL,
    room             text        NOT NULL,
    username         text        NOT NULL,
    device           text        NOT NULL,

    publish          bigint      NOT NULL DEFAULT 0,
    moderation       bigint      NOT NULL DEFAULT 0,
    is_owner         boolean     NOT NULL DEFAULT false,

    presence         text        NOT NULL,
    self_muted       bigint      NOT NULL DEFAULT 0,
    muted_everyone   bigint      NOT NULL DEFAULT 0,
    muted_audience   bigint      NOT NULL DEFAULT 0,
    spotlighted      boolean     NOT NULL DEFAULT false,
    backstage        boolean     NOT NULL DEFAULT false,

    arrived_via      text        NOT NULL DEFAULT '',
    locale           text        NOT NULL DEFAULT '',
    joined_at        timestamptz NOT NULL,
    last_seen_at     timestamptz NOT NULL,
    relay_id         text        NOT NULL DEFAULT '',
    session_id       text        NOT NULL DEFAULT '',
    remote_addr      text        NOT NULL DEFAULT '',
    published_now    text[]      NOT NULL DEFAULT '{}',

    PRIMARY KEY (tenant, room, username, device),
    FOREIGN KEY (tenant, room) REFERENCES rooms (tenant, room) ON DELETE CASCADE
);

-- No foreign key to rooms, and that is deliberate: a tenant-wide ban belongs to
-- no room, and a ban must outlive the room it was issued in or deleting a room
-- would readmit everybody thrown out of it.
CREATE TABLE IF NOT EXISTS bans (
    id          text        PRIMARY KEY,
    tenant      text        NOT NULL,
    room        text        NOT NULL DEFAULT '',
    kind        text        NOT NULL,
    username    text        NOT NULL DEFAULT '',
    device      text        NOT NULL DEFAULT '',
    ip_prefix   text        NOT NULL DEFAULT '',
    code        text        NOT NULL DEFAULT '',
    reason      text        NOT NULL DEFAULT '',
    expires_at  timestamptz,
    created_at  timestamptz NOT NULL,
    created_by  text        NOT NULL DEFAULT ''
);

-- One row per subject, which is what makes lifting a ban lift it. Two rows for
-- one person would leave one in force after an unban and read to a moderator as
-- a button that does not work. The columns not belonging to a kind are blanked
-- on the way in, so this index means what it says.
CREATE UNIQUE INDEX IF NOT EXISTS bans_subject
    ON bans (tenant, room, kind, username, device, ip_prefix);

CREATE INDEX IF NOT EXISTS bans_by_room ON bans (tenant, room);

-- Keyed by audience-only and by media kind as well as by identity. One
-- participant can be silenced for everybody and hidden from the audience at
-- once, and within either scope can be muted on the microphone and the camera
-- for different lengths of time. A row per kind is the only shape that holds
-- both: merging scopes would make lifting one lift both, and merging kinds made
-- muting a camera turn a microphone back on, because the write replaced the row.
CREATE TABLE IF NOT EXISTS mutes (
    tenant        text        NOT NULL,
    room          text        NOT NULL,
    username      text        NOT NULL,
    device        text        NOT NULL,
    audience_only boolean     NOT NULL,
    media         bigint      NOT NULL,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL,

    PRIMARY KEY (tenant, room, username, device, audience_only, media)
);

-- The first change of shape, and the reason the note at the top of this file
-- says a versioned tool is needed. Done here rather than by hand because a
-- force-mute has to survive the deployment that fixes force-mutes: dropping the
-- table would silently unmute everybody who is muted right now.
--
-- Guarded on the old key, so it runs once and is a no-op on a database that has
-- already been through it or was created above.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_index index_
        JOIN pg_class table_ ON table_.oid = index_.indrelid
        JOIN pg_attribute column_ ON column_.attrelid = table_.oid
                                 AND column_.attname = 'media'
        WHERE table_.relname = 'mutes'
          AND index_.indisprimary
          AND NOT column_.attnum = ANY (index_.indkey)
    ) THEN
        CREATE TABLE mutes_by_kind (LIKE mutes);

        -- One row per bit set in the old mask. The three kinds are audio,
        -- video and screenshare, and their bit positions are shared with Go,
        -- Rust and Swift.
        INSERT INTO mutes_by_kind
        SELECT tenant, room, username, device, audience_only, kind,
               expires_at, created_at
        FROM mutes, unnest(ARRAY[1, 2, 4]) AS kind
        WHERE media & kind <> 0;

        DROP TABLE mutes;
        ALTER TABLE mutes_by_kind RENAME TO mutes;
        ALTER TABLE mutes ADD PRIMARY KEY (tenant, room, username, device, audience_only, media);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS grant_overrides (
    tenant     text    NOT NULL,
    room       text    NOT NULL,
    username   text    NOT NULL,
    device     text    NOT NULL,
    publish    bigint  NOT NULL DEFAULT 0,
    moderation bigint  NOT NULL DEFAULT 0,
    is_owner   boolean NOT NULL DEFAULT false,

    PRIMARY KEY (tenant, room, username, device)
);

CREATE TABLE IF NOT EXISTS watchdogs (
    id            text        NOT NULL,
    tenant        text        NOT NULL,
    room          text        NOT NULL,
    kind          text        NOT NULL,
    custom_kind   text        NOT NULL DEFAULT '',
    operated_by   text        NOT NULL DEFAULT '',
    operator_name text        NOT NULL DEFAULT '',
    descriptor    text        NOT NULL DEFAULT '',
    -- A namespace is an ordered tuple whose fields may contain slashes, so it is
    -- stored as an array. Joining it into a string here would be a rendering
    -- nobody can reverse.
    scope         text[]      NOT NULL DEFAULT '{}',
    health        text        NOT NULL DEFAULT '',
    attached_at   timestamptz NOT NULL,
    billable      boolean     NOT NULL DEFAULT false,
    pinned_layer  text        NOT NULL DEFAULT '',

    PRIMARY KEY (tenant, room, id)
);

CREATE TABLE IF NOT EXISTS moderation_log (
    id                 text        NOT NULL,
    tenant             text        NOT NULL,
    room               text        NOT NULL DEFAULT '',
    at                 timestamptz NOT NULL,
    verb               text        NOT NULL,
    service_account    text        NOT NULL DEFAULT '',
    on_behalf_of       text        NOT NULL DEFAULT '',
    target_username    text        NOT NULL DEFAULT '',
    target_device      text        NOT NULL DEFAULT '',
    target_all_devices boolean     NOT NULL DEFAULT false,
    ban_kind           text        NOT NULL DEFAULT '',
    ban_subject        text        NOT NULL DEFAULT '',
    ban_scope_tenant   boolean     NOT NULL DEFAULT false,
    code               text        NOT NULL DEFAULT '',
    reason             text        NOT NULL DEFAULT '',
    expires_at         timestamptz,
    media              bigint      NOT NULL DEFAULT 0,
    audience_only      boolean     NOT NULL DEFAULT false,
    -- Null when the verb carried no grants, which is most of them. Three
    -- columns rather than a document, because a demotion that cannot be read
    -- back by a query is a demotion nobody can audit.
    grants_publish     bigint,
    grants_moderation  bigint,
    grants_is_owner    boolean,
    enforced           boolean     NOT NULL DEFAULT false,
    decided_by_model   text        NOT NULL DEFAULT '',
    idempotency_key    text        NOT NULL DEFAULT '',

    PRIMARY KEY (tenant, id)
);

-- Where idempotency actually lives. Two replicas handling one redelivered
-- request can only be resolved by whatever holds the constraint, so it is here
-- rather than in a service -- and it is scoped to the tenant because keys come
-- from callers and two customers will eventually choose the same one.
CREATE UNIQUE INDEX IF NOT EXISTS moderation_idempotency
    ON moderation_log (tenant, idempotency_key)
    WHERE idempotency_key <> '';

-- Newest first, which is how an audit view opens and how an appeals queue
-- reads.
CREATE INDEX IF NOT EXISTS moderation_recent
    ON moderation_log (tenant, at DESC, id DESC);

CREATE TABLE IF NOT EXISTS events (
    tenant   text        NOT NULL,
    room     text        NOT NULL,
    -- Contiguous within a room and never reused. The primary key is what
    -- enforces it: two writers racing for the same number is a duplicate key
    -- and a retry, rather than one of them silently overwriting the other.
    sequence bigint      NOT NULL,
    at       timestamptz NOT NULL,
    kind     text        NOT NULL,
    body     bytea       NOT NULL DEFAULT '',

    PRIMARY KEY (tenant, room, sequence)
);

-- Locale arrived after the table existed. CREATE TABLE IF NOT EXISTS above
-- will not reshape a table that is already there; this will.
ALTER TABLE participants ADD COLUMN IF NOT EXISTS locale text NOT NULL DEFAULT '';

-- End-to-end encryption arrived the same way, and its default is the point: every
-- room that existed before the column did becomes a room that requires it. A
-- room is unencrypted only because an UpdateRoomPolicy said so, with a reason,
-- in the moderation log below.
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS e2ee text NOT NULL DEFAULT 'required';

-- What a policy update did to a room's security settings. See
-- control.ModerationEntry.PolicyChange.
ALTER TABLE moderation_log ADD COLUMN IF NOT EXISTS policy_change text NOT NULL DEFAULT '';

-- Direct paths. An empty value and a zero limit read as allowed and four, so
-- rooms from before the columns follow MoqomKit's default.
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS direct_paths text NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS max_direct_participants bigint NOT NULL DEFAULT 0;

-- Private relay pools. A pool is never deleted, only marked so: it is the
-- record of what a tenant ran and was billed for. Regions are a document
-- rather than a table because they are only ever read and written with the
-- pool, and a resize replaces them whole.
CREATE TABLE IF NOT EXISTS relay_pools (
    id          text        PRIMARY KEY,
    tenant      text        NOT NULL,
    name        text        NOT NULL,
    regions     jsonb       NOT NULL DEFAULT '[]',
    self_hosted boolean     NOT NULL DEFAULT false,
    access      text        NOT NULL DEFAULT 'public',
    state       text        NOT NULL,
    created_at  timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS relay_pools_by_tenant ON relay_pools (tenant, created_at, id);

-- The relays that have attached to a pool, one row per relay ID, so a relay
-- that reattaches replaces its row rather than adding a second address.
CREATE TABLE IF NOT EXISTS pool_relays (
    pool     text        NOT NULL REFERENCES relay_pools (id),
    relay_id text        NOT NULL,
    address  text        NOT NULL DEFAULT '',
    region   text        NOT NULL DEFAULT '',
    state    text        NOT NULL,
    seen_at  timestamptz NOT NULL,

    PRIMARY KEY (pool, relay_id)
);

-- A tenant with no row is on the shared fleet. pool is '' unless the mode is
-- private.
CREATE TABLE IF NOT EXISTS tenant_networks (
    tenant text PRIMARY KEY,
    mode   text NOT NULL,
    pool   text NOT NULL DEFAULT ''
);
