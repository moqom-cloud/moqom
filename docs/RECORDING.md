# Hosted recording

The hosted recorder records a room for a tenant without the tenant running
anything. It is billed the way the product promises: no recording fee. The
recorder is a viewer and is billed as one, for the bytes it receives, and
storage is charged per GiB-month at cost, with no markup.

This document is for whoever operates or changes it. The public guide
(`sites/moqom-dev/.../guides/recording.md`) covers the API from a customer's side.

| Piece | Where |
| --- | --- |
| Recorder binary and worker | `moqom-engine/moqom-recorder` |
| Namespace watch it relies on | `moqom-engine/moqom-client/src/watch.rs` |
| Admin API | `proto/moqom/admin/v1/recordings.proto` → `go/internal/server/recordings.go` |
| Worker link | `proto/moqom/recorder/v1/recorder.proto` → `go/internal/recorderpb` |
| Recording records | `go/internal/recording` (memory and `postgres/`) |
| Image | `moqom-engine/Dockerfile.recorder` |
| Infrastructure | `deploy/modules/recorder` |

## How a recording runs

1. `StartRecording` checks the room is open and its policy is not `prohibited`,
   attaches a `recording` watchdog operated by the platform, writes the
   moderation entry, and stores a `PENDING` recording. Its storage prefix is
   `{tenant}/{room}/{id}`, each part path-escaped.
2. A worker calls `RecorderLink.Claim`. It gets the oldest claimable recording,
   a lease (45 s by default), an attempt number, and a relay token for the room.
   The token's subject is `moqom-recorder`, its audience the tenant and its
   `cnf.jkt` the recorder key's thumbprint. It has no publish rights.
3. The worker joins the relay with that token, watches the room's namespace, and
   for each participant follows `.catalog` and subscribes to every track the
   catalog names. Tracks that appear, and participants who arrive, are picked up
   as they are announced. A participant who leaves gets a `departed` record and
   their files are closed.
4. Every lease/3 the worker calls `Renew` with its progress. The answer carries
   `stop` (a stop was requested, the room closed, the policy became
   `prohibited`, or `max_duration` passed) and, once the held token is past half
   its life, a fresh one. The worker hands the fresh token to its open session
   as a token update, so the session is not torn down to renew.
5. On stop, deadline, or `SIGTERM`, the worker closes every file, uploads what
   is left, writes the manifest and calls `Finish` with the artifact list. The
   recording is `COMPLETED`, or `FAILED` with the error if the recorder gave up.
   The watchdog is detached, and the storage meter is told what was stored.

### Resuming

A recording outlives its worker:

- **Reconnects** within one claim reuse the same writers. The recorder backs off
  and reconnects with the newest token. Each new session writes a `session`
  record, and objects already archived (the last 64 groups per track) are
  dropped as duplicates. The recorder gives up after 24 consecutive failed
  sessions; a session that lasted 30 s resets the count.
- **Handover.** On `SIGTERM` the worker finishes with `requeue = true`, which
  keeps the recording `RECORDING` with no worker. A worker that disappears
  without a word simply stops renewing. In both cases the next claim gets the
  next attempt number. File names carry the attempt, so a second worker's files
  never overwrite the first's, and both attempts' artifacts stay on the
  recording.
- **Lost lease.** A worker whose renewal is refused (`FAILED_PRECONDITION`,
  `NOT_FOUND`, `PERMISSION_DENIED`), or that could not renew for a whole lease,
  closes its files and stops without reporting, because another worker owns the
  recording now.

What is lost at a handover is whatever the relay delivered between the old
worker's last upload and the new worker's first subscription, less what the
catch-up below recovers. The archive's `session` records make any gap visible.

### Joining a room in progress

A subscription starts at the next group, so a recorder that joins mid-group
(at the start of a recording in a live room, after a reconnect, or after a
handover) would otherwise lose the group in flight. It fetches that group from
the relay's cache:

