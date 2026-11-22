```
         /\
        /  \
       / /\ \
      /_/  \_\        f a c e t
      \ \  / /
       \ \/ /         one task, many repositories,
        \  /          one disposable view
         \/
```

# facet

**Task-scoped workspaces over many git repositories.** A workspace is a directory
that assembles several repositories into one view for one task — and because its
whole layout is declared in `.workspace.json`, the directory is regenerable from
the manifest. Nothing about it is precious.

That is the core, and it stands on its own: no GitHub, no issues, no agents — just
a clean way to lay several repositories side by side and rebuild them anywhere. The
issue-driven features further down grew on top of it and became mainstays, but the
workspace is the thing.

```
~/Workspaces/
  delivery/               # a long-lived, topical workspace
    .workspace.json
    platform/             # a clone this workspace owns outright
    infra/
  iss-platform-67-…/      # an ephemeral workspace, one GitHub issue
```

Each entry is either a **clone** the workspace owns outright — its own branch, its
own index, safe from every other workspace — or a **link** into a shared checkout,
where one working tree is visible everywhere at once.

## Why

Working several issues at once — yourself, or with agents in parallel — means
fighting over one working tree: one branch, one dirty index. Giving each task its
own checkout fixes that, and costs disk. `facet` makes the checkouts nearly free,
and makes the throwaway ones disposable without losing work.

## Install

```sh
go install github.com/bottega-cereghino/facet/cmd/facet@latest
```

macOS. Requires `git`. The issue features additionally use the
[`gh`](https://cli.github.com) CLI; `reap` and `prune` also ask `lsof` (part of
macOS) and `tmux`, when it is installed, what is still running in a workspace. `facet` shells out to both, so it inherits your existing credentials, SSH agent
and `gh` accounts, and never handles a token itself.

## The core: workspaces

Everything here works with plain git repositories and needs nothing else.

```sh
facet new delivery --clone platform=git@github.com:acme/platform.git \
                   --clone infra=git@github.com:acme/infra.git
facet sync                 # idempotently rebuild; never touches an existing clone
facet ls                   # what is here, and is it healthy
facet restore              # a fresh machine: rebuild every workspace
```

`new` scaffolds the manifest and its entries; `add` and `rm` adjust them later.
`sync` makes the directory match the manifest and is safe to run at any time — it
creates what is missing and leaves what already exists alone. On a fresh machine,
`restore` walks every workspace and brings them all back from their manifests.
Run `ls` from a directory with no manifest of its own that holds workspace
subdirectories — the workspaces root itself — and it lists every workspace
under it with a rolled-up health summary, instead of erroring.

`facet ls --size` adds what deleting each workspace would free and whether
`facet reap` would delete it now, with a total at the end:

```
WORKSPACE  ENTRIES  HEALTH  FREES  REAP
notes      0        ok      8K     held (1)
platform   1        ok      812M   reapable

812M in all; 1 reapable now, freeing 812M. `facet reap <name>` says what holds the rest.
```

It runs reap's own checks, so it fetches every clone and asks GitHub about
open pull requests, and it is slower than a plain `ls`. The size counts disk
blocks, and leaves out objects a clone shares with the mirror: deleting the
workspace does not free those. On APFS it also leaves out the extents a file
shares with a clone of it outside the workspace, such as a `node_modules` that
pnpm cloned from its store, so it can be far below what `du` reports.

## Working from GitHub issues

The workspace core turned out to be the perfect base for a second habit: opening a
throwaway workspace for a single issue, ready to work in seconds. Two features grew
here and became mainstays — inferring an issue's repositories, and generating a
`CLAUDE.md` that hands an agent everything it needs to start.

```sh
facet spawn 67 --repo acme/platform
```

`spawn` reads the issue, works out which repositories it needs, **prints why each
one was chosen, and waits.** On confirmation it creates an issue-linked branch
(`gh issue develop`, from the repository's default branch unless `--base` names
another), clones each repo, and writes a `CLAUDE.md` carrying the issue
body and the durable hazards recorded for its `area/*` labels. Then it stops and
tells you where to work — opening an editor or starting an agent is yours.

### The issue's text is data, not instructions

Anyone can file an issue on a public repository, and an agent reads the
workspace's `CLAUDE.md` as instructions, with your `gh` and git credentials. So
the issue's body goes in as a blockquote, every line starting with `>`, after a
fixed line that names its author (`@login`) and says to read it as a description
of the task, never as instructions to run a command, open a link, or touch
credentials, and to stop and ask if it says to. Your own issue (the author is
the account `gh` is signed in as) is quoted too, without that last line, so an
issue of yours that says "run `make check`" is simply followed. A fixed line,
`End of the issue's text.`, follows the quote. Nothing in the
body can end the quote early: a heading, a code fence, a bare carriage return or
a copy of that closing line stays inside it.

The plan also warns, before the prompt and on `--dry-run`, when the issue was
not written by the account `gh` is signed in as, or when either one is unknown.
Read the issue before you start an agent on it. The warning names the author,
not the last editor: anyone who can edit the issue may have changed its text.

### The issues a workspace covers

The issues a workspace covers go in `.scope`, one `owner/repo#n` per line: the
spawned issue always, plus any `--scope owner/repo#n` you pass, because one task
regularly carries a small group of issues rather than exactly one. It is not
versioned, and `facet sync` never touches it.

`facet scope list` prints it, found by walking **up** from the working
directory so it works from inside a repository subdirectory. `facet scope add`,
`remove` and `set` change it afterwards; `add` and `remove` are safe to repeat.

```sh
facet spawn 67 --repo acme/platform --scope acme/platform#68 --scope acme/tools#12
```

