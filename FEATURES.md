# FEATURES

The feature map of facet. States: shipped (on main), in progress (an open PR), planned (an open issue), idea, removed. Reviewed and Verdict are the Sculptor's alone and are left out.

## Workspaces (core)

| ID | Feature | What it is | State | Link |
| --- | --- | --- | --- | --- |
| F01 | Manifest-declared workspaces: `new`, `add`, `rm`, `sync`, `restore` | Lay several clones and links side by side from `.workspace.json`; rebuild anywhere | shipped | [#61](https://github.com/bottega-cereghino/facet/pull/61) |
| F02 | `ls` health, and `--size` with reap verdict | Lists workspaces with health, what deleting frees, and whether reap would delete | shipped | [#218](https://github.com/bottega-cereghino/facet/pull/218) |
| F03 | Clone as a worktree of the `~/Projects` checkout | `sync` makes a clone a worktree of the shared checkout | shipped | [#248](https://github.com/bottega-cereghino/facet/pull/248) |
| F04 | Core stands without GitHub | Workspaces are not gated on the GitHub credential | shipped | [#175](https://github.com/bottega-cereghino/facet/pull/175) |
| F05 | Preflight of the shared GitHub credential | Checks the credential before it is needed | shipped | [#47](https://github.com/bottega-cereghino/facet/pull/47) |
| F06 | Lockfile | Serialises concurrent runs; heartbeat polling | shipped | [#222](https://github.com/bottega-cereghino/facet/pull/222) |
| F07 | Sync hardening: symlink and case races | Symlink swapped before `git clone`, `--prune` case, extra remote, bootstrap | planned | [#299](https://github.com/bottega-cereghino/facet/issues/299) |
| F08 | Mirror temp dir cleanup | A new mirror's `.incoming` cleanup must not delete another mirror | in progress | [#447](https://github.com/bottega-cereghino/facet/pull/447) |
| F09 | `--prune` keeps a link differing in case | Fix for case-only name difference | in progress | [#448](https://github.com/bottega-cereghino/facet/pull/448) |

## Spawn and issues

| ID | Feature | What it is | State | Link |
| --- | --- | --- | --- | --- |
| F10 | `spawn` an ephemeral workspace from an issue | Branch `feature/<n>-<slug>`, CLAUDE.md with the quoted issue | shipped | [#49](https://github.com/bottega-cereghino/facet/pull/49) |
| F11 | Spawn runs nothing by default | `--claude` or `--exec` opt in | shipped | [#180](https://github.com/bottega-cereghino/facet/pull/180) |
| F12 | Spawn quote hazards fixed | VT/FF/RS, title and label quoting | shipped | [#436](https://github.com/bottega-cereghino/facet/pull/436) |
| F13 | Routing: scope, repos involved, fence-aware parsing | Reads the issue body; writeback opt-in; fuzzed | shipped | [#125](https://github.com/bottega-cereghino/facet/pull/125) |
| F14 | `file` and `comment` | Files issues with complexity labels; blocked-by edges become native dependencies | shipped | [#38](https://github.com/bottega-cereghino/facet/pull/38) |
| F15 | `ghx` GitHub wrapper | Errors name their query; refuses when gh could not tell | shipped | [#242](https://github.com/bottega-cereghino/facet/pull/242) |
| F16 | Record the agent session in a workspace | So a lost session can be resumed | planned | [#75](https://github.com/bottega-cereghino/facet/issues/75) |

## Reap and prune

| ID | Feature | What it is | State | Link |
| --- | --- | --- | --- | --- |
| F17 | `reap` refuses to lose work | Holds on unpushed commits, stashes, untracked files, processes, tmux, launchd, submodules, LFS | shipped | [#393](https://github.com/bottega-cereghino/facet/pull/393) |
| F18 | `reap --reapable` and `--dry-run` | Deletes every workspace reap would delete, after one confirmation | shipped | [#227](https://github.com/bottega-cereghino/facet/pull/227) |
| F19 | `reap` for long-lived workspaces and plain worktrees | Same guards; Claude Code worktrees included | shipped | [#249](https://github.com/bottega-cereghino/facet/pull/249) |
| F20 | `prune` frees rebuildable directories | Idle workspaces lose `node_modules`, `.venv` and the like | shipped | [#255](https://github.com/bottega-cereghino/facet/pull/255) |
| F21 | `prune` deletes only what the listing named | Confirmed listing is what goes | shipped | [#445](https://github.com/bottega-cereghino/facet/pull/445) |
| F22 | `reap --closed` | Limit `--reapable` to merged or closed PRs | in progress | [#435](https://github.com/bottega-cereghino/facet/pull/435) |
| F23 | `issues` and `ls --size` offline wording | Say reapable offline and stale fetch where the check did not run in full | in progress | [#449](https://github.com/bottega-cereghino/facet/pull/449) |
| F24 | Landing proof follows symbolic ref chains | Reap sees a branch behind a symbolic ref | planned | [#408](https://github.com/bottega-cereghino/facet/issues/408) |
| F25 | Launchd hold: plist path and case handling | Read the loaded plist; match without resolving last element | planned | [#329](https://github.com/bottega-cereghino/facet/issues/329) |
| F26 | Failing plutil or launchctl holds the workspace | Today it drops the job | planned | [#291](https://github.com/bottega-cereghino/facet/issues/291) |
| F27 | Prune symlink checks on clone and workspace dirs | Check the directories, not only steps below | planned | [#301](https://github.com/bottega-cereghino/facet/issues/301) |

## Queue

| ID | Feature | What it is | State | Link |
| --- | --- | --- | --- | --- |
| F28 | `facet queue`: one agent per queued issue | Runs a configured agent per `next` issue | shipped | [#414](https://github.com/bottega-cereghino/facet/pull/414) |
| F29 | Queue gate and hold | Gate exits 3 and 4 skip a run or a repository; hold names stop files | shipped | [#434](https://github.com/bottega-cereghino/facet/pull/434) |
| F30 | Queue skips issues with an open PR branch |  | shipped | [#425](https://github.com/bottega-cereghino/facet/pull/425) |
| F31 | Queue fixes its own red or conflicting PRs first |  | planned | [#446](https://github.com/bottega-cereghino/facet/issues/446) |
| F44 | A `lane:` label is a preference | No room starts the default agent the same tick and logs `fallback_from`. A `model:` label keeps its agent | in progress | [#473](https://github.com/bottega-cereghino/facet/issues/473) |

## Build, CI and docs

| ID | Feature | What it is | State | Link |
| --- | --- | --- | --- | --- |
| F32 | Release with release-please | Run from this machine, CHANGELOG copied to the release | shipped | [#220](https://github.com/bottega-cereghino/facet/pull/220) |
| F33 | CI: gitleaks, zizmor, CodeQL, shellcheck, Conventional Commit title, test-change check | Gates on every PR | shipped | [#413](https://github.com/bottega-cereghino/facet/pull/413) |
| F34 | Renovate and `.editorconfig` | Dependency updates | shipped | [#199](https://github.com/bottega-cereghino/facet/pull/199) |
| F35 | `AGENTS.md`, `CONTRIBUTING.md`, `docs/CODE.md` | House rules | shipped | [#200](https://github.com/bottega-cereghino/facet/pull/200) |
| F36 | shfmt in `make check` and CI |  | in progress | [#441](https://github.com/bottega-cereghino/facet/pull/441) |
| F37 | Format Go on write via `.claude/settings.json` hook |  | in progress | [#442](https://github.com/bottega-cereghino/facet/pull/442) |
| F38 | Decision records in `docs/decisions/` |  | shipped | [#440](https://github.com/bottega-cereghino/facet/pull/440) |
| F39 | Same hook and shfmt tracked as issues |  | planned | [#439](https://github.com/bottega-cereghino/facet/issues/439) |
| F40 | `FEATURES.md`, this map | Fleet standard features-file | shipped | [#451](https://github.com/bottega-cereghino/facet/pull/451) |

## Removed

| ID | Feature | What it is | State | Link |
| --- | --- | --- | --- | --- |
| F41 | `facet-graph` (`tree`, `deps`) | Issue-graph tools | removed | [#214](https://github.com/bottega-cereghino/facet/pull/214) |
| F42 | Hidden `--seat` flags, seat identity, `facet date` |  | removed | [#215](https://github.com/bottega-cereghino/facet/pull/215) |
| F43 | Windows support | Code paths and CI leg | removed | [#262](https://github.com/bottega-cereghino/facet/pull/262) |
