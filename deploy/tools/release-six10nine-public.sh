#!/bin/sh
# Publish six10nine as a fresh public repository with a single commit.
#
#   deploy/tools/release-six10nine-public.sh            # dry run: build the snapshot, show what ships
#   deploy/tools/release-six10nine-public.sh --publish  # rename the private repo, create the public one, push
#
# Only after the provisional patent application covering six10nine is filed:
# publishing the source is a public disclosure of what it does.
#
# The private repository keeps its full history under a new name. The public one
# starts from a snapshot of the working tree as it is on disk, staged and
# unstaged changes included, so nothing in the private history reaches it. The
# working tree itself is only read: no add, commit, stash or checkout there.
set -eu

SRC="${SIX10NINE_DIR:-$HOME/Projects/six10nine}"
ORG="${SIX10NINE_ORG:-elliott-wave-labs}"
NAME=six10nine
PRIVATE_NAME="${SIX10NINE_PRIVATE_NAME:-six10nine-private}"
HOMEPAGE="https://six10nine.elliottwave.studio"
DESCRIPTION="The same ⌘ chords in every Mac app. Free and source-available, by elliottwave.studio lab."

# Never shipped. Paths are relative to the repository root.
EXCLUDE="
docs/PATENT-ANALYSIS.md
target/
.build/
.swiftpm/
DerivedData/
.DS_Store
"

say() { printf '==> %s\n' "$*"; }

publish=false
[ "${1:-}" = "--publish" ] && publish=true

[ -d "$SRC/.git" ] || { echo "no git repository at $SRC" >&2; exit 1; }
[ -f "$SRC/LICENSE.md" ] || { echo "$SRC has no LICENSE.md; refusing to publish unlicensed code" >&2; exit 1; }

stage="$(mktemp -d "${TMPDIR:-/tmp}/six10nine-public.XXXXXX")"
say "snapshot in $stage"

# Tracked and untracked-but-not-ignored files, as they are on disk now.
(cd "$SRC" && git ls-files -z --cached --others --exclude-standard) |
    (cd "$SRC" && xargs -0 -I{} sh -c '[ -e "$1" ] && printf "%s\0" "$1"' _ {}) |
    (cd "$SRC" && rsync -a --from0 --files-from=- ./ "$stage/")

# The private architecture doc discusses the patent analysis; the public tree
# ships the edition written for it under the usual name.
if [ -f "$stage/docs/ARCHITECTURE.public.md" ]; then
    mv "$stage/docs/ARCHITECTURE.public.md" "$stage/docs/ARCHITECTURE.md"
fi

for path in $EXCLUDE; do
    rm -rf "${stage:?}/$path"
    find "$stage" -name "$(basename "$path")" -path "*$path*" -exec rm -rf {} + 2>/dev/null || true
done

# Anything that names the excluded analysis would point at it.
if rg -n -i "PATENT-ANALYSIS" "$stage" >/dev/null 2>&1; then
    say "files still referring to the excluded analysis (fix in $SRC, then rerun):"
    rg -n -i "PATENT-ANALYSIS" "$stage"
    exit 1
fi
# Key material and env files never belong in a public tree.
leaks="$(cd "$stage" && find . \( -name '*.pem' -o -name '*.p8' -o -name '*.p12' -o -name '*.key' -o -name '.env' -o -name '.env.*' -o -name '*.mobileprovision' \) -print)"
if [ -n "$leaks" ]; then
    say "refusing: key material in the snapshot:"
    printf '%s\n' "$leaks"
    exit 1
fi

(
    cd "$stage"
    git init -q -b main
    git add -A
    git -c user.name="Elliott Wave LLC" -c user.email="hello@elliottwave.studio" \
        commit -q -m "six10nine: first public release"
)
say "snapshot: $(cd "$stage" && git ls-files | wc -l | tr -d ' ') files, $(du -sh "$stage" | cut -f1)"
(cd "$stage" && git ls-files | sed 's|/.*||' | sort | uniq -c)

if ! $publish; then
    say "dry run. Review $stage, then rerun with --publish."
    exit 0
fi

printf 'Rename %s/%s to %s (kept private) and create a PUBLIC %s/%s? Type "publish": ' \
    "$ORG" "$NAME" "$PRIVATE_NAME" "$ORG" "$NAME"
read -r answer
[ "$answer" = publish ] || { say "stopped"; exit 1; }

say "renaming the private repository to $PRIVATE_NAME"
gh repo rename "$PRIVATE_NAME" --repo "$ORG/$NAME" --yes
gh repo edit "$ORG/$PRIVATE_NAME" --visibility private --accept-visibility-change-consequences >/dev/null

say "creating public $ORG/$NAME"
gh repo create "$ORG/$NAME" --public --description "$DESCRIPTION" --homepage "$HOMEPAGE" \
    --disable-wiki >/dev/null
(cd "$stage" && git remote add origin "https://github.com/$ORG/$NAME.git" && git push -q -u origin main)
gh repo edit "$ORG/$NAME" --enable-discussions=false --add-topic macos --add-topic keyboard-shortcuts \
    --add-topic swift --add-topic mcp >/dev/null

say "done: https://github.com/$ORG/$NAME"
say "your local clone still points at the renamed private repo; GitHub redirects the old URL only until the new repo took the name, so update it:"
say "  git -C $SRC remote set-url origin https://github.com/$ORG/$PRIVATE_NAME.git"
