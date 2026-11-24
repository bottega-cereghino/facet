# facet

facet builds task-scoped workspaces over many git repositories: a directory
that lays several clones and links side by side for one task, declared in
`.workspace.json` and rebuilt from it. On top of that core it spawns a
workspace from a GitHub issue, files and comments on issues, and reaps
workspaces without losing unpushed work. One user, macOS, Go. The README
explains the commands; `docs/CODE.md` holds the house rules with their
reasons. Read the section of CODE.md you are about to touch.

## Layout

| Path | What |
| --- | --- |
| `cmd/facet/` | the CLI: cobra wiring (`newXCmd`) and command bodies (`runX`) |
| `internal/<pkg>/` | one package per job; its package doc says which |
| `internal/privacy/` | the organisation-name guard, a test and nothing else |
| `testdata/` | fixtures; `testdata/manifests/*.json` are byte-exact golden files |
| `docs/CODE.md` | the house style, numbered so a rule can be cited |
| `docs/design/` | design notes for single behaviours (reap's refusal) |
| `docs/decisions/` | decision records, one file per decision (`NNNN-slug.md`) |
| `scripts/release.sh` | cuts a release with release-please (section Releases) |
| `scripts/release/` | `package.json` and `package-lock.json` pinning the release-please the script runs |
| `scripts/test-change.sh` | the `test-change` check: does a PR weaken the tests |
| `scripts/gofmt-on-write.sh` | the PostToolUse hook in `.claude/settings.json`: gofmt a Go file after an agent writes it |

Code moves to `internal/` when a second caller needs it, not before.

## Commands

| Command | What |
| --- | --- |
| `make check` | architecture, fmt, vet, lint, shellcheck, shfmt, test, tidy, vuln: every gate CI's Linux job runs except `make build`. Run it before every push |
| `make test` | `go test ./... -race -shuffle=on -cover`; coverage is printed, never a target |
| `make health` | functions over cognitive complexity 15 and Go files over 400 lines. A budget the review reads, never a gate |
| `make fmt-fix` | `gofmt -s -w .` |
| `make build` | `go build ./...` |
| `go test ./internal/<pkg> -run <Name>` | one test |
| `go test ./internal/routing -run '^$' -fuzz '^FuzzUpsertScope$' -fuzztime 60s` | fuzz one parser longer than `make test`, which runs only the seeds; commit what it finds under `internal/routing/testdata/fuzz` |

golangci-lint and govulncheck are pinned in the Makefile and run with
`go run`. Change a version there and nowhere else. shellcheck is no Go
program: CI installs the release `SHELLCHECK_VERSION` in `ci.yml` names,
checked by its SHA-256, and locally the one on PATH runs
(`brew install shellcheck`). shfmt is the same: CI installs the
`SHFMT_VERSION` release, checked by its SHA-256, and locally the one on PATH
runs (`brew install shfmt`).

## CI

- `ci.yml`: `linux` runs each `make` target as its own step; `macos` runs
  build and test after linux passes, and skips on draft PRs and on PRs not
  based on `main`; `renovate` validates `renovate.json` and only warns; `ci` needs
  `linux` and `macos` and is the one result for this workflow. A new blocking
  job goes into `ci`'s `needs`. Every job's `timeout-minutes` is 5 or less
  (fleet standard ci-time).
- `security.yml`: gitleaks on the PR's commits (full history on main and
  weekly) and zizmor on the workflows. A separate workflow, so its two jobs
  ("Committed secrets", "Workflow audit") are required by the ruleset by
  name rather than through `ci`.
- `pr-title.yml`, rerun when the PR's title or body is edited: `pr-title`,
  the title is a Conventional Commit; `test-change`, a PR that weakens the
  tests (`scripts/test-change.sh` says what counts) has a line
  `Test-change: <why>` in its body.
