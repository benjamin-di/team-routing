package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

func admissionFixture(t *testing.T) (policy, candidate, snapshot, time.Time) {
	t.Helper()
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	p := scheduledPolicy()
	p.RequireCreditsOff = false
	raw, _ := json.Marshal(p)
	if err := os.WriteFile("team-routing.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	now := localTime("2026-11-01 19:00")
	c := candidate{ID: "claude-burst-shared", Provider: "claude"}
	q := sample(now, 1, .7, 4*time.Hour)
	on := true
	q.ExtraUsage = &extraUsageStatus{Enabled: &on}
	return p, c, q, now
}
func quotaHeaders(five, weekly float64, reset, weeklyReset time.Time) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", strconv.FormatFloat(five, 'f', 3, 64))
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(weekly, 'f', 3, 64))
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(weeklyReset.Unix(), 10))
	return h
}
func TestCompletedBurstCannotReuseCachedAllowance(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	admitted := 0
	for i := 0; i < 441; i++ {
		at := now.Add(time.Duration(i) * 50 * time.Millisecond)
		if _, ok := rankAccount(p, c, "opus", q, at, 0); !ok {
			break
		}
		admitted++
		recordAdmissionCompletion(c.ID, fmt.Sprint("burst-", i), at)
	}
	if admitted == 0 || admitted > 156 {
		t.Fatal("completed work reused unchanged cached allowance", admitted)
	}
	rank, _ := rankAccount(p, c, "opus", q, now.Add(time.Minute), 0)
	if rank.UnreconciledCompletions != admitted || rank.FiveHourProjected < .2-1e-9 {
		t.Fatal("completed debit missing or reserve overdrawn", rank)
	}
	admission.Lock()
	admission.Path = ""
	admission.Unlock()
	if _, ok := rankAccount(p, c, "opus", q, now.Add(time.Minute), 0); ok {
		t.Fatal("restart forgot unpolled consumption")
	}
}
func TestFreshQuotaReconcilesCompletedReservations(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	for i := 0; i < 80; i++ {
		recordAdmissionCompletion(c.ID, fmt.Sprint(i), now)
	}
	next := now.Add(5 * time.Minute)
	q.At = next
	q.RequestedAt = q.At
	q.Groups[0].Buckets[0].Remaining = .3
	rank, ok := rankAccount(p, c, "opus", q, next, 0)
	if !ok || rank.UnreconciledCompletions != 0 || rank.FiveHourProjected < .299 {
		t.Fatal("fresh quota double charged old completions", rank)
	}
	recordAdmissionCompletion(c.ID, "late", next.Add(-5*time.Second))
	rank, _ = rankAccount(p, c, "opus", q, next, 0)
	if rank.UnreconciledCompletions != 1 {
		t.Fatal("settlement grace missed near-sample request", rank)
	}
}
func TestResponseQuotaStopsBeforeCachedQuota(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	h := quotaHeaders(.79, .3, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset)
	if len(recordAdmissionSignals(c.ID, h, now, now)) != 2 {
		t.Fatal("valid native quota headers ignored")
	}
	rank, ok := rankAccount(p, c, "opus", q, now, 0)
	if ok || rank.QuotaSignal == nil || rank.FiveHourProjected > .21 {
		t.Fatal("old healthy cache overrode live reserve threshold", rank)
	}
	// Later/out-of-order higher remaining cannot reopen this window.
	h = quotaHeaders(.2, .1, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset)
	recordAdmissionSignals(c.ID, h, now.Add(time.Second), now.Add(time.Second))
	if _, ok = rankAccount(p, c, "opus", q, now.Add(time.Second), 0); ok {
		t.Fatal("higher late signal reopened protected quota")
	}
}
func TestLiveResetKeepsWholeSessionProtectedAfterEight(t *testing.T) {
	p, c, q, _ := admissionFixture(t)
	now := localTime("2026-11-01 20:30")
	q.At = now
	q.RequestedAt = q.At
	q.Groups[0].Buckets[0].Reset = time.Time{} // Poll still lacks the active reset.
	reset := localTime("2026-11-01 23:00")
	recordAdmissionSignals(c.ID, quotaHeaders(.81, .3, reset, q.Groups[0].Buckets[1].Reset), now, now)
	rank, ok := rankAccount(p, c, "opus", q, now, 0)
	if ok || rank.FiveHourFloor != .2 || !rank.FiveHourReset.Equal(reset) {
		t.Fatal("actual 18-23 session lost protection after 20:00", rank)
	}
}
func TestNewWindowDoesNotInheritOldSignalOrDebt(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	recordAdmissionSignals(c.ID, quotaHeaders(1, .3, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset), now, now)
	recordAdmissionCompletion(c.ID, "old-window", now)
	next := localTime("2026-11-01 23:01")
	q.At = next
	q.RequestedAt = q.At
	q.Groups[0].Buckets[0].Reset = next.Add(5 * time.Hour)
	if _, ok := rankAccount(p, c, "opus", q, next, 0); !ok {
		t.Fatal("new confirmed included window stayed blocked by old signals")
	}
}
func TestAdmissionCompletionDedupeAndConcurrentWork(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	recordAdmissionCompletion(c.ID, "same-request", now)
	recordAdmissionCompletion(c.ID, "same-request", now.Add(time.Millisecond))
	rank, _ := rankAccount(p, c, "opus", q, now.Add(time.Second), 0)
	if rank.UnreconciledCompletions != 1 {
		t.Fatal("usage and terminal callbacks double charged", rank)
	}
	q.Groups[0].Buckets[0].Remaining = .4
	if _, ok := rankAccount(p, c, "opus", q, now, 40); ok {
		t.Fatal("in-flight reservations bypassed completed debt")
	}
}
func TestAdmissionRejectsMalformedSignalsAndCorruptState(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	h := quotaHeaders(.8, .3, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset)
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "NaN")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "invalid")
	if len(recordAdmissionSignals(c.ID, h, now, now)) != 0 {
		t.Fatal("malformed signals invented quota")
	}
	if err := os.WriteFile("team-routing-admission.json", []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	admission.Lock()
	admission.Path = ""
	admission.Unlock()
	if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
		t.Fatal("corrupt admission state allowed work")
	}
}

