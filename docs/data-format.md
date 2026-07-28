# The MOQOM record format

How application data is laid out on MOQOM tracks: the catalog, the room control
log, and the shared space track.

This document is normative. Swift, Rust, Kotlin, and TypeScript clients must all
produce and accept exactly what is described here. The Swift implementation is
`MoqomCore/DataRecord.swift`, and it is the reference until another one
disagrees, at which point this document decides which of them is wrong.

This is **not** the MOQT wire format. Draft-19 message layouts are fixed by the
specification and are implemented separately; nothing here changes a byte of
them. This describes only the *payload* of objects on tracks whose contents
MOQOM defines.

## Why there is a format here at all

MOQOM's data tracks began as JSON, which is fine for two of the three. A room
control entry arrives eight times in thirty seconds. A catalog arrives once per
broadcast. Neither notices a parse.

The shared space track is different in kind. It carries every moving entity at
tick rate to every viewer, so a parse per tick lands on the frame budget of the
device least able to afford it, and it allocates a dictionary and a clutch of
strings each time in order to describe numbers that were already numbers.
Measured on a fifty-entity tick, the encoding below is 1.9x smaller than JSON and
about 25x faster to read.

FlatBuffers was the plan of record and would have worked. What it costs is a
runtime — roughly 7,400 lines in Swift, and the equivalent again in three more
languages — in an SDK that has no third-party dependencies at all and that
hand-ported an entire draft-19 codec rather than acquire one. What it sells over
this format is random access into an unparsed buffer through vtables and offset
tables, and none of these tracks need that: each is read front to back, once, by
whoever asked for it.

So this format is the part that mattered, which is much narrower than a general
serialisation library.

## Records

A **record** is a sequence of fields and nothing else. There is no header, no
length, and no field count — the enclosing MOQT object already says how many
bytes there are, and a record ends when they run out.

A **field** is a key followed by a payload.

The **key** is a QUIC variable-length integer (RFC 9000 §16) holding:

```
tag << 2 | wireType
```

The **wire type** is two bits, and its only job is to let a reader step over a
field it does not recognise:

| Value | Name | Payload |
|---|---|---|
| 0 | `varint` | one QUIC varint |
| 1 | `bytes` | a varint length, then exactly that many bytes |
| 2 | `fixed32` | exactly 4 bytes |
| 3 | `fixed64` | exactly 8 bytes |

The **tag** identifies the field within its record. Tags are permanent: changing
what a tag means is a format break, which is what a version field is for.

Because a one-byte varint holds 0 through 63 and two of those bits are the wire
type, **tags 1 to 15 encode their key in a single byte**. Fields that repeat
belong there. Tags from 16 to 1,023 cost two bytes.

### Derived types

- **Strings** are `bytes` holding UTF-8. Invalid UTF-8 is an error, not a lossy
  substitution.
- **Booleans** are `varint`, 0 or 1. A reader must treat any non-zero value as
  true.
- **Signed integers** are `varint`, zigzagged: `(n << 1) ^ (n >> 63)`, so that
  small negatives stay short. Decode with `(u >> 1) ^ -(u & 1)`.
- **Floats** are `fixed32`, IEEE 754 single precision.
- **Doubles** are `fixed64`, IEEE 754 double precision.
- **Nested records** are `bytes` holding a record.
- **Repeated fields** are the same tag appearing more than once, in order.
  There is no packed encoding and no count.

### Absence

An absent field means *unchanged* or *the default*, never *null*. This is what
keeps a still entity from costing anything, and it is why a delta on a quiet
board is zero bytes rather than an empty object sixty times a second.

A consequence worth stating: a field whose presence identifies the type of a
value — as in a tagged union encoded as one-of-several-tags — must be written
even when it holds a default, because omitting it does not encode a default, it
encodes no value at all.

### Integer range

The varints are QUIC's, so they carry **62 bits**, not 64. Signed varints are
zigzagged and so carry 61 bits and a sign.

This is inherited deliberately. Encoding integers exactly as MOQT does means an
implementation needs one varint routine rather than two that differ in a corner
nobody tests. Sixty-two bits covers every counter in these tracks with room to
spare — a simulation tick at 60Hz would take two billion years to reach it — and
the values that genuinely span the full width, such as a nanosecond timestamp,
use `fixed64`, which is both exact and shorter.

An encoder must reject a value out of range rather than truncate it.

## Endianness

All fixed-width payloads are **little-endian**.