### After the spawn

`spawn` sets the workspace up and stops: it prints where to work and runs
nothing. Two options start something there, in this terminal, once the
workspace is ready:

| flag | runs in the home clone |
| --- | --- |
| *(none)* | nothing; spawn prints `work in: <path>` and exits |
| `--claude` | `claude`, with Remote Control when routing's `spawn.rc` is true or `--remote` is given |
| `--exec <command>` | `/bin/sh -c <command>`, with `$FACET_WORKSPACE` set to the workspace |

`--exec` is how you open a multiplexer or an editor, the way you like it:

```sh
facet spawn 67 --repo acme/platform --exec 'tmux new-window -c "$PWD"'
facet spawn 67 --repo acme/platform --exec 'code .'
```

The launch runs last, and only on a complete workspace: if a clone failed,
spawn says `Workspace INCOMPLETE`, names what is missing, launches nothing and
exits non-zero. A launch that itself fails only warns, and leaves the ready
workspace for you to open yourself. `--claude` and `--exec` together are refused
before anything is created.

Remote Control makes the session reachable from another device over Anthropic's
relay. It needs `claude` signed in via claude.ai OAuth and the
workspace trusted once. The session is named after the workspace, attached as
`--remote-control=<name>` (never as a positional argument, which a following
prompt would make ambiguous). `spawn.sessionNamePrefix` in routing names the
host.

facet does not open tmux itself. The old `--attach`, `--no-attach`,
`--session`, `--mux`, `--rc` and `--no-rc` are still accepted, hidden and
deprecated: `--rc` means `--claude --remote`, the others do nothing.

### Which repositories, and why

```
acme/platform#67  Rehearse a database restore: nothing has ever been restored
  labels: P0-critical, area/backups, blocked

repos to clone, and why:
  platform    home; label:area/backups          [home, gets the branch]
  infra       blocked-by:acme/infra#41; label:area/backups
```

**Labels cannot decide which repositories an issue needs.** A label describes a
topic, and the same topic label gets used in several repos. The decisive evidence
is in the issue body: `owner/repo#n` cross-references, `Blocked by` lines, and —
for issues filed through a form — an explicit "Repos in scope" field (or
"Repos involved"), listing routing keys, aliases, `owner/repo` or repo URLs. When
that field is filled in it wins over labels; an `owner/repo` routing does not know
is named in the plan rather than silently dropped. The issue
above is labelled `area/backups` with no Terraform label, and still cannot be
closed without a change in another repository. So the inference is always shown,
never silently trusted, and correctable with `--clone` / `--add` / `--rm`.
Each of those takes a routing key or `key=url`, repeatably, and **refuses a
value it cannot resolve** rather than skipping it — a discarded override and an
applied one must not look the same. `--dry-run` prints it and creates nothing.

### Moving the issue on a project board

A GitHub issue has no "in progress" state — it is open or closed. "In progress" is
an option on the **Status** field of a Projects v2 board, and it belongs to the
board *item*, not to the issue. So give `.tools/routing.json` a board to drive, and
`facet spawn` puts the issue on it and sets the field once the workspace is real:

```json
"project": { "owner": "acme", "number": 4, "statusField": "Status", "onSpawn": "In progress" }
```

The board is named, never by node ID: `PVTSSF_lADOD…` is stable but unreadable, and
would rot in a config file without anyone noticing. `facet` resolves the names on
each spawn, matching case-insensitively, and reports the transition:

```
+ project acme/4: Status = In progress
```

Both fields are optional and both are shown by `--dry-run` before anything happens.
Omit `project` and no board is touched. A board that has been renamed, or a `gh`
missing the `project` scope, **warns and does not fail the spawn** — the clones,
the branch and the `CLAUDE.md` are the point, and a complete workspace is never
stranded by GitHub Projects being briefly uncooperative.

### The confirmed repo set can be written back

`facet spawn` prints its inference and waits for you. That answer is worth keeping:
with `--writeback`, on confirmation it records the confirmed repos in the issue's
**Repos in scope** section, so the next spawn reads a decision (`scope-field`) instead of repeating a
guess — and an issue never filed through a form finally declares what it touches.

```
+ issue body: Repos in scope = platform, infra
```

Rewriting someone's issue body is unforgiving, so the rewrite is timid: the
neighbouring sections come back byte for byte, an existing heading keeps the level
its author chose, an empty set writes nothing, and a body that already says the
right thing is left alone — spawning twice does not churn the issue's history. The
body is re-read immediately before the write, because the copy fetched at the top
of `spawn` may be minutes old by then, and someone may have edited it since.
Without `--writeback`, spawn writes nothing to the issue.

### Filing an issue that the board can see

```sh
facet file --repo acme/platform \
  --title "gateway: last_login_at is never written" \
  --label P1-high --label area/security --label complexity/2 --label env/dev \
  --repos platform,gateway --body-file issue.md
```

`facet file` searches for a duplicate before it creates one — concurrent sessions
file into the same repository, and closed issues count, because refiling something
you decided against is the expensive kind of duplicate. Then it checks the title and
the labels against the `conventions` block, reporting **every** violation at once,
so a single filing tells you everything it needs rather than one rule at a time:

```json
"conventions": {
  "titlePattern": "^[^:\\n]{2,60}: .+",
  "requireOneOf": {
    "priority":   ["P0-critical", "P1-high", "P2-medium", "P3-low"],
    "complexity": ["complexity/1", "complexity/2", "complexity/3"]
  },
  "requirePrefix": { "area": "area/" }
}
```

