package main

import (
	"fmt"
	"math"
	"time"
)

// Ratios are learned only from paired changes within the same two windows.
// Carry them across idle samples/resets, but never learn from reset jumps or
// tiny rounded percentage changes. Five is a configurable initial estimate.
func estimateEquivalents(old, next snapshot) map[string]float64 {
	out := map[string]float64{}
	for key, value := range old.Equivalents {
		if value >= 1 && value <= 30 {
			out[key] = value
		}
	}
	for _, g := range next.Groups {
		var five, priorFive *bucket
		prior := map[string]bucket{}
		for _, pg := range old.Groups {
			if pg.Name == g.Name {
				for _, b := range pg.Buckets {
					prior[b.Window] = b
				}
			}
		}
		for i := range g.Buckets {
			if g.Buckets[i].Window == "five_hour" {
				five = &g.Buckets[i]
				if p, ok := prior["five_hour"]; ok {
					copy := p
					priorFive = &copy
				}
				break
			}
		}
		if five == nil || priorFive == nil || !sameQuotaWindow(five.Reset, priorFive.Reset) {
			continue
		}
		df := priorFive.Remaining - five.Remaining
		if df < .05-1e-9 {
			continue
		}
		for _, b := range g.Buckets {
			if !weeklyWindow(b.Window) {
				continue
			}
			p, ok := prior[b.Window]
			if !ok || !sameQuotaWindow(p.Reset, b.Reset) {
				continue
			}
			dw := p.Remaining - b.Remaining
			if dw < .01-1e-9 {
				continue
			}
			observed := df / dw
			if observed < 1 || observed > 30 {
				continue
			}
			key := g.Name + "|" + b.Window
			if prev, ok := out[key]; ok {
				observed = .7*prev + .3*clamp(observed, prev*.7, prev*1.3)
			}
			out[key] = observed
		}
	}
	return out
}

func quotaRatio(p policy, q snapshot, key string) (float64, string) {
	if v := q.Equivalents[key]; v >= 1 && v <= 30 {
		return v, "learned"
	}
	if p.WeeklyRatio >= 1 && p.WeeklyRatio <= 30 {
		return p.WeeklyRatio, "configured estimate"
	}
	return 5, "initial estimate"
}

// Count five-hour opportunities, including fractional last windows, instead of
// treating a week as unlimited immediately spendable quota. Protected future
// sessions have only 80% nominal capacity. This is a planning approximation,
// not a provider promise about the cost or duration of a request.
func quotaOpportunities(p policy, c candidate, now, end time.Time, available float64, until time.Time, demandRates ...float64) float64 {
	if !until.After(now) {
		return 0
	}
	if until.After(now.Add(7 * 24 * time.Hour)) {
		until = now.Add(7 * 24 * time.Hour)
	}
	if !end.After(now) {
		end = now.Add(5 * time.Hour)
	}
	demand := 1.0
	if len(demandRates) > 0 && demandRates[0] > 0 {
		demand = clamp(demandRates[0], .01, 100)
	}
	currentEnd := end
	if until.Before(currentEnd) {
		currentEnd = until
	}
	total := math.Min(available, demand*math.Max(0, currentEnd.Sub(now).Hours()))
	s := sharedSettings(p, c)
	for start := end; start.Before(until); start = start.Add(5 * time.Hour) {
		capacity := 1.0
		finish := start.Add(5 * time.Hour)
		if s != nil && protectedFiveHourWindow(s, finish, start) {
			capacity -= s.FiveHourFloor
		}
		hours := math.Min(5, until.Sub(start).Hours())
		total += math.Min(capacity, demand*hours)
	}
	return total
}

type deadlineWindow struct {
	key                         string
	b                           bucket
	projected, spendable, ratio float64
	source                      string
}

// Bounded scores compress large pressures. When less than a day remains and
// a budget already exceeds its available opportunities, compare raw pressure
// too so a warm but less urgent conversation cannot strand that budget.
func weeklyDeadlineRescue(best, held adaptiveRank, now time.Time) bool {
	return best.Known && held.Known && best.WeeklyPressure > 1 &&
		best.WeeklyPressure-held.WeeklyPressure > weeklyRescuePressureDelta &&
		best.WeeklyPressure > weeklyRescuePressureRatio*math.Max(.05, held.WeeklyPressure) &&
		best.PacingDeadline.After(now) && best.PacingDeadline.Sub(now) <= weeklyRescueHours*time.Hour &&
		(held.PacingDeadline.IsZero() || best.PacingDeadline.Before(held.PacingDeadline))
}

