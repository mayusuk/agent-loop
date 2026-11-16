package core

import "github.com/mayusuk/agent-loop/contract"

// nextStage chooses the next stage when an item leaves a wait or is first
// started (FR-10 AC2, TECHNICAL-SPEC §8.2), then starts it through the
// pre-stage checks. A run refused by the daily limit keeps its attempt and
// crash counts when the same stage is chosen again (work plan D12).
func (d *decision) nextStage(roundLimitResume bool) {
	pending := d.s.Run
	d.s.Run = nil
	pl, rule := d.chooseNext(roundLimitResume)
	if pl.stage == StageMerge {
		d.enterMerging()
		d.rule = rule
		return
	}
	attempt, crashes := 1, 0
	if pending != nil && pending.Stage == pl.stage {
		attempt, crashes = pending.Attempt, pending.Crashes
	}
	d.start(pl, attempt, crashes)
	if d.s.State != StateEscalated {
		d.rule = rule
	}
}

// chooseNext returns the first of the six §8.2 rules that holds.
func (d *decision) chooseNext(roundLimitResume bool) (plan, string) {
	s := &d.s
	switch {
	case roundLimitResume:
		return plan{stage: contract.StageAddress, mode: contract.ModeFindings},
			"TECHNICAL-SPEC §8.2 rule 1: resume after a round-limit escalation with a blocking finding"
	case s.Change == nil:
		return plan{stage: contract.StageImplement}, "TECHNICAL-SPEC §8.2 rule 2: no change"
	case len(s.PendingRulings) > 0:
		// manual_reply, unless a findings run replied too (work plan D5).
		kind := s.NextKind
		if kind == "" {
			kind = contract.KindManualReply
		}
		return plan{stage: contract.StageReview, kind: kind},
			"TECHNICAL-SPEC §8.2 rule 3: manual-comment replies not ruled on"
	case s.LatestReview == nil || s.LatestReview.Commit != s.Change.Head || s.NextKind != "":
		return plan{stage: contract.StageReview, kind: d.reviewKind()},
			"TECHNICAL-SPEC §8.2 rule 4: head not reviewed"
	case len(s.Blocking(d.p)) > 0:
		return plan{stage: contract.StageAddress, mode: contract.ModeFindings},
			"TECHNICAL-SPEC §8.2 rule 5: latest review left a blocking finding"
	}
	return plan{stage: StageMerge}, "TECHNICAL-SPEC §8.2 rule 6: merge"
}

// reviewKind is the kind of a review that no address run or base update
// set: owner_push after an owner push since the latest review, otherwise
// round, and always round for the first review (FR-6 AC6, work plan D4,
// D6, D24).
func (d *decision) reviewKind() contract.ReviewKind {
	switch {
	case d.s.NextKind != "":
		return d.s.NextKind
	case d.s.LatestReview != nil && d.s.OwnerPushed:
		return contract.KindOwnerPush
	}
	return contract.KindRound
}