- **Media.** The recorder waits for the track's first live object, at group `H`
  and object `k`, and then fetches what it missed:
  - If `k` is 0, the group in flight was `H - 1`, which has now finished. The
    recorder fetches all of `H - 1`, so the track starts cleanly at the first
    object of `H - 1`.
  - If `k` is greater than 0, it fetches objects `0` to `k - 1` of `H`.

  The fetch waits for the first live object because a fetch sent when the
  subscription starts would stop at the live edge, and the rest of that group
  would be neither fetched nor subscribed.
- **The catalog.** Fetched as soon as the recorder subscribes, because a catalog
  that does not change is never sent again. On draft-19 this is a joining FETCH
  of the current group. On draft-22 it is a filtered FETCH with
  `LOCATION_FILTER` set to the latest group.

Overlap with live delivery is absorbed by the archive's duplicate check. The
CMAF muxer's three-group reorder window puts a late-arriving `H - 1` in order.

The relay caches 8 groups or 16 seconds per track by default. When the group
has aged out, or the fetch is refused, the track starts at the next group. Each
track's manifest entry has a `joins` list, with one entry per session, that says
what happened:

| `from` | Meaning |
| --- | --- |
| `track_start` | The first live object was group 0, object 0; nothing was missed. |
| `caught_up` | The fetch returned the group from object 0. `group` is where the track starts. |
| `next_group` | Nothing could be fetched. The track starts at `group` and object 0. `reason` says why. |
| `mid_group` | Only part of the group was fetched, or nothing was, and live delivery began part-way into a group. `reason` says why. |

The recorder logs `the group in flight was not caught up` with the reason
whenever it falls back.

### Full rooms

The recorder never takes a participant's seat. Relays leave the recorder out
when they count a room's occupancy against its ceiling (`max_participants`).
The recorder is still admitted when the room is at its ceiling.

A session counts as the recorder only when both of these hold:

- its token's subject is `moqom-recorder`, which `MintClientToken` refuses to
  mint for anyone else;
- it has no publish rights, no capabilities, and is not an owner, backstage or a
  relay.

A token that names the recorder and can publish is counted and refused like
anyone else. A closed room refuses the recorder too. This takes a relay with
this build. An older relay counts the recorder as before.

## What gets written

```text
gs://{bucket}/{tenant}/{room}/{recording}/
  manifest-a{attempt}.json
  {user}/{device}/{track}/a{attempt}-s{segment:05}.mqa          archive, every track
  {user}/{device}/{track}/a{attempt}-r{rendition}-p{part:05}.mp4 CMAF, where possible
```

`{user}` and `{device}` come from the publisher's namespace. Path components are
sanitised, so a track name cannot add directories.

### Archive (`.mqa`)

The archive is always written, for every track, encrypted or not. It needs
nothing from the payload, so it is the one format that cannot be wrong about
media. Anything else, CMAF included, can be rebuilt from it.

```text
file    := "MOQOMAR1" header record*
header  := len:u32 json:len crc:u32          crc32c(json)
record  := kind:u8 len:u32 body:len crc:u32  crc32c(kind ‖ len ‖ body)

0x01 object   group:u64 object:u64 received:u64 plen:u32 properties:plen payload
0x02 session  ordinal:u32 at:u64             a (re)subscription began
0x03 departed at:u64                         the publisher left
0xFF end      objects:u64 bytes:u64 at:u64
```

All fields are big-endian. Times are Unix microseconds on the recorder's clock.
`properties` is the object's MOQT property block as it arrived, which is where
the publisher's capture time is. The header JSON names the recording, the
namespace, the track, the attempt, the segment and whether payloads are
ciphertext.

Objects are in **arrival order**. Groups travel on separate streams and can
interleave, and the archive does not hide that. A file with no `end` record was
cut short, and every record before the tear is still valid on its own.

A file rotates at the first group boundary after 256 MiB, which you can change
with `--segment-mib`. `moqom-recorder inspect FILE [--objects]` prints a file.

### CMAF (`.mp4`)

CMAF is written only where it is a repackaging, never a transcode. MOQOM's media
framing already has the shape of an MP4 sample:

