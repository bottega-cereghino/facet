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
| `cmd/facet-graph/` | `tree` and `deps`, a separate binary, **frozen**: it builds and is tested, nothing is added |
| `internal/<pkg>/` | one package per job; its package doc says which |
| `internal/privacy/` | the organisation-name guard, a test and nothing else |
| `testdata/` | fixtures; `testdata/manifests/*.json` are byte-exact golden files |
| `docs/CODE.md` | the house style, numbered so a rule can be cited |
| `docs/design/` | design notes for single behaviours (reap's refusal) |

Code moves to `internal/` when a second caller needs it, not before.

## Commands

| Command | What |
| --- | --- |
| `make check` | every gate CI's Linux job runs: fmt, vet, lint, test, tidy, vuln. Run it before every push |
| `make test` | `go test ./... -race -shuffle=on -cover`; coverage is printed, never a target |
| `make health` | functions over cognitive complexity 15 and Go files over 400 lines. A budget the review reads, never a gate |
| `make fmt-fix` | `gofmt -s -w .` |
| `make build` | `go build ./...` |
| `go test ./internal/<pkg> -run <Name>` | one test |

golangci-lint and govulncheck are pinned in the Makefile and run with
`go run`. Change a version there and nowhere else.

## CI

- `ci.yml`: `linux` runs each `make` target as its own step; `macos` runs
  build and test after linux passes, and skips on draft PRs; `renovate`
  validates `renovate.json`; `ci` needs all of them and is the one result.
  A new job goes into `ci`'s `needs`.
- `security.yml`: gitleaks on the PR's commits (full history on main and
  weekly) and zizmor on the workflows.
- `pr-title.yml`: the PR title is a Conventional Commit.
- The `main` ruleset requires `linux`, `macos`, `ci` and `pr-title`, a PR,
  linear history and an up-to-date branch. Squash is the only merge method,
  and the squash commit takes the PR title.
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
  CI). Locally it skips without one, so a green `make check` does not prove
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
  could be lost.** These guarantees are listed in the README with the tests
  that hold them. A change that weakens one needs that test changed in the
  same PR, with the reason.
- **A refusal prints the fix**, not only the failure (`ghx.Problem` is the
  shape).
- **Keep behaviour changes out of refactors.**
- **Golden fixtures are byte-exact.** `.gitattributes` and `.editorconfig`
  leave `testdata/` alone; do not reformat it.

## Pull requests

- One issue, one branch `feature/<issue>-<slug>`, one small PR.
- The title is a Conventional Commit: `type(scope): summary`, the scope
  optional. Types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`,
  `build`, `ci`, `chore`, `revert`.
- `Closes #123` on a line of its own, one issue per line. The keywords are
  parsed greedily, even inside a sentence that denies the closure.
- Docs change in the same PR as the code.

## Dependency updates

Renovate (`renovate.json`) on `config:best-practices`, weekly on Monday
morning, and only releases at least 14 days old. It covers Go modules (then
`go mod tidy`), action SHAs, the Makefile tool pins and the gitleaks and
zizmor versions in `security.yml`, each found through a
`# renovate: datasource=… depName=…` comment on the line above the pin.
Patch and digest updates merge themselves on a green `ci`; minors and majors
wait for a person. A gitleaks or zizmor update stays red until its
`*_SHA256` is updated in the same PR, because Renovate cannot fetch the
checksum. The `go` directive in `go.mod` and setup-go's `go-version` move by
hand, together.