func rankAccount(p policy, c candidate, model string, q snapshot, now time.Time, active int) (adaptiveRank, bool) {
	if unavailable, _ := c.Metadata["native_unavailable"].(bool); unavailable {
		if retry, ok := c.Metadata["native_retry_after"].(time.Time); ok && retry.After(now) {
			return adaptiveRank{ID: c.ID, Reason: "Provider cooldown until " + retry.Format(time.RFC3339)}, false
		}
	}
	if reason := creditSafetyReason(p, c, q, now); reason != "" {
		return adaptiveRank{ID: c.ID, Remaining: 1, Owned: ownedAccount(p, c), Reason: reason}, false
	}
	return rankIncludedAccount(p, c, model, q, now, active)
}

// Included-quota eligibility without inspecting the paid-use fence. Recovery
// uses this same logic, so it cannot bypass reserves, freshness or buffers.
func rankIncludedAccount(p policy, c candidate, model string, q snapshot, now time.Time, active int) (adaptiveRank, bool) {
	rank := adaptiveRank{ID: c.ID, Remaining: 1, Owned: ownedAccount(p, c), Reason: "Host eligibility; no normalized quota available"}
	age := now.Sub(q.At)
	rank.QuotaAgeSeconds = age.Seconds()
	fresh := !q.At.IsZero() && age >= -quotaResetJitterSeconds*time.Second && age < time.Duration(p.MaxAge)*time.Second
	if (c.Provider == "claude" || c.Provider == "codex") && !fresh {
		rank.Reason = "Quota reading is stale or unavailable"
		return rank, false
	}
	if sharedSettings(p, c) != nil && age > time.Duration(p.Poll+quotaAgeGraceSeconds)*time.Second {
		rank.Reason = "Shared reserve requires a recent quota reading"
		return rank, false
	}
	five := 1.0
	fiveRate := 0.0
	fiveEnd := time.Time{}
	hasFive := false
	weekly := []deadlineWindow{}
	other := []deadlineWindow{}
	seen := map[string]bool{}
	if fresh {
		for _, g := range q.Groups {
			if !relevantGroup(c.Provider, model, g.Name) {
				continue
			}
			for _, b := range g.Buckets {
				if !relevantQuotaBucket(c.Provider, model, b.Window) {
					continue
				}
				if b.Window == "extra_usage" {
					continue
				}
				seen[quotaWindowKey(g.Name, b.Window)] = true
				if b.Remaining <= 0 {
					rank.Reason = "Included " + b.Window + " quota exhausted"
					return rank, false
				}
				rank.Known = true
				rate := q.Rates[g.Name+"|"+b.Window]
				perRequest := fiveRequestFraction
				if weeklyWindow(b.Window) {
					perRequest = weeklyRequestFraction
				}
				projection := projectAdmission(c, q, b, rate, active, now)
				if projection.Reason != "" {
					rank.Reason = projection.Reason
					return rank, false
				}
				b.Reset = projection.Reset
				projected := projection.Projected
				if b.Window == "five_hour" {
					rank.FiveHourReported = b.Remaining
					rank.FiveHourProjected = projected
					rank.FiveHourReset = b.Reset
					rank.CompletedReserve = projection.CompletedReserve
					rank.UnreconciledCompletions = projection.CompletedCount
					rank.QuotaSignal = projection.Signal
				}
				if projected <= 0 {
					rank.Reason = "Projected " + b.Window + " quota exhausted"
					return rank, false
				}
				// With provider credits enabled (or unconfirmed), leave one
				// estimated request unspent even on owned or unlocked accounts.
				// This reduces spillover risk; it is not a billing guarantee.
				if c.Provider == "claude" && !p.RequireCreditsOff && (q.ExtraUsage == nil || q.ExtraUsage.Enabled == nil || *q.ExtraUsage.Enabled) && projected <= math.Max(perRequest, rate/60)+1e-9 {
					rank.Reason = "Included " + b.Window + " next-request safety buffer"
					return rank, false
				}
				floor := reserveForBucket(p, c, g.Name, b, now)
				buffer := math.Max(perRequest, rate/60)
				if floor > 0 {
					if b.Window == "five_hour" {
						buffer = math.Max(buffer, fiveProtectedBuffer)
					} else {
						buffer = math.Max(buffer, weeklyProtectedBuffer)
					}
				}
				if b.Window == "five_hour" {
					rank.FiveHourFloor = floor
				}
				if floor > 0 && projected <= floor+buffer+1e-9 {
					rank.Reason = "Shared " + b.Window + " reserve and next-request buffer"
					return rank, false
				}
				available := projected - floor
				rank.Remaining = math.Min(rank.Remaining, available)
				if b.Window == "five_hour" {
					hasFive = true
					five = math.Min(five, available)
					fiveRate = math.Max(fiveRate, rate)
					if b.Reset.After(now) && (fiveEnd.IsZero() || b.Reset.Before(fiveEnd)) {
						fiveEnd = b.Reset
					}
				} else if weeklyWindow(b.Window) {
					ratio, source := quotaRatio(p, q, g.Name+"|"+b.Window)
					weekly = append(weekly, deadlineWindow{g.Name + "|" + b.Window, b, projected, available, ratio, source})
				} else {
					other = append(other, deadlineWindow{key: g.Name + "|" + b.Window, b: b, projected: projected, spendable: available})
				}
			}
		}
	}
	if sharedSettings(p, c) != nil {
		for key, r := range p.WindowReserves[c.ID] {
			if relevantGroup(c.Provider, model, r.Group) && relevantQuotaBucket(c.Provider, model, r.Window) && !seen[key] && !seen[quotaWindowKey(r.Group, normalizedQuotaWindow(c.Provider, r.Window, ""))] {
				rank.Reason = "Configured quota window has no recent reading"
				return rank, false
			}
		}
	}
	// Some Claude plans report only a weekly window. Require weekly quota and
	// apply the five-hour window only when the provider reports one.
	if c.Provider == "claude" && len(weekly) == 0 {
		rank.Reason = "Claude account requires a weekly quota reading"
		return rank, false
	}
	if !rank.Known {
		return rank, true
	}
	if !hasFive {
		return rankNativeWindows(rank, append(weekly, other...), now, active, age, p.MaxAge), true
	}
	availableNow := five
	demand := q.DemandRate
	if demand <= 0 {
		demand = fiveRate
	}
	if demand <= 0 {
		demand = 1
	}
	for _, w := range weekly {
		availableNow = math.Min(availableNow, w.spendable*w.ratio)
	}
	for _, w := range weekly {
		if !w.b.Reset.After(now) {
			continue
		}
		opportunities := quotaOpportunities(p, c, now, fiveEnd, availableNow, w.b.Reset, demand)
		pressure := w.projected * w.ratio / math.Max(.001, opportunities)
		pacing := w.b.Reset
		// Plan to consume the currently spendable tail before the protected
		// 15% unlocks, leaving its scheduled release window for that portion.
		if s := sharedSettings(p, c); s != nil && reserveFloor(p, c, w.b, now) > 0 {
			release := weeklyReserveReleaseAt(s, w.b.Reset)
			beforeRelease := quotaOpportunities(p, c, now, fiveEnd, availableNow, release, demand)
			early := w.spendable * w.ratio / math.Max(.001, beforeRelease)
			if early > pressure {
				pressure = early
				pacing = release
			}
		}
		if pressure > rank.WeeklyPressure {
			rank.WeeklyPressure = pressure
			rank.WeeklyUnits = w.projected * w.ratio
			rank.SpendableWeeklyUnits = w.spendable * w.ratio
			rank.Opportunities = opportunities
			rank.WeeklyReset = w.b.Reset
			rank.PacingDeadline = pacing
			rank.Ratio = w.ratio
			rank.RatioSource = w.source
		}
	}
	// Weekly allocation dominates ordinary headroom and active-load differences.
	// No ownership bonus, 70% ownership switch or 85% avoidance remains.
	rank.Score = 2*rank.WeeklyPressure/(1+rank.WeeklyPressure) + .02*availableNow - .001*float64(min(active, 20)) - .002*clamp(age.Seconds()/float64(p.MaxAge), 0, 1)
	rank.Reason = fmt.Sprintf("Weekly deadline demand %.2f; %.2f five-hour equivalents before %s", rank.WeeklyPressure, rank.WeeklyUnits, rank.WeeklyReset.Local().Format("Jan 02 15:04"))
	if rank.WeeklyReset.IsZero() {
		rank.Reason = "Available included quota; no future weekly reset reported"
	}
	if hasFive && fiveEnd.After(now) {
		hours := fiveEnd.Sub(now).Hours()
		waste := math.Max(0, availableNow-demand*hours)
		rank.FiveHourWaste = waste
		// Only genuinely imminent, predicted-unused five-hour capacity gets
		// an override tier. The reserve gate above is never bypassed.
		if hours <= float64(expiryWindowSeconds)/3600 && waste >= expiryMinimumWaste {
			rank.Expiring = true
			rank.Score = 5 + math.Min(5, waste/math.Max(hours, 1.0/12)) + .1*math.Min(2, rank.WeeklyPressure)
			rank.Reason = fmt.Sprintf("Five-hour expiry: %.0f%% usable capacity at risk in %.0f min", waste*100, hours*60)
		}
	}
	return rank, true
}
