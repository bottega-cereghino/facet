# 0001: CI runs on GitHub-hosted runners

## Context

The author's other repositories run CI on self-hosted runners. This repository
is public. A self-hosted runner would run code from strangers' pull requests on
the author's machine. Hosted minutes are free for a public repository.

## Decision

Every job runs on a GitHub-hosted runner. This is an exception to the
self-hosted-only rule of the author's other repositories (AGENTS.md, section CI).

## Consequences

- No stranger's code runs on the author's machine.
- No cost in minutes.
- Do not move a job to a self-hosted runner while the repository is public.
