# Hosted recording

The hosted recorder records a room for a tenant without the tenant running
anything. It is billed the way the product promises: no recording fee. The
recorder is a viewer and is billed as one, for the bytes it receives, and
storage is passed through at cost.

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
worker's last upload and the new worker's first subscription. The archive's
`session` records make that gap visible.

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
| The room requires E2EE | Payloads are ciphertext, and the recorder holds no media key. |
| The catalog names a `keyID` for the track | The same, for a track encrypted in a room where E2EE is optional. |
| Codecs other than H.264, HEVC and Opus; PCM; data tracks | There is no sample mapping without parsing or transcoding the payload. Transcoding is a different product with a different price, and it would put plaintext media in a MOQOM process. |
| Format `ARCHIVE` | It was not asked for. |

### Manifest

`manifest-a{attempt}.json` has `"format": "moqom-recording"` and `"version": 1`.
It contains the recording, tenant, room, attempt, whether the room was
encrypted, start and end times, the reconnect count, and per track: kind, codec,
object and duplicate counts, and `cmaf_skipped`. It also lists every artifact
with its name, kind (`archive` / `cmaf` / `manifest`), track, size, CRC32C and
first/last times.

## End-to-end encryption

Rooms are E2EE by default, and the recorder never has a media key. In an E2EE
room it stores exactly the ciphertext it receives: archive only, with
`encrypted: true` in every header, the manifest and the API. A customer decrypts
it with their own keys. No plaintext of an E2EE room is ever in a MOQOM process.

The recorder decides a track is ciphertext if **either** the room requires E2EE
(the assignment's `e2ee_required`, from the room's policy at claim time) **or**
the publisher's catalog names a key for the track. Either alone is enough. A
room switched to optional mid-recording is treated as encrypted until the next
claim, which is the safe direction.

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

**Storage.** Billing has no storage line yet, so the control plane calls
`recording.StorageMeter.Stored` once per finished attempt with tenant, room,
recording, attempt, bytes and file count. The default `LogMeter` logs it as
`recording_storage_metered`. Billing it at cost needs a meter in
`internal/billing` that turns those events (and the bucket's lifecycle,
which moves objects to colder classes) into byte-days. That is a follow-up, and
it belongs to the billing package, not this one.

## Operating it

Control plane environment:

| Variable | |
| --- | --- |
| `MOQOM_RECORDING_BUCKET` | Turns hosted recording on. Unset, `Recordings` answers `FAILED_PRECONDITION` and the worker link is not registered. |
| `MOQOM_RECORDER_THUMBPRINT` | Required with the bucket. Print it with `moqom-recorder thumbprint KEY`. |
| `MOQOM_RECORDING_SIGNER_ACCOUNT` | Service account to sign download links as (`module.recorder.signer_account`). Unset, `GetRecording` omits links. |

Records go in Postgres, in the table `recordings` (its own schema and advisory
lock, beside the room store), whenever the control plane runs on Postgres.
Otherwise they are kept in memory with a warning. One active recording per room
is enforced by a unique partial index. Claims use `FOR UPDATE SKIP LOCKED`, so
any number of workers and control-plane replicas can claim at once.

A worker is one `is_recorder: true` entry in the service-account file, shared by
the pool. That account reaches `RecorderLink` and nothing else. The admin API,
`/join`, the live map and the rooms bridge all refuse it.

Deploying, without applying anything until it has been reviewed:

1. `moqom-recorder thumbprint --generate recorder-key.pem`. Put the key in the
   `…-recorder-key` secret and the thumbprint in the control plane's
   `MOQOM_RECORDER_THUMBPRINT`.
2. Add `{"name": "recorders", "credential": "…", "is_recorder": true}` to the
   accounts file, and put the credential in `…-recorder-credential`.
3. Issue a RelayLink client certificate for the recorder from the link CA. The
   control plane's gRPC port requires one.
4. Build `moqom-engine/Dockerfile.recorder` and add a `module "recorder"` block
   to the environment. Pass `recording_bucket = module.data.recording_bucket`,
   the control plane's service account, `control_endpoint`, `relay`, and the
   Kubernetes secret holding `recorder-key.pem`, `credential`, `link-ca.pem`,
   `link-cert.pem`, `link-key.pem` and `relay-cert.pem`. `sync-secrets.sh` does
   not fill that secret yet.
5. Set `MOQOM_RECORDING_SIGNER_ACCOUNT` from the module's output.

Workers have no probes. A stuck worker stops renewing, its lease lapses, and
the recording moves on, which is the recovery a restart would give. The spool is
an `emptyDir` sized by `spool_size`, and the root filesystem is read-only.

The workers' Google account can create objects and nothing else. A compromised
worker cannot read any tenant's recordings. A separate reader account that
nothing runs as holds `objectViewer`, and the control plane signs V4 links as it
through IAM `signBlob`. No key file exists anywhere.

## Known gaps

- **Room ceiling.** Relays count every session against `max_participants`,
  including the recorder's, so a full room refuses the recorder (it retries and
  eventually fails). Exempting it is a relay change: skip the ceiling for the
  recorder identity, as the control plane skips the roster. It was left out to
  keep relay edits to none.
- **Warm start.** A recorder joining a room in progress starts each track at the
  next group. There is no joining FETCH for the group in flight, and an
  existing catalog is seen when its next object arrives.
- **Signing key.** The recorder's ES256 key is a file from Secret Manager.
  Holding it in Cloud KMS would mean `DeviceKey` signing through a remote
  signer. The proof is signed on every connect and renewal, so the latency
  matters less than the dependency.
- **Storage billing**, as above.
- **Scheduled rooms** record from creation, as above.
- **Postgres conformance** runs only with `MOQOM_TEST_POSTGRES` set.