func TestUsageAndLifecycleIDsShareOneSelectionDebit(t *testing.T) {
	_, c, _, _ := admissionFixture(t)
	metadata := map[string]any{"selection_id": "same-selection", "selected_auth_id": c.ID}
	after, _ := json.Marshal(map[string]any{"RequestID": "http-request-id", "Metadata": metadata})
	if _, err := trackRequest("request.intercept_after", after); err != nil {
		t.Fatal(err)
	}
	usage, _ := json.Marshal(map[string]any{"RequestID": "provider-execution-id", "SelectionID": "same-selection", "AuthID": c.ID, "Provider": "claude", "Detail": map[string]int{"InputTokens": 10, "OutputTokens": 1}})
	if _, err := observeUsage(usage); err != nil {
		t.Fatal(err)
	}
	if _, err := trackRequest("request.complete", after); err != nil {
		t.Fatal(err)
	}
	admission.Lock()
	n := len(admission.Data.Completed)
	admission.Unlock()
	if n != 1 {
		t.Fatal("different host request IDs duplicated one execution debit", n)
	}
}

func TestFreshResponseReadingsReconcileEstimatedCompletionDebt(t *testing.T) {
	p, c, q, now := admissionFixture(t)
	for i := 0; i < 250; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		if rank, ok := rankAccount(p, c, "opus", q, at, 0); !ok {
			t.Fatal("fresh provider headroom lost to accumulated estimates", i, rank)
		}
		recordAdmissionCompletion(c.ID, fmt.Sprint(i), at)
		recordAdmissionSignals(c.ID, quotaHeaders(.10, .30, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset), at, at)
	}
	rank, ok := rankAccount(p, c, "opus", q, now.Add(250*time.Second), 0)
	if !ok || rank.CompletedReserve > .08 || rank.FiveHourProjected < .8 {
		t.Fatal("response observation did not settle older estimates", rank)
	}
}

