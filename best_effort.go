package main

import "time"

const bestEffortEligibility = "Eligible if the pool needs fallback: fresh quota after recorded usage and reserves"

// Try without the learned forecast only. The ordinary admission checks still
// account for completed and in-flight work, response headers and billing.
func bestEffortRank(p policy, c candidate, model string, q snapshot, now time.Time, active int) (adaptiveRank, bool) {
	if c.Provider != "claude" {
		return adaptiveRank{}, false
	}
	rank, ok := rankAccountWithForecast(p, c, model, q, now, active, true)
	if ok {
		rank.BestEffort = true
		rank.Reason = bestEffortEligibility
	}
	return rank, ok
}

// Assess the entire model pool before considering forecast-only fallback.
// Callers hold state.Lock when reading the quota cache.
func assessPool(p policy, candidates []candidate, model string, now time.Time, counts map[string]int, demand float64) []rankEvidence {
	rows := make([]rankEvidence, len(candidates))
	anyEligible := false
	for i, c := range candidates {
		q := state.Quota[c.ID]
		q.DemandRate = demand
		rank, ok := rankAccount(p, c, model, q, now, counts[c.ID])
		rows[i] = poolEvidence(p, model, c, rank, ok, counts[c.ID])
		anyEligible = anyEligible || ok
	}
	if !anyEligible {
		for i, c := range candidates {
			q := state.Quota[c.ID]
			q.DemandRate = demand
			if rank, ok := bestEffortRank(p, c, model, q, now, counts[c.ID]); ok {
				rows[i] = poolEvidence(p, model, c, rank, true, counts[c.ID])
			}
		}
	}
	return rows
}

func poolEvidence(p policy, model string, c candidate, rank adaptiveRank, eligible bool, active int) rankEvidence {
	rank.Priority = effectiveCandidatePriority(p, model, c)
	rank.ActiveRequests = active
	return rankEvidence{adaptiveRank: rank, Eligible: eligible}
}
