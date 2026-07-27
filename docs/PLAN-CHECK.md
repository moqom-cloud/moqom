# Keeping the plan honest

`docs/PLAN.md` is the design document and the work list, and it is checked
against the repository on every push by `tools/plan-check.py`.

The reason it is checked at all: a plan drifts silently, and it drifts in two
directions that look identical to a reader. Work lands and the item still says
pending, so the plan understates what exists and someone rebuilds it. Or an item
says completed and the code was never written, so the plan overstates what
exists and someone builds on top of nothing. Neither is visible by reading.
Both are trivially visible by looking at the tree.

## The two rules

**A completed item must have its evidence present.** If the plan claims
something is done, the files it produced are on disk. Missing evidence fails the
build.

**A pending item whose evidence is *entirely* present fails too.** This is the
direction that actually catches things. It means the work happened and nobody
came back to the plan. Partial presence is ignored, because that is just work in
flight.

`in_progress` is exempt from both, on purpose. It is the honest state for
something half-built, and an item sits there exactly when neither rule can say
anything useful. Use it, and use the `remaining` field to say how far.

## Writing evidence

Evidence is a list. Each entry is either a path glob:

```yaml
    evidence:
      - rs/moqom-proto/src/varint.rs
      - deploy/modules/*/main.tf
```

or a path with a regex that must match inside it:

```yaml
    evidence:
      - path: rs/Cargo.toml
        contains: '\[patch\.crates-io\]'
```

The `contains` form is for claims a filename cannot support. `vendor/quinn/`
existing proves someone copied a directory; `[patch.crates-io]` in `rs/Cargo.toml`
proves the build actually resolves to it. Reach for it whenever the interesting
part of an item is a decision rather than a file — an ALPN string, a lint being
denied, a feature flag being off.

Point evidence at things that only exist if the work was really done. A
`Cargo.toml` proves a crate was created, not that it does anything, so pair it
with a source file or a symbol. Tests are often the best evidence available: a
passing test file is harder to produce accidentally than a module.

## Items with no evidence

Most of the plan has none, and that is correct rather than a gap to close. The
document is mostly argument — why MoQ beats WebRTC on the metrics that matter,
where E2EE has a real adversary and where it does not, why a novel codec loses
to hardware decode. None of that is checkable by looking for files, and
attaching fake evidence to make the number go up would turn the check into a
rubber stamp.

The checker reports the unverifiable count on every run so the ratio stays
visible. The rule of thumb: **if finishing the item will leave a file behind,
name that file.** If it will only change how someone thinks, do not.

## When the check fails

Fix the code or fix the plan. Do not fix the check.

Loosening evidence to make a red build green is the one move that destroys the
value of the whole arrangement, because after it happens once nobody can trust a
green run again. If evidence is genuinely wrong — a file moved, a crate was
renamed — correct it in the same commit as the move, with the reason in the
commit message.

## Relationship to the Cursor plan

`docs/PLAN.md` is canonical. A copy exists in Cursor's plan storage and is not
authoritative; it drifted badly before this file existed, at one point still
listing the codec as pending after eighteen control messages, a data plane, and
a fuzzing harness had landed. Edit the repository copy. If you plan in Cursor,
copy the result here in the same commit as the work.
