# Decision records

One file per decision, named `NNNN-slug.md`, numbered in the order written.
Each file has three sections:

- **Context**: what forced a choice.
- **Decision**: what was chosen, in one or two sentences.
- **Consequences**: what follows, good and bad.

A decision that is reversed gets a new record. The old one stays, with a line
`Superseded by NNNN`. Design notes for single behaviours live in `docs/design/`;
the reasons behind the house rules live in `docs/CODE.md`.

| Record | Decision |
| --- | --- |
| [0001](0001-ci-on-hosted-runners.md) | CI runs on GitHub-hosted runners |
| [0002](0002-releases-from-a-machine.md) | Releases run from a machine, not a workflow |
| [0003](0003-privacy-denylist-out-of-repo.md) | The privacy guard's word list stays out of the repository |
