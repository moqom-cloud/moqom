#!/usr/bin/env python3
"""Check docs/PLAN.md against the repository it describes.

A plan nobody verifies becomes fiction at the speed of the project, and the
failure is silent in both directions: work lands and the plan still calls it
pending, or an item is marked done and the code was never written. Both look
identical to a reader, which is what makes them expensive.

Two rules, run against a real checkout:

  completed  every piece of evidence must be present. Missing evidence is a
             claim the tree does not support.

  pending    if *all* the evidence is present, the work happened and nobody
             updated the plan. Partial presence is ignored -- that is just
             work in flight.

in_progress is exempt from both, deliberately. It is the honest state for
something half-built, and an item sits there precisely when neither rule can
say anything useful.

Evidence is a path glob, or a mapping of `path` plus a `contains` regex that
must match somewhere in the file. Items with no evidence are counted and
reported but never fail: most of this plan is argument rather than artifact,
and pretending otherwise would turn the check into a rubber stamp.

Evidence may also name another repository with `repo`, because the plan covers
the whole platform while the Swift SDK lives in `moqom-swift` -- SwiftPM cannot
consume a package nested in a monorepo, so that split is forced rather than
chosen. Such entries resolve against a sibling checkout when one exists, and are
otherwise reported as external and left unjudged. They never fail the run.

That is a deliberately weaker guarantee, and it is weak in the right place. What
these entries would prove here is that a file exists; the other repository's own
CI builds those files and runs their tests, which is a stronger claim than this
check could make. What matters is that an unresolved entry is never silently
counted as verified.
"""

from __future__ import annotations

import pathlib
import re
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = pathlib.Path(__file__).resolve().parent.parent
PLAN = ROOT / "docs/PLAN.md"

GREEN, RED, YELLOW, DIM, BOLD, OFF = (
    "\033[32m",
    "\033[31m",
    "\033[33m",
    "\033[2m",
    "\033[1m",
    "\033[0m",
)

if not sys.stdout.isatty():
    GREEN = RED = YELLOW = DIM = BOLD = OFF = ""


def check_one(entry: str | dict) -> tuple[str, str | None]:
    """Judge one evidence entry.

    Returns ("ok", None), ("fail", why), or ("external", where) -- the last for
    evidence in another repository with no checkout to resolve it against.
    """
    if isinstance(entry, str):
        pattern, contains, repo = entry, None, None
    else:
        pattern = entry["path"]
        contains = entry.get("contains")
        repo = entry.get("repo")

    base = ROOT
    if repo:
        # A sibling of this repo, which is where a working checkout lands if you
        # cloned both into the same directory. Anywhere else and we cannot know.
        base = ROOT.parent / repo
        if not base.is_dir():
            return "external", f"{repo}/{pattern} (no checkout at {base})"

    matches = sorted(base.glob(pattern))
    if not matches:
        where = f"{repo}/{pattern}" if repo else pattern
        return "fail", f"no file matches {where}"

    if contains is None:
        return "ok", None

    regex = re.compile(contains)
    for path in matches:
        try:
            if regex.search(path.read_text(errors="replace")):
                return "ok", None
        except OSError as exc:
            return "fail", f"{path} unreadable: {exc}"

    where = f"{repo}/{pattern}" if repo else pattern
    return "fail", f"{where} exists but nothing in it matches /{contains}/"


def main() -> int:
    if not PLAN.exists():
        sys.exit(f"plan not found: {PLAN}")

    text = PLAN.read_text()
    try:
        front = yaml.safe_load(text.split("---", 2)[1])
    except (IndexError, yaml.YAMLError) as exc:
        sys.exit(f"cannot parse plan frontmatter: {exc}")

    todos = front.get("todos") or []
    if not todos:
        sys.exit("plan has no todos")

    seen: set[str] = set()
    failures: list[tuple[str, str, list[str]]] = []
    external: list[tuple[str, list[str]]] = []
    verified = unverifiable = 0

    for todo in todos:
        tid = todo.get("id")
        status = todo.get("status")

        if not tid:
            failures.append(("<no id>", "an item is missing its id", []))
            continue
        if tid in seen:
            failures.append((tid, "duplicate id", []))
        seen.add(tid)

        if status not in {"pending", "in_progress", "completed", "cancelled"}:
            failures.append((tid, f"unknown status {status!r}", []))
            continue

        evidence = todo.get("evidence") or []
        if not evidence:
            unverifiable += 1
            continue

        judged = [check_one(e) for e in evidence]
        problems = [why for verdict, why in judged if verdict == "fail" and why]
        elsewhere = [where for verdict, where in judged if verdict == "external" and where]

        if elsewhere:
            external.append((tid, elsewhere))

        if status == "completed":
            if problems:
                failures.append(
                    (tid, "marked completed, but the tree disagrees", problems)
                )
            elif elsewhere:
                # Some or all of the evidence could not be resolved, so this is
                # not verified and must not be counted as such.
                pass
            else:
                verified += 1
        elif status == "pending" and not problems and not elsewhere:
            failures.append(
                (
                    tid,
                    "marked pending, but every piece of its evidence is present "
                    "-- the work landed and the plan was not updated",
                    [str(e if isinstance(e, str) else e["path"]) for e in evidence],
                )
            )
        else:
            verified += 1

    total = len(todos)
    print(f"{BOLD}plan check{OFF}  {PLAN.relative_to(ROOT)}  ({total} items)")
    print(
        f"  {GREEN}{verified} verified against the tree{OFF}, "
        f"{DIM}{unverifiable} carry no evidence{OFF}"
    )

    if external:
        print(
            f"  {YELLOW}{len(external)} item(s) cite evidence in another repo "
            f"with no checkout to resolve it{OFF}"
        )
        for tid, wheres in external:
            print(f"      {DIM}{tid}{OFF}")
            for where in wheres:
                print(f"        {YELLOW}{where}{OFF}")
        print(
            f"  {DIM}Not counted as verified. Clone the sibling repo to check "
            f"these locally; its own CI{OFF}"
        )
        print(f"  {DIM}builds and tests those files, which proves more.{OFF}")

    if unverifiable:
        print(
            f"  {DIM}Items without evidence are unchecked. Most of this plan is "
            f"argument, not artifact,{OFF}"
        )
        print(
            f"  {DIM}but anything that will leave a file behind should name it.{OFF}"
        )

    if not failures:
        print(f"\n{GREEN}plan and tree agree{OFF}")
        return 0

    print(f"\n{RED}{BOLD}{len(failures)} disagreement(s):{OFF}\n")
    for tid, why, detail in failures:
        print(f"  {RED}{tid}{OFF}: {why}")
        for line in detail:
            print(f"      {YELLOW}{line}{OFF}")
        print()

    print(f"{DIM}Fix the code or fix the plan. Do not fix the check.{OFF}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