facet knows that *some* labels are required, never which ones. Omit the block and
nothing is enforced. `--repos` is recorded in the body, so the first spawn of that
issue is exact.

A `### Complexity` section in the body, starting with a number (`2`, `c2` or
`complexity/2`), adds the `complexity/N` label, so the body and the labels agree
and the conventions see it. A `--label complexity/M` that disagrees is refused.
A repository that does not define the label gets the issue without it, and a
warning. `file` prints the labels it applied after the URL.

### Tidying up

`facet issues` lists the ephemeral workspaces. `facet reap <name>` deletes a
workspace, ephemeral or long-lived, and **refuses** while there are unpushed
commits, uncommitted changes, stashes, an open pull request, or a tmux pane or
process still in the workspace — the states where
deleting would lose work, or delete a directory out from under something still
running in it. A refusal lists every holder it can see, with a count, so
clearing what it names and re-running should not turn up more.

A long-lived workspace can also hold files of its own beside its clones: notes,
a `CLAUDE.md`. Any that are not committed in an enclosing repository (a
versioned `~/Workspaces`, say) hold it as well, because the workspace is their
only copy. A workspace that is itself a repository holds too, whatever is
committed or pushed there: its `.git` goes with it. An issue workspace holds
the same way, except for the `CLAUDE.md`
spawn wrote, as long as it is unchanged. Links are removed, never followed, so
their targets are untouched.

`facet reap --reapable` deletes every workspace at the root that `reap` would
delete now. It lists them with the disk each frees and the total, asks once,
and leaves every held workspace alone. The held ones are listed too, one line
each, grouped by what holds them (unpushed commits, uncommitted changes,
ignored files nothing rebuilds, with the first of those files named);
`facet reap <name>` says all that holds one. It never forces, and it checks each
workspace again just before deleting it, so work started while the prompt
waited is not lost. It never guesses either: a workspace with a remote it could
not fetch is left alone and named, since a branch deleted upstream would still
read as pushed; `facet reap <name>` takes it with a note saying so. It fetches
each repository once, however many worktree clones share it. `--dry-run`
prints the list, worktrees included, and stops there; `facet ls --size` shows
the workspaces without deleting.

`reap` also takes a plain git worktree: one made with `git worktree add`, by
hand or by an agent (Claude Code keeps its own under `.claude/worktrees/`).
`facet reap <path>` refuses while it has uncommitted or untracked files,
commits only its HEAD reaches, an open pull request for its branch, or a
process, tmux pane or launchd job inside it, then removes it with
`git worktree remove`. Its branches stay in the checkout. `--reapable` adds
every reapable worktree inside a checkout directly under `~/Projects`, such as
`.claude/worktrees/`, named by its path. It leaves out a worktree outside its
checkout, which may be a long-lived copy (a service run from a tag, with its
data beside the code), and every worktree of a bare repository, where the
everyday checkout is a worktree too; `facet reap <path>` still takes either.
The worktree clones of a workspace go only with their workspace, and `reap`
refuses one named on its own.

`facet prune` frees the disk a workspace keeps without deleting it: the
directories in its clones that one command rebuilds (`node_modules`, `.venv`
and the tool caches). It lists what each workspace would free and deletes only
with `--apply`, after one confirmation (`-y` skips it), and only from a
workspace no clone of which was used within `--idle-days` (14 by default) and
that nothing runs from. Use is the newest of a clone's git activity and every
entry inside the listed directories, so a `pip install` into
`.venv/lib/…/site-packages` or an edit deep in `node_modules` counts
(`TestPruneSeesAnInstallOrEditInsideTheDirectory`). A directory that frees under 1 MiB, such as a
`node_modules` pnpm cloned from its store, is not named: deleting it would only
cost a reinstall. A directory is named only when the clone tracks a file
that rebuilds it in the same directory, the one its tool reads when it
writes it: `package.json` for `node_modules`, `.next`, `.turbo` and
`.parcel-cache`; `pyproject.toml`, `setup.py`, `setup.cfg`, `Pipfile` or a
`requirements*.txt` for `.venv` and the Python caches, which also take their
tool's own config (`mypy.ini`, `pytest.ini` or `conftest.py`, `ruff.toml`). A
file further up does not count: the root's `pyproject.toml` does not install
a `.venv` in `api/`. For `node_modules` and `.venv`, which people also install
into by hand, the file must declare a package as well, in the copy staged in
the index and on disk alike: a `package.json` lists a dependency or a
workspace, or has a `pnpm-workspace.yaml` beside it; a `pyproject.toml` has a
`[project]` or `[tool.poetry]` table, or a `uv.lock` beside it; a `setup.cfg`
has `[metadata]` or `[options]` and a `setup.py`, or a `pyproject.toml` that
leaves the build to setuptools, beside it; a requirements file names a
requirement. A `setup.py` or a `Pipfile` counts as it is. A tool's settings
alone (`[tool.ruff]`, `[flake8]`) still count for that tool's cache. Without
such a file the directory is kept, and the listing says so on a `kept:` line.
`.terraform` is never named: it keeps the selected `terraform workspace`, and
a fresh `init` would quietly plan against `default`.

## The issue graph was removed

`facet-graph` (`tree` and `deps`, for sub-issue hierarchies and blocked-by
dependencies) was dropped after v0.2.0; `go install …/cmd/facet-graph@v0.2.0`
still builds the last version. `facet file` still records `Blocked by` edges
when it files an issue.

## Comments, by kind

```sh
facet comment last owner/repo#282 --kind plan
```