This is the opposite of MOQT's own framing, where every fixed-width field is
big-endian, and the divergence is the entire reason the fast path is fast. A
little-endian float array is already the memory layout of every processor MOQOM
targets, so a reader can point at it rather than convert it. Big-endian would
mean a byte swap per float, per entity, per tick.

## Fixed-layout arrays

The one place this format does something a tag-length-value encoding normally
does not.

A field of wire type `bytes` may hold a run of fixed-size records: `count`
elements of exactly `stride` bytes, back to back, with no per-element key, no
per-element length, and no padding. The stride is a property of the field,
established by the track's schema rather than carried on the wire.

A reader **must** verify that the payload length is a whole multiple of the
stride, and reject the record otherwise. Doing that once at the boundary is what
makes indexing safe without bounds arithmetic on every access.

Readers should expose these as a view over the received bytes rather than
decoding them into a list. That is the entire point: a renderer walks the run in
place, allocating nothing, and can hand it to a GPU buffer whole.

Where a record needs two parallel runs — handles and poses, say — they are
separate fields, index-aligned, and a reader must reject a record where their
lengths disagree.

## Evolution

A reader **must** skip fields whose tags it does not recognise, using the wire
type to know how far. This is the whole of the versioning story, and it is what
lets a client that shipped last year read a publisher that shipped today.

Adding a field is therefore always safe. Removing one is safe if no reader
requires it. Changing a tag's meaning or its wire type is not safe, and needs a
version bump on the enclosing track.

## Reading is not trusting

These bytes arrive from another participant, by way of a relay that does not
inspect them. With end-to-end encryption on they are authenticated only as having
come from somebody holding the room key — which is everybody in the room. With it
off they are whatever arrived.

Every implementation must therefore:

- bound every read against the remaining length, and never read past the end of
  a buffer;
- never use a length from the wire as an allocation instruction without checking
  it against what is actually present;
- confine a nested record to its own slice, so that over-reading inside it stops
  at the slice rather than continuing into the parent's later fields;
- reject a fixed-layout array whose length is not a multiple of its stride, and
  parallel arrays whose lengths disagree;
- return an error rather than trapping, for every one of these.

The Swift implementation is tested against truncation at every prefix length and
against several thousand random buffers. A new implementation should be held to
the same, and the property to assert is only that it returns or throws — never
that it succeeds.

## Shared enumerations

Some values cross language boundaries and must agree numerically.

**Time trust**, matching `Trust` in `go/internal/timecode` here and in
`moqom-relay/src/clock.rs` in the engine repository:

| Value | Meaning |
|---|---|
| 0 | `fallback` — the local system clock; the reference could not be reached |
| 1 | `synchronised` — measured against the authority, recently |
| 2 | `authoritative` — produced by a reference clock itself |

Fallback is zero on purpose, in all three languages: a field nobody set must read
as "we do not know" rather than as a claim of accuracy. An unrecognised value
must be read as fallback rather than rejected, so that a future peer with a
fourth kind of clock degrades older readers to "do not rely on this" instead of
making them drop the object.

## Track layouts

Each track that uses this format documents its own tags alongside its types.

| Track | Layout | Status |
|---|---|---|
| `.space` | `MoqomKit/Space/SpaceState+Wire.swift` | implemented |
| `.control` | room control log | JSON, to be moved |
| catalog | per-publisher track list | JSON, to be moved |

### The space track, in outline

Worth summarising here because it is the track the format was designed around,
and because its two decisions generalise.

**Entities are numbered, not named.** Entity identifiers are application-chosen
strings, which is right for an API and wrong for something sent sixty times a
second. So they are interned: a snapshot carries the names and assigns each a
32-bit handle, and deltas carry handles alone. This is safe because every group
begins with a snapshot and a subscriber joins at a group boundary, so the mapping
always arrives before anything that uses it; a spawn mid-group carries both. A
snapshot **replaces** the table outright, which is what makes handles survive an
authority handover.

The consequence is that this track's codec is stateful — one encoder per
publication, one decoder per subscription — and that a handle nobody has seen is
an error rather than a skipped entity. It means a snapshot was missed and the
whole delta is suspect.

**Movement is two arrays, not a list of pairs.** Moved handles and moved poses
are separate index-aligned fixed-layout arrays. A pose is 32 bytes — position,
rotation as `(x, y, z, w)`, then a uniform scale, eight little-endian floats — so
the pose array is a contiguous run that a renderer reads in place.
