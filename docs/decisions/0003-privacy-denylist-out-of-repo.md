# 0003: The privacy guard's word list stays out of the repository

## Context

This repository is public. `internal/privacy` scans the tree for forbidden
organisation names. Committing the list of forbidden names would be the
disclosure the guard prevents.

## Decision

The list is supplied out of band: the `FACET_DENYLIST` secret in CI, or a
gitignored `.denylist` locally. In CI the guard fails when no list is set.
Never commit the list and never weaken the guard to get green (CODE.md, section 8).

## Consequences

- A local `make check` skips the guard without a list, so green locally does not
  prove it ran.
- CI cannot pass a fork's pull request that has no access to the secret.