- A video object is one access unit, as length-prefixed NAL units. The parameter
  sets travel in the `configuration` property on each group's first object.
  That is an `avc1`/`hvc1` sample and its `avcC`/`hvcC`.
- An Opus object is one packet, which is one `Opus` sample.
- Capture time becomes decode time.

Each `.mp4` part is self-contained (`ftyp`+`moov`, then fragments) and plays on
its own. Video is one fragment per group, so every fragment opens on a keyframe.
Audio is cut about once a second. Up to three groups (64 MiB) are held so that
interleaved groups go into the file in order. A group that arrives after a later
one has been written is skipped in CMAF and kept in the archive. A change of
parameter sets starts a new rendition (`-r{n}`).

CMAF is not written, and the reason goes in the manifest's `cmaf_skipped`, for:

| Case | Why |
| --- | --- |
| Every room: payloads are ciphertext | The hosted recorder holds no media key. A tenant that wants a playable recording runs an agent; see below. |
| Codecs other than H.264, HEVC and Opus; PCM; data tracks | There is no sample mapping without parsing or transcoding the payload. Transcoding is a different product with a different price, and it would put plaintext media in a MOQOM process. |
| Format `ARCHIVE` | It was not asked for. |

### Manifest

`manifest-a{attempt}.json` has `"format": "moqom-recording"` and `"version": 1`.
It contains the recording, tenant, room, attempt, whether the room was
encrypted, start and end times, the reconnect count, and per track: kind, codec,
object and duplicate counts, and `cmaf_skipped`. It also lists every artifact
with its name, kind (`archive` / `cmaf` / `manifest`), track, size, CRC32C and
first/last times, and the track's `joins` (see
[Joining a room in progress](#joining-a-room-in-progress)).

## End-to-end encryption

Every room is end-to-end encrypted, and the hosted recorder is never a member of
a room's group, so it never holds a media key. It stores exactly the ciphertext
it receives: archive only, with `encrypted: true` in every header, the manifest
and the API. No plaintext of any room is ever in a MOQOM process. The catalog is
sealed too; the recorder files tracks by the catalog's disclosure (names, kinds
and video geometry), and archives the sealed catalog and the signed control log
beside the media.

