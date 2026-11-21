# Contributing to facet

The house style — layout, errors, boundaries, tests, platform policy, CI, and
the privacy rules this public repository is written to — lives in
**[`docs/CODE.md`](docs/CODE.md)**. Every rule there carries the reason it
exists; if you find one whose reason no longer holds, change the rule rather
than working around it.

Three things worth knowing before you open a pull request:

- **Run `make check`.** It is every gate CI's first tier runs, target for
  target. Green locally is not quite green there: the privacy guard skips
  without a word list (CI has one; a pull request from a fork does not, so
  its `linux` job fails on the guard until a maintainer runs it).
- **This repository is public.** No private repository names and no issue
  references an outside reader cannot follow. Keep the reasoning, drop the
  coordinate — `docs/CODE.md` section 8 has the detail.
- **The pull request title is a Conventional Commit**, because the squash
  merge makes it the commit message on `main`: `type(scope): summary`, the
  scope optional. Types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`,
  `build`, `ci`, `chore`, `revert`. `pr-title.yml` checks it. Older history
  used `area: summary`; that is no longer accepted.