func TestUnreservedNonClaudeBurstDoesNotFalseRejectHealthyQuota(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	p := defaultPolicy()
	raw, _ := json.Marshal(p)
	if err := os.WriteFile("team-routing.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	now := localTime("2026-11-01 19:00")
	c := candidate{ID: "ag-ultra", Provider: "antigravity"}
	q := snapshot{
		At: now,
		Groups: []group{{
			Name: "Gemini Models",
			Buckets: []bucket{
				{Window: "five_hour", Remaining: .63, Reset: now.Add(3 * time.Hour)},
				{Window: "weekly", Remaining: .84, Reset: now.Add(5 * 24 * time.Hour)},
			},
		}},
	}
	for i := range 180 {
		recordAdmissionCompletion(c.ID, fmt.Sprintf("ag-%d", i), now.Add(time.Duration(i)*time.Second))
	}
	later := now.Add(4 * time.Minute)
	if rank, ok := rankAccount(p, c, "gemini-4-neon", q, later, 0); !ok {
		t.Fatalf("unreserved non-Claude account with 63%% headroom was falsely rejected: %+v", rank)
	}
	inventoryFixture(t, []hostAuthFile{{ID: c.ID, Provider: "antigravity"}}, []hostCredentialModels{{ID: c.ID, Provider: "antigravity", Models: []string{"gemini-4-neon"}}})
	state.Quota = map[string]snapshot{c.ID: q}
	if r := availabilityPreflight(p, availabilityRequest{Model: "gemini-4-neon"}, later); len(r) != 0 {
		t.Fatalf("preflight falsely rejected healthy unreserved non-Claude account: %v", r)
	}
	if got := adaptiveChoose(p, "antigravity", "gemini-4-neon", nil, nil, []candidate{c}, later); got["AuthID"] != c.ID {
		t.Fatalf("selection failed to choose healthy unreserved non-Claude account: %v", got)
	}
	// Real provider exhaustion must still reject.
	q.Groups[0].Buckets[0].Remaining = 0
	if _, ok := rankAccount(p, c, "gemini-4-neon", q, later, 0); ok {
		t.Fatal("exhausted non-Claude account remained eligible")
	}
}

func TestLearnedRequestCostCalibratesReservedNonClaudeAccount(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	p := scheduledPolicy()
	reserve := *p.SharedClaude
	p.ProviderReserves = map[string]*sharedClaudePolicy{"antigravity": &reserve}
	raw, _ := json.Marshal(p)
	if err := os.WriteFile("team-routing.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	now := localTime("2026-11-01 19:00")
	c := candidate{ID: "ag-reserved", Provider: "antigravity"}
	old := snapshot{
		At: now,
		Groups: []group{{
			Name: "Gemini Models",
			Buckets: []bucket{
				{Window: "five_hour", Remaining: .80, Reset: now.Add(3 * time.Hour)},
				{Window: "weekly", Remaining: .85, Reset: now.Add(5 * 24 * time.Hour)},
			},
		}},
	}
	for i := range 150 {
		recordAdmissionCompletionForModel(c.ID, "gemini-4-neon", fmt.Sprintf("learn-%d", i), now.Add(time.Duration(i+1)*time.Second))
	}
	polledAt := now.Add(5 * time.Minute)
	next := snapshot{
		At: polledAt,
		Groups: []group{
			{
				Name: "Gemini Models",
				Buckets: []bucket{
					{Window: "five_hour", Remaining: .785, Reset: now.Add(3 * time.Hour)},
					{Window: "weekly", Remaining: .845, Reset: now.Add(5 * 24 * time.Hour)},
				},
			},
			{
				Name: "Claude and GPT models",
				Buckets: []bucket{
					{Window: "five_hour", Remaining: 1, Reset: now.Add(3 * time.Hour)},
				},
			},
		},
	}
	old.Groups = append(old.Groups, group{Name: "Claude and GPT models", Buckets: []bucket{{Window: "five_hour", Remaining: 1, Reset: now.Add(3 * time.Hour)}}})
	next.RequestCosts = estimateRequestCosts(c.Provider, c.ID, old, next, polledAt)
	cost := next.RequestCosts["Gemini Models|five_hour"]
	if cost <= 0 || cost >= 0.0005 {
		t.Fatalf("expected learned five_hour cost near 0.0001/req, got %v", cost)
	}
	if _, leaked := next.RequestCosts["Claude and GPT models|five_hour"]; leaked {
		t.Fatalf("Gemini completions leaked learned cost into unrelated group: %+v", next.RequestCosts)
	}
	// A subsequent 150-request burst at 0.63 remaining should reserve ~0.015,
	// staying well above the 0.20 reserve floor.
	next.Groups[0].Buckets[0].Remaining = .63
	for i := range 150 {
		recordAdmissionCompletionForModel(c.ID, "gemini-4-neon", fmt.Sprintf("burst-%d", i), polledAt.Add(time.Duration(i+1)*time.Second))
	}
	rank, ok := rankAccount(p, c, "gemini-4-neon", next, polledAt.Add(4*time.Minute), 0)
	if !ok || rank.CompletedReserve > .05 || rank.FiveHourProjected < .55 {
		t.Fatalf("learned cost failed to prevent false reserve block: %+v", rank)
	}
	// Gemini burst must not debit the unrelated Claude and GPT models group.
	claudeRank, ok := rankAccount(p, c, "claude-opus-5-5", next, polledAt.Add(4*time.Minute), 0)
	if !ok || claudeRank.UnreconciledCompletions != 0 {
		t.Fatalf("Gemini completions debited unrelated Antigravity group: %+v", claudeRank)
	}
	// Reserve floor must still stop admission when real quota approaches 0.20.
	next.Groups[0].Buckets[0].Remaining = .21
	if _, ok := rankAccount(p, c, "gemini-4-neon", next, polledAt.Add(4*time.Minute), 0); ok {
		t.Fatal("reserved non-Claude account bypassed its five-hour reserve floor")
	}
}

func TestEstimatedDepletedNonClaudeNeverOutranksHealthyPeer(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	p := defaultPolicy()
	raw, _ := json.Marshal(p)
	if err := os.WriteFile("team-routing.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	now := localTime("2026-11-01 19:00")
	busy := candidate{ID: "codex-busy", Provider: "codex"}
	idle := candidate{ID: "codex-idle", Provider: "codex"}
	// Same weekly budget near deadline; busy has a burst pushing its estimated
	// five-hour projection <= 0, while idle has 90% five-hour headroom.
	makeSnap := func(five float64) snapshot {
		return snapshot{
			At: now,
			Groups: []group{{
				Name: "Codex",
				Buckets: []bucket{
					{Window: "five_hour", Remaining: five, Reset: now.Add(4 * time.Hour)},
					{Window: "weekly", Remaining: .80, Reset: now.Add(4 * time.Hour)},
				},
			}},
		}
	}
	for i := range 180 {
		recordAdmissionCompletionForModel(busy.ID, "gpt-5.4", fmt.Sprintf("busy-%d", i), now.Add(time.Duration(i)*time.Second))
	}
	later := now.Add(4 * time.Minute)
	state.Lock()
	state.Quota = map[string]snapshot{busy.ID: makeSnap(.60), idle.ID: makeSnap(.90)}
	state.Pins = map[string]binding{}
	state.Unlock()
	if got := adaptiveChoose(p, "codex", "gpt-5.4", nil, nil, []candidate{busy, idle}, later); got["AuthID"] != idle.ID {
		t.Fatalf("estimated-depleted account outranked healthy peer: %v", got)
	}
	// Even with an existing pin on idle, weeklyDeadlineRescue must not pull it onto busy.
	pk := affinityKey(nil, nil, "codex", "gpt-5.4")
	state.Lock()
	state.Pins[pk] = binding{ID: idle.ID, At: now, BoundAt: now}
	state.Unlock()
	if got := adaptiveChoose(p, "codex", "gpt-5.4", nil, nil, []candidate{busy, idle}, later); got["AuthID"] != idle.ID {
		t.Fatalf("weeklyDeadlineRescue pulled pinned session onto estimated-depleted account: %v", got)
	}
	// Conversely, a session pinned on busy should move to healthy idle.
	state.Lock()
	state.Pins[pk] = binding{ID: busy.ID, At: now, BoundAt: now}
	state.Unlock()
	if got := adaptiveChoose(p, "codex", "gpt-5.4", nil, nil, []candidate{busy, idle}, later); got["AuthID"] != idle.ID {
		t.Fatalf("session pinned to estimated-depleted account did not move to healthy peer: %v", got)
	}
}