- `codeql.yml`: CodeQL on the Go code (`security-and-quality`), on PRs and
  main. The job only uploads; code scanning's own "CodeQL" check on the PR
  fails on a new alert of error severity or of high security severity and
  above. It was seen red on a planted command injection (#223). It is not a
  required check, so read it before merging.
- The `main` ruleset requires `linux`, `macos`, `ci`, `pr-title`, `test-change`,
  "Committed secrets" and "Workflow audit", a PR,
  linear history and an up-to-date branch. Squash is the only merge method,
  and the squash commit takes the PR title.
- Jobs run on GitHub-hosted runners, an exception to the self-hosted-only
  rule the author's other repositories follow: hosted minutes are free for a
  public repository, and a self-hosted runner here would run code from
  strangers' pull requests on the author's machine. Keep it that way.
- Actions are pinned to a full SHA with the version in a comment. Checkout
  sets `persist-credentials: false`. Every job has `timeout-minutes`.
- Never add `continue-on-error`, `|| true` or `if: always()` to a gate. The
  one `if: always()` is on `ci`, where it makes a failure count.
- A new gate is seen red once before it is trusted: plant a failure, link
  the red run in the PR, revert the plant.

## Rules a model would not guess

- **This repository is public.** No private repository names, no references
  of the form `name#123` or `owner/repo#123` to other repos, no
  employer-internal terms, in code, comments, commits or PR text. A bare
  `#123` here is fine. Examples use `acme/…`, `owner/repo`, `o/r`.
- **The privacy guard** (`internal/privacy`) scans the tree for a word list
  that is never committed: `.denylist` at the root (gitignored) or the
  `FACET_DENYLIST` environment variable, comma-separated (a repo secret in
  CI, handed only to the two `make test` steps). Locally it skips without one, so a green `make check` does not prove
  it ran; in CI it fails without one. Never commit the list, and never
  weaken the guard to get green.
- **Tests use the real filesystem and real git repositories**, and `mux`
  drives a real tmux server on its own socket. Fakes sit only at the tool
  seams (`gitx.Runner`, `ghx.Client` or a small local interface; CODE.md
  section 4), and `gitxtest.Runner` is the shared git fake. Slow is accepted;
  a test that silently skips its subject is not.
- **Never point a test or a manual run at the real roots.** facet reads
  `FACET_WORKSPACES` (default `~/Workspaces`), `FACET_PROJECTS`
  (`~/Projects`), `FACET_MIRRORS`, `FACET_ROUTING` and `FACET_KNOWLEDGE`.
  Set them to a temp directory when you try a command.
- **`sync` never touches an existing clone, and `reap` refuses while work
  could be lost.** These guarantees are listed in the README section
  "Guarantees, and the tests that hold them". A change that weakens one is a
  behaviour change: say so in the PR, with the reason, and change the tests
  that hold it in the same PR.
- **A refusal prints the fix**, not only the failure (`ghx.Problem` is the
  shape).
- **Keep behaviour changes out of refactors.**
- **Golden fixtures are byte-exact.** `.editorconfig` leaves `testdata/`
  alone and `.gitattributes` keeps git off the line endings of
  `testdata/manifests/*.json`; do not reformat either.

## Pull requests

- One issue, one branch `feature/<issue>-<slug>`, one small PR.
- The title is a Conventional Commit: `type(scope): summary`, the scope
  optional. Types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`,
  `build`, `ci`, `chore`, `revert`.
- `Closes #123` on a line of its own, one issue per line. The keywords are
  parsed greedily, even inside a sentence that denies the closure.
- Docs change in the same PR as the code. The CHANGELOG is not hand-edited:
  release-please writes it from the PR titles, so the title is the line a
  user reads. `feat` lands under Added, `fix` under Fixed, `perf` under
  Changed; a `!` or a `BREAKING CHANGE:` footer is listed first.

## Releases

release-please, run from a machine logged in with `gh`, not from a workflow:
the org does not let Actions open pull requests, and a PR opened with
`GITHUB_TOKEN` would never run its required checks (#219). The script needs
`node` and `npm`: each run installs release-please and its dependencies from
`scripts/release/package-lock.json` with `npm ci --ignore-scripts` into a
temporary directory, so every package the gh token is handed to is pinned by
version and checksum. Never replace that with `npx`, which resolves the tree
afresh.

1. `scripts/release.sh pr` opens or updates the release PR from the commits
   on `main` since the last tag. Below 1.0 a breaking change bumps the minor.
2. Merge it like any PR, once its checks are green. To reword the notes, edit
   `CHANGELOG.md` on the release branch before merging; a new `pr` run
   rewrites the branch.
3. `scripts/release.sh tag` tags the merge commit `vX.Y.Z`, publishes the
   GitHub release, and copies the version's CHANGELOG entry into the release
   notes (release-please would write them from the commit titles instead).
   `go install …/cmd/facet@vX.Y.Z` then works.

The way back from a bad release is the previous tag:
`go install github.com/bottega-cereghino/facet/cmd/facet@<previous tag>`.
Tags up to v0.3.0 declare the module's old name, so for those it is
`go install github.com/RiccardoCereghino/facet/cmd/facet@<tag>`.

## Dependency updates

Renovate (`renovate.json`) on `config:best-practices`, weekly between
00:00 and 06:00 on Monday (Europe/Rome), and only releases at least 14 days old. It covers Go modules (then
`go mod tidy`), action SHAs, the Renovate validator image in `ci.yml`, the Makefile tool pins,
the gitleaks and zizmor versions in `security.yml`, shellcheck's in `ci.yml`, and release-please in
`scripts/release/package.json` with its lockfile (the npm manager). The
Makefile, `security.yml` and shellcheck pins are
found through a `# renovate: datasource=… depName=…` comment on the line
above the pin. Patch updates, and digest updates of Go modules, merge
themselves once every check is green. An action's or image's digest changing
under the same version means its tag was moved, so that waits for a look, as
minors and majors wait for a person. A gitleaks, zizmor or shellcheck update stays red until its
`*_SHA256` is updated in the same PR, because Renovate cannot fetch the
checksum. The `go` directive in `go.mod` and setup-go's `go-version` move by
hand, together. The `toolchain` line in `go.mod` is what every build uses,
locally (Go downloads it) and for `go install`; keep it at the newest patch
`make vuln` is clean with, since an older local Go fails that gate.