Where a decision is revised by posting it again, the newest one is what binds,
and finding it by eye in a long thread is how the wrong revision gets acted on.
A kind is a named regexp in the routing file's `commentKinds`; `--grep` needs
no configuration. Anchor the pattern to the whole heading rather than to a
word — `^#{1,6} +Plan\b`, not `^#+ .*plan` — because a loose one does not
error, it silently returns an older comment as the latest. The match count is
always printed so that is visible.

`facet comment edit owner/repo#n --id <id> --body-file <path>` edits a comment
only when it is on that issue. A comment id names a comment anywhere in the
repository, so an id copied from the wrong thread is refused, with the issue it
is really on.

## The credential preflight

Everything facet does on the forge rides one `gh` credential, shared with
every other tool and agent session on the machine. That credential has been
invalidated before with no announcement, and the fault was found mid-operation
rather than up front.

`facet preflight` checks the credential surface before it is needed:

```
$ facet preflight
host        github.com
account     RiccardoCereghino
token type  ghp_ (value never read)
scopes      read:org, repo, workflow
protocol    ssh
source      ~/.config/gh/hosts.yml

✓ credential preflight passed
```

It checks that gh is logged in and the account is active (and, with
`FACET_GH_ACCOUNT` set, the expected one);
that the token is **not** a `gho_` OAuth-App token — GitHub issues one per (app,
user) pair, so any other `gh auth login` anywhere silently invalidates it, which
is what happened; that `repo`, `read:org` and `workflow` are all granted (a fine-grained
`github_pat_` token has permissions rather than scopes, which gh cannot list, so
for one the scopes check is reported as not run); that
git's **host-level** protocol is `ssh` (`gh auth login --with-token` flips it to
https silently, and `gh config get git_protocol` reports the *global* default, so
it is the wrong thing to read); and that `~/.ssh/id_ed25519` exists and is not
group- or world-readable, since that key authenticates every push.

`facet spawn` runs the same checks first, before routing and before the issue
lookup, and **warns** about each problem rather than refusing. It leaves out the
token-type rule: a default `gho_` token breaks nothing about a spawn, so only
`facet preflight` reports it. A credential that cannot
reach GitHub at all fails at the issue lookup, before anything is created. The
workspace core (`new`, `add`, `sync`, `restore`) never consults the credential.

The account check is opt-in: set `FACET_GH_ACCOUNT` to the login the credential
must belong to.

Two properties are deliberate. It reads `gh auth status` and nothing else,
because gh reports a credential as invalid *without* a valid credential: the
check does not go blind during the fault it exists to catch. And it never reads,
prints or stores a token value — only the type prefix — and never calls
`gh auth login`, `logout` or `refresh`, because a repair path that can leave the
machine with no credential is the same class of fault as the outage.

Both are also true of the failure path, and `preflight` works with no workspaces
root and a broken config: the thing you reach for when the lab is wrong cannot
require the lab to be right.

## A checkout you already have becomes a worktree

When `~/Projects` already holds a checkout of a repository a workspace
declares, `sync`, `restore` and `spawn` add a git worktree of that checkout
instead of cloning again. The workspace then costs its working tree and
nothing more: no second `.git`, and no mirror to keep in step.

- The checkout is `~/Projects/<name>` or `~/Projects/<repository>`, and is
  used only when its `origin` is the clone's URL. A directory of that name
  tracking another repository is left alone, and the clone is made as below.
- facet fetches `origin` there and adds the worktree on a detached HEAD at the
  default branch; `spawn` then puts the home clone on the issue branch. The
  checkout's own branch, index and files are never touched, so it stays on
  `main`. A local branch of the issue's name that holds commits origin lacks
  makes `spawn` refuse rather than reset it.
- A worktree shares the checkout's branches, stash, remotes and hooks. A
  branch made in the workspace is a branch of the checkout, and stays there
  after `reap`.
- `--no-worktree` clones every repository, as before.

## Mirrors make the clones cheap

A repository with no checkout under `~/Projects` is cloned.
`facet sync --via-mirror`, and every `facet spawn`, clones from a bare mirror under
`~/Projects/.mirrors/` rather than from the forge. Git hardlinks `.git/objects`
when cloning from a local path, so a second workspace over the same repository
costs its working tree and **zero bytes of objects**. Each clone keeps an
independent `.git`, and `origin` is repointed at the forge, so pushes and fetches
reach GitHub.

Hardlinks rather than `--shared`/alternates: an inode outlives the mirror's
directory entry, so repacking or garbage-collecting either side is safe. And
correctness never depends on a mirror being fresh — a failed mirror fetch is a
warning, because every clone's origin is the forge.

## Design

**`facet` knows nothing about your organisation.** Which repositories a label
implies and what hazards an area carries are *data*, read from your workspaces
root:

| File | What it holds |
| --- | --- |
| `.tools/routing.json` | the repo table, the label → repos prior, and the project board |
| `.knowledge/area-*.md` | durable hazards, inlined into a spawned workspace |

A knowledge fragment holds **invariants only** — things true about a system
whichever issue you happen to be working on. Status, phase and "as of" notes belong
in the long-lived workspace named by the fragment's `source_workspace`. Keeping the
two apart is the only thing that stops a fragment quietly becoming a second, staler
source of truth. The loader rejects a `kind:` other than `invariants`.

**`facet` shells out to `git` and `gh`** rather than using a pure-Go git library.
It needs Git-LFS, credential helpers, SSH-agent auth and — decisively — the
`--local` hardlink clone, none of which `go-git` provides. And `gh` already holds
working, multi-account authentication.

**The manifest format is frozen.** `facet` reproduces one byte for byte, inserting
only the empty schema keys a file predates. It never reformats or reorders one, so
it can be adopted by an existing, versioned set of workspaces without churn.

