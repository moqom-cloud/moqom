#!/bin/sh
# Build each site and upload it under its prefix in the sites bucket.
#
#   deploy/tools/publish-sites.sh                 # the four main sites
#   deploy/tools/publish-sites.sh moqom-dev       # one
#   deploy/tools/publish-sites.sh six10nine       # any directory under sites/
#   MOQOM_SITES_SKIP_BUILD=1 deploy/tools/publish-sites.sh   # upload dist/ as built
#
# The default stays the four main sites rather than every directory, so a
# project site is only published when named. A site directory uploads to the
# bucket prefix of the same name; it serves once the load balancer maps a host
# onto it (`sites` or `project_hosts` in envs/web).
#
# Cloud Build (cloudbuild-sites.yaml) builds in a Node image and uploads from a
# gcloud image, so it sets MOQOM_SITES_SKIP_BUILD for the second step.
#
# The load balancer rewrites a request path onto the site's prefix and does
# nothing cleverer, so every page is uploaded three times: `pricing/index.html`,
# `pricing/` and `pricing`. That is what lets /pricing and /pricing/ both work
# without depending on index-document behaviour behind a path rewrite.
#
# The aliases are excluded from the rsync, so it never deletes them: deleting
# and re-copying left every page but the home page a 404 for the minutes the
# copies took. They are overwritten in place instead, so the site stays up.
#
# Hashed assets are cached for a year. HTML is revalidated by the browser on
# every load (max-age=0) and held five minutes at the edge (s-maxage), and the
# edge is purged once everything is uploaded, so a refresh shows the new build.
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
BUCKET="${MOQOM_SITES_BUCKET:-moqom-sites}"
SITES_DIR="${MOQOM_SITES_DIR:-$(cd "$(dirname "$0")/../../.." && pwd)/sites}"
export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCS="gcloud storage --project=$PROJECT --quiet"
URL_MAP="${MOQOM_SITES_URL_MAP:-moqom-sites}"
HTML_CACHE="public, max-age=0, s-maxage=300, must-revalidate"

say() { printf '==> %s\n' "$*"; }

# put_alias TOKEN OBJECT FILE: upload FILE as exactly OBJECT, with HTML headers.
put_alias() {
    name="$(printf '%s' "$2" | python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.stdin.read(), safe=""))')"
    code="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
        -H "Authorization: Bearer $1" \
        -H "Content-Type: text/html; charset=utf-8" \
        --data-binary @"$3" \
        "https://storage.googleapis.com/upload/storage/v1/b/$BUCKET/o?uploadType=media&name=$name")"
    [ "$code" = 200 ] || return 1
    code="$(curl -s -o /dev/null -w '%{http_code}' -X PATCH \
        -H "Authorization: Bearer $1" -H "Content-Type: application/json" \
        -d "{\"cacheControl\":\"$HTML_CACHE\",\"contentType\":\"text/html; charset=utf-8\"}" \
        "https://storage.googleapis.com/storage/v1/b/$BUCKET/o/$name")"
    [ "$code" = 200 ]
}

if [ "$#" -eq 0 ]; then
    set -- moqom-cloud moqom-dev elliottwave-studio elliottwave-dev
fi

# Check every name before uploading any, so a typo doesn't leave a run half done.
for site in "$@"; do
    case "$site" in
        _* | */* | .* | "") echo "$site: not a site directory name" >&2; exit 1 ;;
    esac
    [ -f "$SITES_DIR/$site/package.json" ] || { echo "$site: no site at $SITES_DIR/$site" >&2; exit 1; }
done

for site in "$@"; do
    dir="$SITES_DIR/$site"
    target="gs://$BUCKET/$site"

    # One publish per site at a time: two share dist/, so one's build renames
    # the hashed assets the other's upload is about to point the HTML at.
    lock="${TMPDIR:-/tmp}/moqom-publish-$site.lock"
    until mkdir "$lock" 2>/dev/null; do
        say "$site: waiting for another publish to finish"
        sleep 5
    done
    trap 'rmdir "$lock" 2>/dev/null' EXIT INT TERM
    if [ "${MOQOM_SITES_SKIP_BUILD:-}" = 1 ]; then
        [ -f "$dir/dist/index.html" ] || { echo "$site: no dist/index.html to upload" >&2; exit 1; }
    else
        say "$site: building"
        (cd "$dir" && pnpm install --frozen-lockfile >/dev/null && pnpm build >/dev/null)
    fi

    say "$site: uploading to $target"
    # Hashed assets first and never deleted: a page cached at the edge for five
    # minutes still points at the previous build's files.
    if [ -d "$dir/dist/_astro" ]; then
        $GCS rsync "$dir/dist/_astro" "$target/_astro" --recursive \
            --cache-control="public, max-age=31536000, immutable" >/dev/null
    fi
    # cp rather than rsync: the root alias objects `$site` and `$site/` make
    # rsync's destination ambiguous. dist/ holds no alias objects; they are
    # written below.
    (cd "$dir/dist" && for entry in *; do
        [ "$entry" = _astro ] && continue
        $GCS cp --recursive --cache-control="$HTML_CACHE" "$entry" "$target/" >/dev/null 2>&1 || exit 1
    done)
    # --exclude only filters the source, so rsync's own delete would still remove
    # old hashed assets and aliases; stale files are removed here instead.
    $GCS ls --recursive "$target/**" | DIST="$dir/dist" TARGET="$target" python3 -c '
import os, re, sys
dist, target = os.environ["DIST"], os.environ["TARGET"] + "/"
for url in sys.stdin.read().split():
    rel = url[len(target):] if url.startswith(target) else ""
    if not rel or rel.startswith("_astro/") or re.fullmatch(r"(.*/)?[^/.]+/?", rel):
        continue
    if not os.path.isfile(os.path.join(dist, rel)):
        print(url)
' | { grep . || true; } | $GCS rm -I >/dev/null 2>&1 || true

    # `gcloud storage cp` writes to `route/index.html` when `route/` already exists
    # as a prefix, so the alias objects go through the JSON API by exact name.
    say "$site: aliasing pages"
    token="$(gcloud auth print-access-token)"
    # Site root aliases (`site` and `site/`) so product hosts that rewrite `/` to
    # the site prefix (e.g. void.khensu.nu → /khensu-void) do not 301 through index.html.
    if [ -f "$dir/dist/index.html" ]; then
        for alias in "$site" "$site/"; do
            put_alias "$token" "$alias" "$dir/dist/index.html" || {
                say "$site: failed to alias /$alias"
                exit 1
            }
        done
    fi
    (cd "$dir/dist" && find . -name index.html ! -path ./index.html) | while read -r page; do
        route="${page#./}"
        route="${route%/index.html}"
        for alias in "$route" "$route/"; do
            put_alias "$token" "$site/$alias" "$dir/dist/$route/index.html" || {
                say "$site: failed to alias /$alias"
                exit 1
            }
        done
    done
    rmdir "$lock" 2>/dev/null
done

# One purge for the whole run. Without --host it covers every site on the map,
# which is cheaper than one purge per host and only costs a few cache misses.
if [ "${MOQOM_SITES_SKIP_PURGE:-}" != 1 ]; then
    say "purging the edge cache"
    gcloud compute url-maps invalidate-cdn-cache "$URL_MAP" --global --path="/*" \
        --project="$PROJECT" --quiet >/dev/null
fi
say "done. A refresh shows the new build."
