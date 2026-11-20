#!/usr/bin/env bash
# Cut a facet release with release-please, from this machine.
#
#   scripts/release.sh pr    open or update the release PR from the commits on main
#   scripts/release.sh tag   after the release PR merged: tag it, publish the release,
#                            and copy the version's CHANGELOG entry into its notes
#
# Why not a workflow: the org does not let GitHub Actions open pull requests, and
# a PR opened with GITHUB_TOKEN starts no workflows, so its required checks would
# never run. Run from here, the PR is opened with the `gh` login, like every other
# PR, and CI runs on it. Issue #219 has the whole reasoning.
#
# The token reaches release-please in a file only this user can read, removed on
# exit, so it never shows on a command line.
set -euo pipefail

# renovate: datasource=npm depName=release-please
RELEASE_PLEASE_VERSION=17.11.2
REPO_URL=bottega-cereghino/facet

case "${1:-}" in
  pr) cmd=release-pr ;;
  tag) cmd=github-release ;;
  *)
    echo "usage: scripts/release.sh pr|tag" >&2
    echo "  pr   open or update the release PR from the commits on main" >&2
    echo "  tag  after the release PR merged: tag it and publish the release" >&2
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
trap 'rm -f "$token_file" "$notes_file"' EXIT
chmod 600 "$token_file"
gh auth token >"$token_file"

# release-please reads --token as a file when it looks like a path.
npx --yes "release-please@${RELEASE_PLEASE_VERSION}" "$cmd" \
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
git show origin/main:CHANGELOG.md |
  awk -v v="$version" '
    index($0, "## [" v "]") == 1 { on = 1; next }
    on && /^## \[/ { exit }
    on { print }
  ' >"$notes_file"
if ! grep -q '[^[:space:]]' "$notes_file"; then
  echo "CHANGELOG.md on main has no entry for $version; the release keeps release-please's notes." >&2
  exit 1
fi
printf '\nInstall: `go install github.com/RiccardoCereghino/facet/cmd/facet@v%s`\n' "$version" >>"$notes_file"
gh release edit "v$version" --repo "$REPO_URL" --notes-file "$notes_file" >/dev/null
echo "v$version: release notes set from CHANGELOG.md"
