# Carry-over for TECHNICAL-SPEC.md (planned)

Removed verbatim from PRODUCT-SPEC.md in the final address round (§14.14), following the owner ruling on D17 and D27 (§14.13): the detailed ordered rules belong in the technical spec. The product spec keeps only the outcome of each case. Section references below are to PRODUCT-SPEC.md as it stood before that round; some cross-references (for example "FR-6 AC9" suspending reviewer escalation, or owner pushes using the allowance) were changed by the same round's owner rulings, so re-check them against the current product spec before reuse.

## From FR-7 AC6 (D17): merge-failure cause order

- **AC6:** When a condition fails, the loop acts on the first cause in this list that holds, and it never force-pushes. Each run handles one cause. After it, the merge conditions are checked again from the top:
  1. The change has an untrusted commit (FR-13 AC5): the item is escalated.
  2. The head moved after the review (AC1, AC5): the item goes back to `in_review` for a new review. Its kind follows FR-6 AC6, so it is a counted round (FR-6 AC8) unless FR-6 AC9 applies. It is not escalated.
  3. The head doesn't contain the base tip, with or without conflicts (AC4, AC8): the item goes to `addressing` for a base update only (FR-5 AC6), then gets a base-update review (FR-6 AC6). Open manual comments wait for the next check. If `max_base_updates` is used up, the item is escalated instead (FR-6 AC7).
  4. A manual review comment is not cleared (AC9): the item goes to `addressing` to answer it (AC10).
  5. A gate failed, branch protection refuses, or the merge call fails: the item is escalated.
  6. A required gate has no result yet: the item stays in `merging` and is checked again each supervisor interval, within the merge stage's time limit (FR-16 AC1). Reaching the time limit escalates the item.

## From FR-10 AC2 (D27): next-stage rule list

- **AC2:** The owner resumes an item with a single action (a command, or removing the marker), and the item continues from the stage after the last completed one. Whenever an item leaves `blocked`, `escalated`, `paused` or `waiting_limit`, the loop chooses that **next stage** with the first of these rules that holds (§6 `next_stage`):
  1. The item is resumed after an FR-6 AC3 escalation (round limit with a blocking finding): an address run (AC5), even if the head moved since the last review.
  2. The item has no change: implement.
  3. An address run replied to manual review comments and no review has ruled on those replies yet: a review (FR-4 AC8). Its kind follows FR-6 AC6.
  4. The change's head has no review yet: a review. Its kind follows FR-6 AC6.
  5. The latest review left a blocking finding: an address run.
  6. Otherwise: merge, which checks the merge conditions from the top (FR-7 AC6).

## From the §6 Mermaid diagram: numbered next-stage edges and merge-cause comment

```
    %% Merging (deterministic), causes in the order of FR-7 AC6
    %% Next stage after a wait, rules tried in the order of FR-10 AC2
    next_stage --> addressing : 1. resume after a round-limit escalation with blocking findings (FR-10 AC5)
    next_stage --> implementing : 2. no change yet
    next_stage --> in_review : 3. replies to manual comments not ruled on yet (FR-4 AC8)
    next_stage --> in_review : 4. head not reviewed yet
    next_stage --> addressing : 5. review left blocking findings
    next_stage --> merging : 6. clean review on the current head
    next_stage --> waiting_limit : chosen stage is an agent stage, daily limit reached (FR-16 AC3)
    next_stage --> escalated : chosen stage is a counted round, round allowance used up, every manual comment cleared (FR-6 AC8)
```

## From the §6 summary

A failed merge is routed by the first cause that holds, in the order of FR-7 AC6: back to `in_review` (the head moved), to `addressing` (the base moved, or a manual review comment isn't cleared), or to `escalated` (anything else).
Whenever an item leaves `blocked`, `escalated`, `paused` or `waiting_limit`, the supervisor picks the **next stage** by the ordered rules in FR-10 AC2.
