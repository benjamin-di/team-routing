package main

import (
	"math"
	"testing"
	"time"
)

func deadlineSample(now time.Time, five, week float64, fiveEnd, weekEnd time.Time) snapshot {
	off := false
	return snapshot{At: now, ExtraUsage: &extraUsageStatus{Enabled: &off}, Groups: []group{{Name: "Claude", Buckets: []bucket{{Window: "five_hour", Remaining: five, Reset: fiveEnd}, {Window: "seven_day", Remaining: week, Reset: weekEnd}}}}}
}
func TestSafwanDeadlineOverridesOwnershipAndExistingBinding(t *testing.T) {
	now := localTime("2026-10-31 08:30")
	p := protectedPolicy()
	mine := candidate{ID: "mine", Provider: "claude", Metadata: map[string]any{"email": "mine@example.com"}}
	safwan := candidate{ID: "safwan", Provider: "claude"}
	mq := deadlineSample(now, .7, .4, now.Add(5*time.Hour), localTime("2026-11-04 14:00"))
	sq := deadlineSample(now, 1, .64, time.Time{}, localTime("2026-11-01 18:00"))
	a, ok := rankAccount(p, safwan, "opus", sq, now, 0)
	if !ok {
		t.Fatal("Safwan blocked")
	}
	b, _ := rankAccount(p, mine, "opus", mq, now, 0)
	if a.Score <= b.Score || math.Abs(a.WeeklyUnits-3.2) > 1e-8 || math.Abs(a.SpendableWeeklyUnits-2.45) > 1e-8 || !a.PacingDeadline.Equal(localTime("2026-10-31 18:00")) {
		t.Fatal("deadline plan ignored weekly unlock", a, b)
	}
	state.Pins = map[string]binding{}
	state.Quota = map[string]snapshot{"mine": mq, "safwan": sq}
	choose(p, "claude", "opus", nil, nil, []candidate{mine}, now)
	if r := choose(p, "claude", "opus", nil, nil, []candidate{mine, safwan}, now); r["AuthID"] != "safwan" {
		t.Fatal("fresh binding prevented urgent reallocation", r)
	}
}
func TestDeadlineOwnershipOnlyBreaksEqualScores(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	p.Owned = []string{"mine@example.com"}
	own := candidate{ID: "z-own", Provider: "claude", Metadata: map[string]any{"email": "mine@example.com"}}
	peer := candidate{ID: "a-peer", Provider: "claude"}
	q := sample(now, .8, .2, 4*time.Hour)
	a, _ := rankAccount(p, own, "opus", q, now, 0)
	b, _ := rankAccount(p, peer, "opus", q, now, 0)
	if a.Score != b.Score {
		t.Fatal("numerical ownership bonus survived", a, b)
	}
	state.Pins = map[string]binding{}
	state.Quota = map[string]snapshot{own.ID: q, peer.ID: q}
	if r := choose(p, "claude", "opus", nil, nil, []candidate{peer, own}, now); r["AuthID"] != own.ID {
		t.Fatal("equal-score ownership tie-break missing", r)
	}
	pq := q
	pq.Groups = []group{{Name: "Claude", Buckets: []bucket{{Window: "five_hour", Remaining: .8, Reset: now.Add(4 * time.Hour)}, {Window: "seven_day", Remaining: .2, Reset: now.Add(36 * time.Hour)}}}}
	state.Quota[peer.ID] = pq
	if r := choose(p, "claude", "opus", nil, nil, []candidate{peer, own}, now); r["AuthID"] != peer.ID {
		t.Fatal("ownership/affinity beat material expiry pressure", r)
	}
}
func TestOpportunityPlanningHonorsProtectedFutureWindows(t *testing.T) {
	p := protectedPolicy()
	now := localTime("2026-11-01 09:00")
	shared := candidate{Provider: "claude"}
	got := quotaOpportunities(p, shared, now, localTime("2026-11-01 14:00"), .8, localTime("2026-11-01 20:00"))
	if math.Abs(got-2.4) > 1e-8 {
		t.Fatal("future protected/partial windows wrong", got)
	}
	own := candidate{Provider: "claude", Metadata: map[string]any{"email": "mine@example.com"}}
	got = quotaOpportunities(p, own, now, localTime("2026-11-01 14:00"), 1, localTime("2026-11-01 20:00"))
	if math.Abs(got-3) > 1e-8 {
		t.Fatal("owned future capacity wrongly reserved", got)
	}
}
func TestLegacyThresholdsNoLongerChangeDeadlineScore(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	q := sample(now, .1, .6, 3*time.Hour)
	c := candidate{Provider: "claude"}
	a, ok := rankAccount(p, c, "opus", q, now, 0)
	if !ok {
		t.Fatal("usable account stranded at ninety percent consumption")
	}
	p.Cutoff = .05
	p.ExpiringUsed = .04
	p.ExpiringSeconds = 60
	b, _ := rankAccount(p, c, "opus", q, now, 0)
	if a.Score != b.Score {
		t.Fatal("legacy thresholds still shape allocation")
	}
}
func TestImminentFiveHourWasteCanOverrideWeeklyPlan(t *testing.T) {
	now := localTime("2026-10-31 08:30")
	p := protectedPolicy()
	a := candidate{ID: "weekly", Provider: "claude"}
	b := candidate{ID: "expiring", Provider: "claude"}
	weekly := deadlineSample(now, 1, .64, time.Time{}, localTime("2026-11-01 18:00"))
	soon := deadlineSample(now, .8, .8, now.Add(10*time.Minute), now.Add(6*24*time.Hour))
	wa, _ := rankAccount(p, a, "opus", weekly, now, 0)
	sa, _ := rankAccount(p, b, "opus", soon, now, 0)
	if !sa.Expiring || sa.Score <= wa.Score {
		t.Fatal("imminent useful capacity stranded", wa, sa)
	}
	// Plenty of observed eligible demand means no predicted waste override.
	soon.DemandRate = 10
	sa, _ = rankAccount(p, b, "opus", soon, now, 0)
	if sa.Expiring {
		t.Fatal("five-hour override ignores observed demand")
	}
}
func TestWeeklyConversionLearnsAndSurvivesRoundingAndResets(t *testing.T) {
	now := time.Now()
	old := sample(now, .9, .9, 3*time.Hour)
	next := sample(now.Add(5*time.Minute), .7, .86, 3*time.Hour)
	for i := range next.Groups[0].Buckets {
		next.Groups[0].Buckets[i].Reset = old.Groups[0].Buckets[i].Reset.Add(200 * time.Millisecond)
	}
	ratios := estimateEquivalents(old, next)
	if math.Abs(ratios["Claude|seven_day"]-5) > 1e-8 {
		t.Fatal("paired conversion not learned", ratios)
	}
	next.Equivalents = ratios
	if got := estimateEquivalents(next, next)["Claude|seven_day"]; got != ratios["Claude|seven_day"] {
		t.Fatal("idle sample erased conversion")
	}
	reset := sample(now, 1, .86, 5*time.Hour)
	if got := estimateEquivalents(next, reset)["Claude|seven_day"]; got != ratios["Claude|seven_day"] {
		t.Fatal("five-hour reset fabricated/lost conversion")
	}
	q := next
	q.Equivalents = map[string]float64{"Claude|seven_day": 7}
	r, source := quotaRatio(defaultPolicy(), q, "Claude|seven_day")
	if r != 7 || source != "learned" {
		t.Fatal("learned ratio not used")
	}
}
func TestClaudeWeeklyOnlyAccountIsEligible(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	off := false
	weeklyOnly := func(week float64) snapshot {
		return snapshot{At: now, ExtraUsage: &extraUsageStatus{Enabled: &off}, Groups: []group{{Name: "Claude", Buckets: []bucket{{Window: "seven_day", Remaining: week, Reset: now.Add(72 * time.Hour)}}}}}
	}
	c := candidate{ID: "weekly-only", Provider: "claude"}
	if r, ok := rankAccount(p, c, "opus", weeklyOnly(.56), now, 0); !ok {
		t.Fatal("weekly-only Claude account blocked", r.Reason)
	}
	if _, ok := rankAccount(p, c, "opus", weeklyOnly(0), now, 0); ok {
		t.Fatal("exhausted weekly-only Claude account admitted")
	}
	none := snapshot{At: now, ExtraUsage: &extraUsageStatus{Enabled: &off}, Groups: []group{{Name: "Claude", Buckets: []bucket{{Window: "five_hour", Remaining: .9, Reset: now.Add(4 * time.Hour)}}}}}
	if _, ok := rankAccount(p, c, "opus", none, now, 0); ok {
		t.Fatal("Claude account without a weekly reading admitted")
	}
}