## Guarantees, and the tests that hold them

- **An existing clone is never touched** by `sync` — no pull, no reset, no clean.
  It may hold the only copy of unpushed work.
- **`sync` writes only inside the workspace.** A clone or link key must be one
  directory name and a link target a folder under the projects root; a manifest
  naming `../x`, `a/b` or `/abs` is refused when it is read, before anything is
  cloned or linked (`TestValidate`, `TestReadRefusesAnEscapingKey`). The one
  write outside it is git's own, in a checkout a clone becomes a worktree of:
  a fetch of `origin`, and the worktree's entry in that checkout's `.git`.
- **A manifest URL is never read as a git option.** Every git call that takes a
  URL, path or remote name from the manifest puts `--` before it, so a URL such
  as `--upload-pack=<command>` is looked up as a repository and fails
  (`TestCloneTakesAURLStartingWithDashesAsARepository`).
- **`--prune` deletes only links**, never a clone: each entry is confirmed to be
  a symlink before it is removed, so a plain directory is never mistaken for one.
- **`reap` counts commits reachable from any local ref and from no remote.**
  Every ref is local except `refs/remotes`, `refs/tags` and `refs/notes`: HEAD,
  every branch, `refs/replace`, and any namespace a tool writes, so a list of
  names cannot miss the next one (`TestReapHoldsACommitOnlyALocalRefReaches`).
  A local ref that points at a tree or a blob holds, since nothing tells
  whether a remote has it (`TestReapHoldsALocalRefThatIsNotACommit`); the stash
  holds as a stash (`TestReapDoesNotCountRefsOutsideLocalWork`), even once its
  reflog is expired and `git stash list` prints nothing
  (`TestReapHoldsAStashWhoseReflogWasExpired`). Unlike
  `@{u}..HEAD`, that also catches a branch which was never
  pushed at all — the branch most easily lost. Only the refs of a configured
  remote count, refreshed by the fetch below, so a ref left under
  `refs/remotes` by a remote removed by hand proves nothing
  (`TestReapHoldsWorkOnlyAStaleRemoteRefHas`); and only a remote whose
  repository is outside the workspace, so the clone itself, a bare backup in
  it or a sibling clone is no copy
  (`TestReapHoldsWorkWhoseOnlyRemoteIsInsideTheWorkspace`). The repository is
  resolved as git reads it: a directory outside whose `.git` is a gitfile or a
  symlink into the clone is the clone
  (`TestReapHoldsWorkWhoseRemoteIsAGitfileIntoTheClone`), and a backup that
  borrows the clone's objects through its alternates (`git clone --shared`)
  holds none of them
  (`TestReapHoldsWorkPushedToABackupThatBorrowsTheClonesObjects`). In a worktree
  clone, whose branches are the checkout's and stay with it, HEAD and the
  worktree's own refs count (`refs/worktree/*`, `refs/bisect/*`, and
  `refs/rewritten/*`, where an interrupted `rebase -r` keeps its labels),
  since they go with it
  (`TestReapHoldsACommitOnlyAPerWorktreeRefReaches`,
  `TestReapHoldsACommitOnlyARebaseLabelReaches`). It fetches every remote with
  `--prune` first, not only `origin`, so a branch deleted from a fork since it
  was pushed there no longer reads as pushed; a remote it cannot fetch is named
  (`TestReapFetchesEveryRemoteBeforeCountingWorkAsPushed`,
  `TestReapNamesTheRemoteItCouldNotFetch`). It fetches into
  `refs/remotes/<name>/` only, whatever the remote's own refspec says, so a
  remote added with `--mirror=fetch` cannot prune local branches away, not even
  in a dry run (`TestReapsFetchNeverWritesOutsideRefsRemotes`).
- **`reap --reapable` leaves alone a workspace it could not fetch for**, and
  names it: "nothing unpushed" was judged against refs on disk
  (`TestReapReapableHoldsAWorkspaceJudgedAgainstAFailedFetch`). It fetches
  each repository once, so worktree clones of one checkout do not fetch into
  the same refs at once and collide (`TestSharedFetchFetchesEachRepositoryOnce`).
- **`reap` judges every repository a clone holds as a clone of its own**,
  recursively: a submodule's commits, branches and stash live in the clone's
  git directory and go with it, while the superproject sees only the commit it
  records (`TestReapHoldsAnUnpushedSubmoduleCommit`). That includes a
  submodule that is not checked out, switched to a commit without it or
  deinitialised, whose repository stays in the clone's git directory
  (`TestReapHoldsASubmoduleThatIsNotCheckedOut`); a repository added with
  `git add` and no `.gitmodules`
  (`TestReapHoldsAnEmbeddedRepositoryWithoutGitmodules`); and one made inside
  a directory the clone tracks files in
  (`TestReapHoldsARepositoryInATrackedDirectory`).
- **`reap` sees every untracked and modified file whatever git's config says.**
  Its status check names `--untracked-files=all` and `--ignore-submodules=none`,
  so `status.showUntrackedFiles=no` cannot hide a new file from it
  (`TestReapHoldsAnUntrackedFileTheConfigHides`), and turns off
  `core.fsmonitor`, whose hook can miss an edit
  (`TestReapSeesAnEditTheFSMonitorMisses`). A tracked file marked
  `assume-unchanged` or `skip-worktree`, or read through a clean filter, is
  hashed as it is on disk and compared with what is committed, in a clone and
  in the enclosing repository alike, since git status skips the first and sees
  the second only through the filter (`TestReapSeesAnEditBehindAnIndexBit`,
  `TestReapHoldsAnEnclosingEditBehindAnIndexBit`,
  `TestReapSeesContentACleanFilterHides`). A Git LFS file matches when the
  committed pointer names its content
  (`TestReapJudgesAnLFSFileByThePointerItsBlobHolds`); any other filter that
  changes what it stores, such as git-crypt's, holds the file. A clone whose
  `core.worktree` points elsewhere holds, since git would read that directory
  instead of the one reap deletes
  (`TestReapSeesFilesWhenCoreWorktreePointsElsewhere`).