That archive cannot be opened later by anyone, the tenant included: the keys
are per [MLS](https://www.rfc-editor.org/rfc/rfc9420) epoch, held only by the group's members, and gone when the epoch
ends. A tenant that needs plaintext — a recording it can play, or a classifier
that has to see the picture — runs an **agent** instead: MoqomKit's `RoomAgent`,
on the tenant's own machines. It joins as a member of the group under the
username `watchdog.<id>`, decrypts there, and hands the tenant's code decrypted
media. The agent is never hidden:

1. Attach the watchdog first (`AttachWatchdog`), with the descriptor people will
   be shown. `MintClientToken` mints a `watchdog.<id>` token only while a
   watchdog with that ID is attached to the room and has not failed, and the
   token grants watching and nothing else.
2. Every member's device sees the agent's leaf in the group — the epoch
   authenticator proves they all see the same leaves — and withholds its own
   media until the room's signed control log discloses that watchdog. Detaching
   the watchdog holds media again until the agent has been removed.

The recorder decides a track is ciphertext if **either** the room requires E2EE
(the assignment's `e2ee_required`) **or** the catalog's disclosure names a key
for the track. Every room now does both; the rule stays so that an archive made
before encryption became mandatory reads the same.

## Hidden, and disclosed

The recorder never appears in the roster:

- It never announces, so relays do not list it in the room's namespace.
- The control plane drops its relay sessions before they become participants.
  A session is the recorder's only if its username is `moqom-recorder` **and**
  its device is the one derived from the recorder key's thumbprint.
  `MintClientToken` refuses both that username and that thumbprint, so no
  participant can claim to be the recorder.

It is disclosed instead as the room's `recording` watchdog (`OperatedBy: platform`,
billable), which sets `Room.is_recording`, appears in room state, and is written
to the moderation log on attach and detach.

## Policy

| Policy | Effect |
| --- | --- |
| `allowed` | `StartRecording` works. |
| `prohibited` | `StartRecording` returns `FAILED_PRECONDITION`. Setting it on a room stops recordings in progress at the next renewal. A claim of such a recording ends it. |
| `required` | A recording starts on `CreateRoom` and on any `UpdateRoomPolicy` that leaves it `required` while none is running. `StopRecording` returns `FAILED_PRECONDITION` while the room is open. `CloseRoom` stops it. |

A required recording that cannot start (no recorder configured, a store error)
is logged. The room is still created, because the room is what the caller asked
for. The next policy write tries again. With no worker running, the recording
waits as `PENDING`, which `ListRecordings` shows.

A scheduled room is open, so `required` starts its recording at creation, and
the recorder sits in an empty room until it goes live. A follow-up would defer
the claim to `starts_at`.

## Billing

**Viewing.** The recorder is a subscriber on the relay like any other, under the
tenant's audience with the identity `moqom-recorder`, so it is billed through
the same usage path at the same rate as a viewer. Pinning a lower layer is not
offered for hosted recordings yet: it records what it is sent. Note that **relays
do not emit usage records yet**, so today this is billed exactly as much as
every other viewer is, which is not at all. Nothing here needs to change when
that lands.

**Storage.** Charged per GiB-month stored, at cost: Cloud Storage's list price
with no markup, as the public pricing pages state. There is no fee and no
ceiling. The charge goes on the tenant's prepaid ledger as a usage entry, so it:

- counts against the balance, spend limits and alerts like any other usage;
- appears in `GET /v1/usage/summary` as a line with
  `product: "recording_storage"`;
- also appears under `storage` in the same response, with `storage_gib_months`;
- is not included in `by_role` or `by_tier`.

`GET /v1/rates` lists the cost per class as
`recording_storage_cost_per_gib_month`.

How it is metered (`recording.StorageBilling`, which runs hourly in the control
plane):

- `Finish` stamps each artifact with `stored_at`.
- Each attempt's files age together. They move through the storage classes on
  the bucket's lifecycle days (`MOQOM_RECORDING_LIFECYCLE`) and stop costing
  anything when the lifecycle deletes them.
- Each closed UTC day is billed once per attempt and class. The idempotency key
  is `recording:attempt:day:class`, so a pass that runs twice charges once.
- `storage_billed_through` on the recording is the watermark.
- `storage_done` is set once everything has been deleted.

Unregistered and exempt tenants are not charged, as with relayed usage.
`StorageMeter.Stored`, which logs `recording_storage_metered` at each `Finish`,
is unchanged.

The prices are Cloud Storage's list prices, in
`billing.GCSStorageListPrice` (source:
<https://cloud.google.com/storage/pricing>, retrieved 7 October 2026). The table
lists North American regions (us-west1, us-central1, us-east1) and the `US`
multi-region:

| Class | Regional, per GiB-month | US multi-region, per GiB-month |
| --- | --- | --- |
| Standard | $0.020 | $0.026 |
| Nearline | $0.010 | $0.015 |
| Coldline | $0.004 | $0.00875 |
| Archive | $0.0012 | $0.003 |

`MOQOM_RECORDING_BUCKET_LOCATION` picks the column. If it is unset, the regional
column is used. A location that is not in the table stops the control plane at
startup rather than guessing a price. These are list prices, not a negotiated
rate, and have not been confirmed by the owner.

Not billed:

- operation charges;
- Nearline, Coldline and Archive retrieval and early-deletion fees;
- download egress (see Known gaps).

## Operating it

Control plane environment:

| Variable | |
| --- | --- |
| `MOQOM_RECORDING_BUCKET` | Turns hosted recording on. Unset, `Recordings` answers `FAILED_PRECONDITION` and the worker link is not registered. |
| `MOQOM_RECORDER_THUMBPRINT` | Required with the bucket. Print it with `moqom-recorder thumbprint KEY`. |
| `MOQOM_RECORDING_SIGNER_ACCOUNT` | Service account to sign download links as (`module.recorder.signer_account`). Unset, `GetRecording` omits links. |
| `MOQOM_RECORDING_LIFECYCLE` | The bucket's lifecycle, e.g. `nearline=30,coldline=90,archive=365,delete=0` (days, 0 for never). Storage is billed in the class this says an object is in, so it must match the bucket. Unset is Standard, kept for ever. |
| `MOQOM_RECORDING_BUCKET_LOCATION` | The bucket's location, which picks the storage price. Unset is a North American region. |

Records go in Postgres, in the table `recordings` (its own schema and advisory
lock, beside the room store), whenever the control plane runs on Postgres.
Otherwise they are kept in memory with a warning. One active recording per room
is enforced by a unique partial index. Claims use `FOR UPDATE SKIP LOCKED`, so
any number of workers and control-plane replicas can claim at once.

A worker is one `is_recorder: true` entry in the service-account file, shared by
the pool. That account reaches `RecorderLink` and nothing else. The admin API,
`/join`, the live map and the rooms bridge all refuse it.

Deploying to dev is described in `deploy/README.md` under "Hosted recording in
dev", which has the exact commands. In outline:

1. Apply `envs/dev` with `recorder_image` and `recorder_thumbprint` unset. This
   creates the recorder's accounts, grants and secret containers.
2. Run `deploy/tools/recorder-secrets.sh`, then `sync-secrets.sh`. The first
   script:
   - generates the key (with `moqom-recorder thumbprint --generate`);
   - adds `{"name": "recorders", "credential": "…", "is_recorder": true}` to the
     accounts file;
   - issues the recorder's link client certificate;
   - writes the Kubernetes secret the workers mount (`recorder-key.pem`,
     `credential`, `link-ca.pem`, `link-cert.pem`, `link-key.pem`,
     `relay-cert.pem`);
   - prints the thumbprint.
3. Build `moqom-engine/Dockerfile.recorder`. Then set `recorder_image` (by
   digest) and `recorder_thumbprint`, and apply. The control plane then gets the
   bucket, the thumbprint, `MOQOM_RECORDING_SIGNER_ACCOUNT` from
   `module.recorder.signer_account`, and the bucket's location and lifecycle.

Prod has no `module "recorder"` block yet.

Workers have no probes. A stuck worker stops renewing, its lease lapses, and
the recording moves on, which is the recovery a restart would give. The spool is
an `emptyDir` sized by `spool_size`, and the root filesystem is read-only.

The workers' Google account can create objects and nothing else. A compromised
worker cannot read any tenant's recordings. A separate reader account that
nothing runs as holds `objectViewer`, and the control plane signs V4 links as it
through IAM `signBlob`. No key file exists anywhere.

## Known gaps

- **Older relays.** The room-ceiling exemption and the fetch that catches up
  the group in flight both need a relay with this build. An older relay counts
  the recorder against the ceiling and refuses the fetch. A refused fetch
  starts the track at the next group, and the manifest records why.
- **Catch-up window.** Only what the relay still caches can be caught up (8
  groups or 16 seconds per track by default). A group that has aged out is
  recorded as `next_group`.
- **Signing key.** The recorder's ML-DSA-87 key is a file from Secret Manager.
  Holding it in Cloud KMS would mean `DeviceKey` signing through a remote
  signer. The proof is signed on every connect and renewal, so the latency
  matters less than the dependency.
- **Download egress is not billed.** Downloads use signed Cloud Storage links,
  which bypass the control plane, so nothing here sees the bytes. Billing them
  would need the bucket's usage logs or a download proxy.
- **Storage prices are list prices in code** and need the owner's
  confirmation. A bucket outside the listed locations needs a row added to
  `GCSStorageListPrice`.
- **Recordings deleted outside the lifecycle.** There is no API to delete a
  recording's files, so storage billing assumes only the lifecycle deletes
  them. Deleting files by hand keeps billing them until the lifecycle's
  deletion day.
- **Scheduled rooms** record from creation, as above.
- **Postgres conformance** runs only with `MOQOM_TEST_POSTGRES` set.
