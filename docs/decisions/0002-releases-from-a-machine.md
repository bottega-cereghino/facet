# 0002: Releases run from a machine, not a workflow

## Context

Releases come from release-please (#219). Running it in a workflow does not work:

- The organisation does not let Actions open pull requests, so `GITHUB_TOKEN`
  cannot open the release PR.
- A PR opened with `GITHUB_TOKEN` starts no workflows, so its required checks
  never run.
- No GitHub App with a release key is installed, and adding one means a new
  secret.

## Decision

Run the release-please CLI from a developer machine logged in with `gh`, through
`scripts/release.sh`. The CLI version is pinned by `scripts/release/package-lock.json`.

## Consequences

- The release PR runs CI like any other PR.
- A release needs a person at a machine with `node`, `npm` and `gh`.
- Revisit when the organisation allows Actions to open pull requests.
