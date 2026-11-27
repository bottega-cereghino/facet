#!/usr/bin/env bash
# Cut a facet release with release-please, from this machine.
#
#   scripts/release.sh pr    open or update the release PR from the commits on main
#   scripts/release.sh tag   after the release PR merged: tag it, publish the release,
#                            and copy the version's CHANGELOG entry into its notes
#   scripts/release.sh ship  after the release PR merged: do `tag`, install the tag into
#                            $FACET_INSTALL_ROOT/<tag>/, copy it to .../current/, run the
#                            swap command, and print the one-line way back
#
# ship reads two environment variables. FACET_INSTALL_ROOT is the install root
# (default $HOME/.local/lib/facet). FACET_SWAP_CMD names the operator's command
# that moves the queue onto the new version; it runs with the new tag and the
# previous tag as its two arguments. Unset, nothing is swapped and ship says so.
# The service itself is not in this repository.
#
# Why not a workflow: the org does not let GitHub Actions open pull requests, and
# a PR opened with GITHUB_TOKEN starts no workflows, so its required checks would
# never run. Run from here, the PR is opened with the `gh` login, like every other
# PR, and CI runs on it. Issue #219 has the whole reasoning.
#
# The token reaches release-please in a file only this user can read, removed on
# exit, so it never shows on a command line.
#
# release-please and its dependencies come from scripts/release/package-lock.json,
# installed with `npm ci --ignore-scripts` into a temporary directory on each run:
# every package the token is handed to is pinned by version and checksum, and none
# runs an install script. Renovate moves the pin in scripts/release/package.json.
set -euo pipefail

REPO_URL=bottega-cereghino/facet

case "${1:-}" in
  pr) cmd=release-pr ;;
  tag | ship) cmd=github-release ;;
  *)
    echo "usage: scripts/release.sh pr|tag|ship" >&2
    echo "  pr   open or update the release PR from the commits on main" >&2
    echo "  tag  after the release PR merged: tag it and publish the release" >&2
    echo "  ship after the release PR merged: tag, install the tag, swap the queue onto it" >&2
    exit 2
    ;;
esac

gh auth status >/dev/null 2>&1 || {
  echo "gh is not logged in. fix: gh auth login" >&2
  exit 1
}

cd "$(git rev-parse --show-toplevel)"
token_file=$(mktemp)
notes_file=$(mktemp)
changelog_file=$(mktemp)
npm_dir=$(mktemp -d)
trap 'rm -f "$token_file" "$notes_file" "$changelog_file"; rm -rf "$npm_dir"' EXIT
chmod 600 "$token_file"

cp scripts/release/package.json scripts/release/package-lock.json "$npm_dir"/
npm ci --prefix "$npm_dir" --ignore-scripts --no-audit --no-fund --loglevel=error >&2 || {
  echo "could not install release-please from scripts/release/package-lock.json. fix: check npm and the network, then run again" >&2
  exit 1
}

gh auth token >"$token_file"

# release-please reads --token as a file when it looks like a path.
"$npm_dir/node_modules/.bin/release-please" "$cmd" \
  --repo-url="$REPO_URL" \
  --target-branch=main \
  --config-file=release-please-config.json \
  --manifest-file=.release-please-manifest.json \
  --token="$token_file"

[ "$cmd" = github-release ] || exit 0

# release-please writes the release notes from the commit titles, not from the
# CHANGELOG, so an entry reworded on the release PR would not reach the release
# page. Copy the entry for the version on main there (#228).
git fetch --quiet origin main
version=$(git show origin/main:.release-please-manifest.json | sed -n 's/.*"\.": *"\([^"]*\)".*/\1/p')
# Write the file first. awk exits at the next heading, and in a pipe that gives
# git show SIGPIPE (exit 141 under pipefail) on a large changelog (#546).
git show origin/main:CHANGELOG.md >"$changelog_file"
awk -v v="$version" '
  index($0, "## [" v "]") == 1 { on = 1; next }
  on && /^## \[/ { exit }
  on { print }
' "$changelog_file" >"$notes_file"
if ! grep -q '[^[:space:]]' "$notes_file"; then
  echo "CHANGELOG.md on main has no entry for $version; the release keeps release-please's notes." >&2
  exit 1
fi
# shellcheck disable=SC2016 # the backticks are Markdown, printed as they are
printf '\nInstall: `go install github.com/bottega-cereghino/facet/cmd/facet@v%s`\n' "$version" >>"$notes_file"
gh release edit "v$version" --repo "$REPO_URL" --notes-file "$notes_file" >/dev/null
echo "v$version: release notes set from CHANGELOG.md"
[ "$1" = ship ] || exit 0

tag="v$version"
root=${FACET_INSTALL_ROOT:-$HOME/.local/lib/facet}
git fetch --quiet --tags origin
# The newest tag below the new one, by version order.
previous=$(git tag --list 'v*' --sort=-v:refname | awk -v t="$tag" 'seen { print; exit } $0 == t { seen = 1 }')
if [ -z "$previous" ]; then
  echo "no tag below $tag, so there is no way back. fix: tag the release again once an earlier tag exists" >&2
  exit 1
fi

mkdir -p "$root/$tag" "$root/current"
GOBIN="$root/$tag" go install "github.com/$REPO_URL/cmd/facet@$tag" || {
  echo "could not install $tag. fix: check the network and that the tag exists, then run again" >&2
  exit 1
}
cp "$root/$tag/facet" "$root/current/facet.new"
mv "$root/current/facet.new" "$root/current/facet"
echo "$tag installed in $root/$tag and copied to $root/current"

back="GOBIN=$root/$previous go install github.com/$REPO_URL/cmd/facet@$previous && cp $root/$previous/facet $root/current/facet && ${FACET_SWAP_CMD:-<swap command>} $previous $tag"
if [ -z "${FACET_SWAP_CMD:-}" ]; then
  echo "FACET_SWAP_CMD is not set: the service was not swapped. Run your swap command with: $tag $previous" >&2
else
  "$FACET_SWAP_CMD" "$tag" "$previous" || {
    echo "the swap command failed for $tag. way back: $back" >&2
    exit 1
  }
  running=$("$root/current/facet" version)
  if [ "$running" != "facet $tag" ]; then
    echo "the binary in $root/current says \"$running\", not \"facet $tag\". way back: $back" >&2
    exit 1
  fi
  echo "swapped to $tag; $root/current/facet reports $running"
fi
echo "way back: $back"
