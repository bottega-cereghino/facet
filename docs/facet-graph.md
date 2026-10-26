# facet-graph: the issue graph (frozen)

> **Frozen.** `facet-graph` is a separate binary in this repository. It builds and
> its tests run in CI, but it is not actively maintained, and nothing in `facet`
> depends on it. It grew for a multi-agent setup that has since been retired.
>
> ```sh
> go install github.com/RiccardoCereghino/facet/cmd/facet-graph@latest
> ```

## Issue hierarchies

GitHub lets an issue be a sub-issue of another, across repositories. `facet-graph
tree` reads and writes that graph:

```sh
facet-graph tree wire   owner/repo#121 --parent owner/other#72
facet-graph tree show   owner/repo#46
facet-graph tree list   owner/repo#46 --level seat
facet-graph tree status owner/repo#46
facet-graph tree doctor owner/repo#46
facet-graph tree orphans --repo owner/repo [--json]
```

Every one of those but the last reads *downward* from an issue you name, so
none of them can answer **"what is under nothing at all?"** — an issue with no
parent is by definition not below any node you could pass in. `orphans` reads
the complement, one repository at a time.

**It reports a set, not a verdict.** An unparented issue is a perfectly valid
issue, and plenty are deliberately outside a hierarchy — so finding some is
exit 0. **Exit 2 means it could not look** — a repository could not be read, or
none was named — which is the fact worth failing on: silence about a repository
nobody could list would read as "nothing unparented there".

**There is no exit 1 here, and that is the shape rather than an omission.**
`doctor` and `labels` use 1 for *I looked, and here is what is wrong*; this verb
has no such answer, because finding orphans is exit 0. So the only failure left
is not having been able to ask.

```sh
facet-graph tree labels [--repo owner/name] [--create]
```

`labels` is the parity check for the rest of it: `wire` records a node's level
by applying a label, and **a label a repository never defined cannot be
applied** — so without this the edge lands, the level does not, and the command
succeeds. With no `--repo` it sweeps every repository the routing file knows,
because the ones that are short are the ones nobody looks at.

The required set is whatever `structure` declares **for that repository** — a
label declared on a repo-scoped shape is reachable only there — so `--create`
can only ever create a label the configuration already names for the repo it is
writing to.

**None of this is required to use facet, and none of it is assumed anywhere
else.** `facet spawn` never asks whether an issue has a parent; an issue with
no parent is a valid issue rather than a degraded one; and no command here is a
precondition of another — `show` works on a tree facet never built. That is
deliberate. Somebody adopting facet may want workspaces and none of the rest.

So the levels a tree *ought* to have are declared in the routing file, beside
the label rules that are already data:

```json
"structure": {
  "levels": [
    { "name": "programme" },
    { "name": "record", "requiresChildren": true,
      "accepts": [{ "repo": "notes", "titlePattern": "^record: " }] },
    { "name": "bundle", "optional": true },
    { "name": "task" }
  ]
}
```

A level with no `accepts` admits anything. `optional` lets a tree skip a rung —
a bundle of one is just the task, and forcing a placeholder in to satisfy a
schema is worse than allowing the gap — and skipping stops at the first
required rung, which is what catches a task filed straight under the programme
with the record missing.

An accepted shape may be matched **by label** as well as by title:

```json
{ "name": "record", "accepts": [
    { "repo": "notes", "titlePattern": "^record: ", "label": "type/record" },
    { "label": "type/backlog", "childMustBe": "bundle" }
] }
```

With a `titlePattern` beside it the label is an **alternative** — either
satisfies the shape, which is what lets an issue carrying no title convention
still be placed. **With no `titlePattern`, the label is the test**, not merely
what records the match: reading it as decorative would leave the shape
admitting everything, and an unconstrained shape on a skippable rung silently
absorbs the rung below it.

`childMustBe` narrows what may sit under a node that matched **this** shape,
which position alone cannot express because the shapes share a rung. Above, a
record may hold a task directly while a backlog must have its tasks bundled
first. It may only name a rung the children could already occupy — anything
else is refused when the routing file loads, so a narrowing can remove a
candidate and never introduce one.

**Without a `structure` block, `doctor` checks no shape at all.** It still
reports cycles, unreadable nodes, and a closed parent with open children, which
are wrong on any tree's own terms, and it says when shape went unchecked rather
than letting silence read as approval.

`wire` prints both tiers on every edge, and states that the child's own governs
— a parent's complexity is an at-a-glance worst case for the grouping, never
inherited. An edge that quietly changed who may merge something would look
exactly like filing, which is the whole reason it is said out loud.

## Dependencies are a different graph

Blocked-by says what must land first. A parent says what a thing is part of.
Neither substitutes for the other, so they have separate commands:

```sh
facet-graph deps show  owner/repo#75   # both directions
facet-graph deps check owner/repo#75   # declared in the body vs actually wired
facet-graph deps ready owner/repo#46   # what below this could be started now
```

`check` exists because `facet file` creates these edges once, at filing, and
every failure there is a warning rather than a refusal — the issue is already
filed. Nothing looked afterwards. Only one direction is a defect: a blocker
**declared and not wired** means the write failed silently and the dependency
is prose nobody schedules from, while one wired and not mentioned in the body
is ordinary ageing.

