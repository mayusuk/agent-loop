# Plans

New milestones for agent-loop are minted into this directory: the minter proposes a plan as a
pull request, and when the owner merges it, materialise creates one issue per task
(PRODUCT-SPEC FR-9, FR-17). The layout is a contract, defined in INTEGRATION-SPEC §7:

```
plans/
  <plan-key>/
    plan.md            free text for the owner: goal, approach, decisions
    tasks.yaml         the task list (the contract)
    tasks/<task>.md    one body file per task, named by tasks.yaml
```

- `<plan-key>` is the directory name, `[a-z0-9-]{1,64}`. `tasks.yaml`'s `plan` must equal it.
- Each task has a `key` (unique in the plan, never reused), a `type` from the profile's `types`
  (`feature` or `bug`), a `title` of at most 200 characters, a `body` path relative to the plan
  directory (a regular file, at most 64 KiB), and `depends_on`: task keys in this plan, or existing
  issues such as `"#17"`.
- The dependencies must not have a cycle.

`plans_dir` in `.agent-loop.toml` points here. [`example/`](example/) is a sample of the layout; it
was added by hand, not minted, so it is never materialised (FR-17 AC1).

## Checking a plan

Check a plan before merging it, including one you edited by hand:

```sh
go run ./cmd/agent-loop plan validate --profile .agent-loop.toml plans/<plan-key>/
```

It lists every problem and exits 1, or exits 0 when the plan is valid. Add
`--project <name>` instead of `--profile` to also check external `depends_on` refs on the
project's tracker. CI runs `scripts/validate-plans.sh`, which checks every `plans/*/` directory.

## Older plans

The plans made before this layout are in [`docs/plans/`](../docs/plans/), and their tasks were
filed as issues by hand. They stay there and are not converted.