- **`reap` reads the repository on disk, not what the environment tells git.**
  Every git call drops `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the
  other variables that point git at another repository, index or config (git
  sets `GIT_DIR` for a `!facet reap` alias), ignores replace refs, which are
  local and never pushed, and reads every path as a file name, never as
  pathspec magic (`TestReapIgnoresAnInheritedGitDir`,
  `TestReapIgnoresAReplaceRefThatGraftsALocalCommitOntoTheRemote`,
  `TestReapHoldsAFileWhoseNameIsPathspecMagic`). A repository whose git
  directory has `info/grafts`, the older local rewrite that nothing turns off,
  holds (`TestReapHoldsAGraftFile`), and so does one whose alternates lead into
  the workspace, since the objects it borrows go with it
  (`TestReapHoldsACloneThatBorrowsObjectsFromTheWorkspace`). The landing proof names the
  default branch by its full ref, so a local branch called `origin/main` is not
  taken for it (`TestReapHoldsALocalBranchNamedOriginMain`).
- **`reap` refuses when it cannot tell whether a pull request is open.** Only
  gh's "no pull requests found" counts as none; a repository gh cannot resolve
  (renamed, transferred, unreadable) holds the workspace
  (`TestViewPRRefusesWhenTheRepositoryCannotBeResolved`).
- **`reap` refuses while a clone has a linked worktree** (`git worktree add`):
  its files and detached commits are invisible to the checks above, and deleting
  the clone deletes its repository (`TestLinkedWorktreeBlocksReap`).
  A clone or plain worktree that is itself a worktree is held by any worktree
  inside its directory, such as Claude Code's `.claude/worktrees`, which goes
  with it (`TestWorktreeCloneIsHeldByAWorktreeNestedInIt`,
  `TestPlainWorktreeIsHeldByAWorktreeNestedInIt`).
- **`reap` refuses while anything beside the clones exists nowhere else**, in an
  issue workspace as in a long-lived one: a checkout `facet rm` dropped from the
  manifest, a hand clone, a note at the root. It deletes only what it can
  account for: every entry at the top level is a declared clone, a declared
  link, facet's own file, the `CLAUDE.md` spawn wrote, or a directory whose
  every file an enclosing repository (`~/Workspaces` usually is one) has
  committed at that exact path with the same bytes
  (`TestIssueWorkspaceHoldsFilesBesideItsClones`,
  `TestReapAccountsOnlyForCommittedBytes`). Committed means in its HEAD: a file
  that repository ignores, even when it ignores the whole workspace, or has
  only staged, holds
  (`TestReapHoldsANoteInAWorkspaceTheEnclosingRepoIgnores`). A name matches
  byte for byte, never across case or Unicode forms, so a note renamed in case
  only on macOS's disk, or whose name the disk keeps decomposed, holds
  (`TestReapHoldsAnEditedNoteRenamedInCaseOnly`,
  `TestReapHoldsANoteWhoseNameIsDecomposed`); a symlink the manifest does not
  declare holds (`TestReapHoldsAnUndeclaredSymlink`). Only a repository above
  the workspace counts: a workspace that is itself a repository (someone ran
  `git init` in it) keeps its history in the `.git` that reap deletes, so it
  holds, pushed or not, and so does a `.git` git cannot read
  (`TestReapHoldsAWorkspaceThatIsItsOwnRepository`,
  `TestReapHoldsARepositoryAtTheWorkspaceRoot`). A repository beside the
  clones, at any depth (git or another VCS, bare included), holds whatever the
  enclosing repository records of it: `git add -A` keeps a hand clone as a
  gitlink, its HEAD's sha and none of its commits
  (`TestReapHoldsAHandCloneTheEnclosingRepoHasAsAGitlink`,
  `TestReapHoldsAGitlinkedHandCloneThatMovedOn`,
  `TestReapHoldsAGitlinkedHandCloneOneLevelDown`,
  `TestReapHoldsARepositoryDeepInALooseDirectory`). The `CLAUDE.md` spawn
  writes is facet's own only while it hashes to what spawn recorded in the
  manifest; edited, or spawned before the hash was recorded, it holds: nothing
  rewrites it, so an agent's notes there are the only copy
  (`TestIssueWorkspaceHoldsAnEditedClaudeMD`). Only a symlink counts as a
  link: a real directory at a name the manifest declares a link holds too
  (`TestReapHoldsARealDirectoryAtALinkName`), and `facet add link` refuses a
  name a directory already holds, before it writes the manifest
  (`TestAddLinkRefusesARealDirectoryAtItsName`).
- **`reap` refuses while a repository it does not judge sits anywhere in a
  clone's work tree**, such as a fork cloned into an ignored `vendor/` to
  patch: its commits are invisible to the checks above, and it goes with the
  clone (`TestReapHoldsARepositoryInAnIgnoredDirectory`). One walk reads the
  whole tree, ignored directories included, never following a symlink; only a
  git repository at a gitlink or in a tracked directory is judged, as a clone
  (above). Any VCS counts, and so does a bare repository, which has no `.git`
  (a backup the clone pushes to there would make its commits read as pushed)
  (`TestReapHoldsAnyRepositoryInAnIgnoredDirectory`), an ignored directory
  that is itself the marker, such as a hidden `.hg/` or Jujutsu colocated at
  the clone's root (`TestReapHoldsARepositoryWhoseMarkerIsTheIgnoredDirectory`),
  and another VCS beside a judged repository's `.git`
  (`TestReapHoldsAnotherVCSBesideASubmodulesGit`). A marker git tracks, such as
  a test fixture, is committed content and does not hold. A submodule's
  directory that is not checked out holds when it is not empty: git status
  never looks there, so its files are in no repository
  (`TestReapHoldsFilesInASubmoduleDirectoryThatIsNotInitialised`,
  `TestReapDoesNotHoldOnAnEmptySubmoduleOrATrackedMarker`).
- **`reap` refuses while a clone holds a file it ignores that nothing
  rebuilds**, such as a `.env` or a local config: the clone is its only copy
  (`TestReapHoldsAnIgnoredFileNothingRebuilds`). Dependency folders and build
  output do not hold: prune's rebuildable list (`node_modules`, `.venv`, the
  Python tool caches, `.next`, `.turbo`, `.parcel-cache`), plus `target`,
  `dist`, `__pycache__` and `.DS_Store`, at any depth.
- **What `reap` does not protect:** what those rebuildable directories hold;
  commits reachable only from a local tag (a clone carries every upstream tag,
  so a tag cannot tell your work from a release); and commits reachable only
  from the reflog (amended, rebased or reset away) or from `refs/notes`, since
  every rebase leaves the first and no fetch brings back the second, so either
  would hold every workspace. Copy or push those first.
- **`prune` deletes only directories git ignores**, named in a fixed list of
  rebuildable ones, holding no tracked file and outside any nested repository
  or worktree; an ignored `.secrets/` or an unignored `node_modules` stays
  (`TestPruneDeletesWhatWasFoundAndNothingElse`). One with a repository
  anywhere inside it, such as an editable pip install in a `.venv`, stays too,
  whether git, Mercurial, Jujutsu, Subversion, Sapling, Bazaar, Darcs, Pijul,
  CVS, RCS, SCCS or Fossil, a bare git repository or a Fossil repository file
  (`TestInspectPruneKeepsADirectoryHoldingARepository`,
  `TestPruneKeepsADirectoryHoldingANonGitRepository`), and so does one with
  no file that rebuilds it tracked beside it, such as a hand-built `.venv` with
  no `pyproject.toml`: an untracked one, or one further up, does not count
  (`TestPruneKeepsADirectoryNothingTrackedBesideItRebuilds`,
  `TestPruneKeepsWhatNothingTrackedRebuildsAndSaysWhy`). Beside a `.venv` or
  a `node_modules`, a file that declares no package to install does not count
  either, such as a `pyproject.toml` holding only ruff's settings or a
  `package.json` holding only `"type": "module"`, nor one changed on disk
  since it was staged (`TestPruneKeepsADirectoryBesideAFileThatInstallsNothing`,
  `TestPruneDeletesADirectoryBesideAFileThatInstallsIt`).
- **`prune` holds what it cannot check**: a clone whose last use cannot be
  read (looked up in its own git directory, so a worktree clone is dated too),
  or a probe for what runs from it that fails. It checks each workspace again
  just before deleting, and checks that no step of the path is a symlink, so a
  parent swapped for one cannot send it outside the clone
  (`TestPruneChecksAgainBeforeDeleting`, `TestPruneDoesNotFollowASwappedParent`).
- **`reap` deletes a whole workspace or worktree, never a folder inside one.**
  Pointed at a clone or a folder within a workspace, or at a folder inside a
  worktree, it refuses and names what to reap instead; a worktree git will not
  remove is deleted by hand only when its checkout lists it
  (`TestReapRefusesAFolderInsideAWorkspace`,
  `TestInspectWorktreeRefusesAFolderInsideOne`,
  `TestRemoveWorktreeLeavesAFolderItDoesNotList`). A manifest a repository
  commits makes nothing a workspace: the enclosing workspace and the worktree
  are checked first, and the top level of a repository is never a workspace,
  so a clone or a worktree whose repository tracks a `.workspace.json`, or a
  folder of such a clone, is refused or judged as what it is
  (`TestReapRefusesACloneWhoseRepositoryCommitsAManifest`,
  `TestReapInspectsAWorktreeWithACommittedManifestAsAWorktree`,
  `TestReapRefusesAFolderOfACloneThatTracksAManifest`). It judges and deletes
  where the path leads, so a clone named through a symlink outside its
  workspace is refused as well, and a worktree named through one is walked in
  full (`TestReapRefusesAWorkspacesCloneNamedThroughASymlink`,
  `TestReapHoldsARepositoryInAWorktreeNamedThroughASymlink`).
- **`reap` checks again after its prompt**, so work that started while it
  waited holds the workspace, with `--force` too: force covers the holders it
  showed, never new ones (`TestReapChecksAgainAfterThePrompt`,
  `TestReapForceRefusesWhatStartedDuringThePrompt`). A holder's sentence
  ("app has uncommitted changes") reads the same however much work lands
  under it, so with holders shown, any change to the workspace's files during
  the prompt refuses too: a new file in a clone already dirty, an amend
  (`TestReapForceRefusesWorkThatLandsDuringThePrompt`). It refuses, in
  `--reapable` too, when the target's path leads to another real directory
  than before the prompt, or a step of it became or stopped being a symlink,
  so a parent swapped while it waited cannot point the delete elsewhere
  (`TestReapDoesNotFollowASwappedTargetParent`). The launchd jobs are
  read again for that check, so a job loaded while the prompt waited holds too
  (`TestReapRecheckSeesALaunchdJobLoadedDuringThePrompt`), in `--reapable`
  and `prune` alike
  (`TestReapableRecheckSeesALaunchdJobLoadedDuringThePrompt`,
  `TestPruneRecheckSeesALaunchdJobLoadedDuringThePrompt`).
- **`reap --reapable` deletes only what `reap` alone would**, and checks each
  workspace again right before deleting it (`TestReapReapableChecksAgainBeforeDeleting`).
  It refuses `--force`, and with `--dry-run` it deletes nothing
  (`TestReapReapableDryRunDeletesNothing`).
- **`reap --reapable` sweeps only worktrees inside their checkout**, never
  one elsewhere and never a bare repository's, so an everyday checkout that is
  a worktree, or a service run from one, keeps its ignored files
  (`TestProjectWorktreesOffersOnlyWorktreesInsideTheirCheckout`,
  `TestProjectWorktreesSkipsABareLayout`,
  `TestReapReapableSweepsOnlyWorktreesInsideTheirCheckout`).
- **`reap` judges a worktree clone by what removing it loses**: its
  uncommitted files and the commits only its HEAD reaches. The checkout's
  other branches, its stash and its other worktrees stay where they are, so
  they do not hold it. It removes the worktree through git, so the checkout
  forgets it, and never touches the checkout's own files or branches
  (`TestWorktreeCloneIsNotHeldByWhatTheCheckoutKeeps`,
  `TestReapRemovesTheWorktreeAndLeavesTheCheckout`). A clone entry that is a
  symlink is a link: even `--force` unlinks it and leaves the worktree it
  points at (`TestReapForceUnlinksASymlinkedCloneAndKeepsItsTarget`).
- **`reap` never deletes a checkout.** Given a directory with no manifest, it
  acts only when that is a linked worktree, and removes it through git, which
  keeps the repository and every branch (`TestInspectWorktreeRefusesACheckout`,
  `TestReapRemovesAPlainWorktreeAndKeepsItsBranch`).
- **`reap` never touches the mirror.** Deleting a hardlinked object drops that
  name; the mirror keeps its own, with its mode: reap changes no file's mode,
  only a directory's, to empty a read-only one
  (`TestReapLeavesTheMirrorAloneAndDeletesReadOnlyDirs`).
- **`reap` also refuses while a tmux pane or process is in the workspace**, in
  any session — not only the one named after it, which a `link-window`ed pane
  would slip past. A session named after the workspace is its own only while
  one of its panes is rooted there, and `--force` ends only that one, never
  the session facet runs in; a session that only shares the name is left
  alone (`TestReapLeavesAnUnrelatedSessionOfTheSameNameAlone`,
  `TestReapForceEndsTheWorkspacesSession`). A process counts when its working directory, its executable
  or any file it has open is inside the workspace, so a watcher started
  elsewhere that tails a log in it is caught too. Each is named with its pid:
  clear them by pid, never by a pattern kill, which can take out processes
  belonging to other workspaces. A macOS system service (an executable under
  `/System`, outside `/System/Applications`, read from the kernel and never
  from the name a process gives itself) counts only by its working directory:
  Spotlight indexing the tree, or the virtualization service sharing it with a
  VM, would otherwise hold every workspace
  (`TestLsofProcessesUnderJudgesASystemServiceByItsExecutable`). What runs
  inside such a VM is not visible from the host. A git object file with
  another hard link does not count either: lsof matches by inode, so a sibling
  clone's process holding its own pack would read as this workspace's, and
  deleting one name of it breaks nothing
  (`TestLsofProcessesUnderSkipsAHardlinkedObjectHeldElsewhere`). `lsof` sees
  only your own processes.
  tmux missing, or no tmux server, means no panes. `lsof` missing, or failing
  (a warning on stderr, an exit other than 1), holds the workspace, since a
  directory it could not read looks like one nothing runs from
  (`TestLsofProcessesUnderFailsWhenLsofFails`,
  `TestPruneHoldsWhenTheProcessProbeFails`).
- **`reap` refuses while a loaded launchd job runs from the workspace**, running
  or not: a scheduled job between runs holds nothing open, so `lsof` misses it,
  and its next run would fail. A job counts when its `Program`, an absolute path
  in its `ProgramArguments`, or its `WorkingDirectory` is inside the workspace;
  only jobs `launchctl list` shows count, since a plist that is not loaded starts
  nothing. A loaded job's plist is read wherever it is: from the LaunchAgents
  folders, or from the file `launchctl print` says it was loaded from
  (`TestUnderFindsAJobLoadedFromAPlistElsewhere`). Unload the job, or move it out, then reap
  (`TestReapReapableLeavesAWorkspaceALaunchdJobRunsFrom`). A `launchctl list`
  that fails holds every workspace, since no job can be ruled out; only a
  launchctl that is not installed (off macOS) reads as no jobs
  (`TestReapHoldsAWorkspaceWhenLaunchctlListFails`).
- **An issue's body stays quoted in the `CLAUDE.md` spawn writes.** Every line
  of it starts with `>`, whatever it holds, so it cannot pose as the file's own
  headings or rules (`TestIssueClaudeMDKeepsTheBodyInsideTheQuote`).

## Status

A personal tool, used daily on one Mac by its author, and held together by a real
test suite: CI runs it on macOS and Linux on every change. macOS is the platform
it is built for. Windows is not supported, and facet does not build there.

## Licence

[Apache License 2.0](LICENSE). Copyright 2026 Riccardo Cereghino.
