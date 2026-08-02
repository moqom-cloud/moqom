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

-- Keyed by audience-only as well as by identity, because one participant can be
-- silenced for everybody and hidden from the audience at once. Merging them
-- would make lifting one lift both.
CREATE TABLE IF NOT EXISTS mutes (
    tenant        text        NOT NULL,
    room          text        NOT NULL,
    username      text        NOT NULL,
    device        text        NOT NULL,
    audience_only boolean     NOT NULL,
    media         bigint      NOT NULL,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL,

    PRIMARY KEY (tenant, room, username, device, audience_only)
);

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
