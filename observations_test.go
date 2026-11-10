package main

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

func observationFixture(t *testing.T) (policy, candidate, snapshot, time.Time, time.Time) {
	t.Helper()
	p, c, _, at := admissionFixture(t)
	p.RequireCreditsOff = true
	p.Poll = 60
	p.MaxAge = 300
	q := sample(at, .9, .88, 4*time.Hour)
	signalAt := at.Add(4 * time.Minute)
	now := signalAt.Add(30 * time.Second)
	return p, c, q, signalAt, now
}

func observationHeaders(q snapshot) http.Header {
	h := quotaHeaders(.1, .12, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset)
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", " Rejected ")
	return h
}

func TestClaudeHeaderObservationsKeepThrottledQuotaEligible(t *testing.T) {
	p, c, q, signalAt, now := observationFixture(t)
	recordAdmissionSignals(c.ID, observationHeaders(q), signalAt, now)
	if rank, ok := rankAccount(p, c, "opus", q, now, 0); !ok {
		t.Error("fresh response observations did not preserve eligibility:", rank.Reason)
	} else if rank.QuotaAgeSeconds != now.Sub(signalAt).Seconds() {
		t.Error("rank used endpoint age:", rank.QuotaAgeSeconds)
	}
	inventoryFixture(t, []hostAuthFile{{ID: c.ID, Provider: c.Provider}}, []hostCredentialModels{{ID: c.ID, Provider: c.Provider, Models: []string{"opus"}}})
	state.Quota = map[string]snapshot{c.ID: q}
	if got := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); len(got) != 0 {
		t.Error("fresh response observations did not pass preflight:", got["StatusCode"])
	}
}

func TestClaudeHeaderObservationCoverage(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"complete", true}, {"weekly_alias", true}, {"zero_bucket_reset", true}, {"new_window", true},
		{"unrelated_family", true}, {"extra_usage", true}, {"best_effort", true}, {"beyond_max_age", true},
		{"missing_five", false}, {"model_specific", false}, {"older_signal", false}, {"equal_signal", false},
		{"previous_window", false}, {"expired_window", false}, {"stale_signal", false}, {"no_endpoint", false}, {"future_endpoint", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c, q, at, now := observationFixture(t)
			h := observationHeaders(q)
			switch tc.name {
			case "weekly_alias":
				q.Groups[0].Buckets[1].Window = "weekly"
			case "zero_bucket_reset":
				q.Groups[0].Buckets[0].Reset = time.Time{}
			case "new_window":
				q.Groups[0].Buckets[0].Reset = q.Groups[0].Buckets[0].Reset.Add(-2 * time.Hour)
			case "unrelated_family", "model_specific":
				family := "sonnet"
				if tc.name == "model_specific" {
					family = "opus"
				}
				q.Groups[0].Buckets = append(q.Groups[0].Buckets, bucket{Window: "seven_day_" + family, Remaining: .8, Reset: q.Groups[0].Buckets[1].Reset})
			case "extra_usage":
				q.Groups[0].Buckets = append(q.Groups[0].Buckets, bucket{Window: "extra_usage", Remaining: 1})
			case "best_effort":
				p.RequireCreditsOff = false
				h.Del("Anthropic-Ratelimit-Unified-Overage-Status")
			case "beyond_max_age":
				now = q.At.Add(time.Duration(p.MaxAge+1) * time.Second)
			case "missing_five":
				h.Del("Anthropic-Ratelimit-Unified-5h-Utilization")
			case "older_signal":
				at = q.At.Add(-time.Second)
			case "equal_signal":
				at = q.At
			case "previous_window":
				h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(q.Groups[0].Buckets[1].Reset.Add(-24*time.Hour).Unix(), 10))
			case "expired_window":
				h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(-time.Second).Unix(), 10))
			case "stale_signal":
				at = q.At.Add(10 * time.Second)
			case "no_endpoint":
				q.At = time.Time{}
			case "future_endpoint":
				q.At = now.Add((quotaResetJitterSeconds + 1) * time.Second)
			}
			recordAdmissionSignals(c.ID, h, at, now)
			if rank, ok := rankAccount(p, c, "opus", q, now, 0); ok != tc.want {
				t.Fatalf("eligible = %v, want %v: %s", ok, tc.want, rank.Reason)
			}
		})
	}
}

func TestClaudeHeaderObservationUsesOldestCoveredWindow(t *testing.T) {
	p, c, q, at, now := observationFixture(t)
	h := observationHeaders(q)
	h.Del("Anthropic-Ratelimit-Unified-7d-Utilization")
	recordAdmissionSignals(c.ID, h, at.Add(-20*time.Second), now)
	h = observationHeaders(q)
	h.Del("Anthropic-Ratelimit-Unified-5h-Utilization")
	recordAdmissionSignals(c.ID, h, at, now)
	rank, ok := rankAccount(p, c, "opus", q, now, 0)
	if !ok || rank.QuotaAgeSeconds != 50 {
		t.Fatal("freshest window concealed the oldest observation", rank)
	}
	// A newer weekly response alone cannot keep the five-hour reading fresh.
	recordAdmissionSignals(c.ID, h, now.Add(time.Minute), now.Add(time.Minute))
	if _, ok := rankAccount(p, c, "opus", q, now.Add(2*time.Minute), 0); ok {
		t.Fatal("one fresh window concealed an expired observation")
	}
}

func TestClaudeHeaderObservationPreservesReserveReason(t *testing.T) {
	p, c, q, at, now := observationFixture(t)
	p.RecordRouting = true
	h := observationHeaders(q)
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.86")
	recordAdmissionSignals(c.ID, h, at, now)
	rank, ok := rankAccount(p, c, "opus", q, now, 0)
	if ok || rank.Reason != "Shared seven_day reserve and next-request buffer" {
		t.Fatal("fresh headers bypassed reserve or reported stale quota", rank)
	}
	inventoryFixture(t, []hostAuthFile{{ID: c.ID, Provider: c.Provider}}, []hostCredentialModels{{ID: c.ID, Provider: c.Provider, Models: []string{"opus"}}})
	state.Quota = map[string]snapshot{c.ID: q}
	if got := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); got["StatusCode"] != 429 {
		t.Fatal("preflight bypassed header reserve", got)
	}
	raw, err := os.ReadFile("team-routing-rejections.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Reasons map[string]int `json:"blocking_reasons"`
	}
	if err := json.Unmarshal(raw, &event); err != nil || event.Reasons["reserve_or_projected_usage"] != 1 || event.Reasons["fresh_quota_pending"] != 0 {
		t.Fatal("preflight mislabeled fresh headers", string(raw), err)
	}
}

func TestClaudeBillingHeaderAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		stale                bool
	}{
		{"allowed", " Allowed ", "Extra usage is available at the provider; account is excluded", false},
		{"unknown_status", "unknown", "Extra usage is available at the provider; account is excluded", false},
		{"empty_status", " ", "Extra usage is available at the provider; account is excluded", false},
		{"rejected", "rejected", "", false},
		{"enabled", "rejected", "Extra usage is enabled at the provider; account is excluded", false},
		{"unknown_setting", "rejected", "Extra usage setting is unknown; account is excluded", false},
		{"missing_setting", "rejected", "Extra usage setting is unknown; account is excluded", false},
		{"no_header_fresh", "", "", false},
		{"no_header_stale", "", "A recent included quota and billing reading is required", true},
		{"stale_billing", "rejected", "A recent included quota and billing reading is required", true},
		{"older_allowed", "allowed", "", false},
		{"best_effort_allowed", "allowed", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c, q, at, now := observationFixture(t)
			h := observationHeaders(q)
			h.Del("Anthropic-Ratelimit-Unified-Overage-Status")
			recordAdmissionSignals(c.ID, h, at, now)
			billingAt := at
			if !tc.stale {
				q.At = now.Add(-time.Minute)
			}
			switch tc.name {
			case "enabled":
				on := true
				q.ExtraUsage.Enabled = &on
			case "unknown_setting":
				q.ExtraUsage.Enabled = nil
			case "missing_setting":
				q.ExtraUsage = nil
			case "older_allowed":
				billingAt = q.At.Add(-time.Second)
			case "stale_billing":
				billingAt = q.At.Add(time.Second)
			case "best_effort_allowed":
				p.RequireCreditsOff = false
			}
			billingHeaders := http.Header{}
			if tc.status != "" {
				billingHeaders.Set("Anthropic-Ratelimit-Unified-Overage-Status", tc.status)
			}
			recordAdmissionSignals(c.ID, billingHeaders, billingAt, now)
			rank, ok := rankAccount(p, c, "opus", q, now, 0)
			if tc.reason != "" {
				if ok || rank.Reason != tc.reason {
					t.Fatalf("eligible = %v, reason = %q, want %q", ok, rank.Reason, tc.reason)
				}
			} else if !ok {
				t.Fatal("usable included quota blocked:", rank.Reason)
			}
		})
	}
}

func TestBillingHeaderPersistenceAndOrdering(t *testing.T) {
	p, c, q, at, now := observationFixture(t)
	if err := os.WriteFile("team-routing-admission.json", []byte(`{"completed":{},"signals":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	admission.Lock()
	admission.Path = ""
	loadAdmission()
	broken := admission.Broken
	admission.Unlock()
	if broken {
		t.Fatal("legacy admission file marked broken")
	}
	recordAdmissionSignals(c.ID, observationHeaders(q), at, now)
	allowed := http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {" Allowed "}}
	recordAdmissionSignals(c.ID, allowed, at.Add(time.Second), now)
	recordAdmissionSignals(c.ID, observationHeaders(q), at, now) // Older completion arrives last.
	q.At = at.Add(-time.Second)
	want := "Extra usage is available at the provider; account is excluded"
	for _, reload := range []bool{false, true} {
		if reload {
			admission.Lock()
			admission.Path = ""
			admission.Unlock()
		}
		if reason := creditSafetyReason(p, c, q, now); reason != want {
			t.Fatal("billing ordering or reload lost newer observation", reload, reason)
		}
	}
	recordAdmissionSignals(c.ID, observationHeaders(q), at.Add(2*time.Second), now)
	admission.Lock()
	admission.Path = ""
	admission.Unlock()
	if reason := creditSafetyReason(p, c, q, now); reason != "" {
		t.Fatal("newer rejected observation did not survive reload", reason)
	}
}

func TestBillingHeaderTimestampValidation(t *testing.T) {
	for _, kind := range []string{"future", "retained", "zero"} {
		t.Run(kind, func(t *testing.T) {
			p, c, q, _, now := observationFixture(t)
			q.At = now.Add(-time.Minute)
			at, want := now.Add((quotaResetJitterSeconds+1)*time.Second), ""
			if kind == "retained" {
				at = now.Add(-(ledgerRetentionSeconds + 1) * time.Second)
			} else if kind == "zero" {
				at = time.Time{}
				want = "Extra usage is available at the provider; account is excluded"
			}
			recordAdmissionSignals(c.ID, http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": {"allowed"}}, at, now)
			if reason := creditSafetyReason(p, c, q, now); reason != want {
				t.Fatal("invalid request-start timestamp changed billing", reason)
			}
		})
	}
}

func TestHeaderObservationsLeaveOtherProvidersUnchanged(t *testing.T) {
	for _, provider := range []string{"codex", "antigravity"} {
		t.Run(provider, func(t *testing.T) {
			p, c, q, at, now := observationFixture(t)
			c.Provider = provider
			q.Groups[0].Name = "opus"
			before, eligible := rankAccount(p, c, "opus", q, now, 0)
			recordAdmissionSignals(c.ID, observationHeaders(q), at, now)
			after, ok := rankAccount(p, c, "opus", q, now, 0)
			if ok != eligible || after != before {
				t.Fatal("Claude headers changed provider ranking", before, after)
			}
			if provider == "codex" && ok {
				q.At = now.Add(-time.Duration(p.MaxAge+1) * time.Second)
				if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
					t.Fatal("Claude headers refreshed stale Codex quota")
				}
			}
		})
	}
}

func TestAdaptiveDemandIncludesHeaderObservations(t *testing.T) {
	p, c, q, at, now := observationFixture(t)
	q.Rates = map[string]float64{"Claude|five_hour": .2}
	recordAdmissionSignals(c.ID, observationHeaders(q), at, now)
	state.Quota = map[string]snapshot{c.ID: q}
	state.Pins = map[string]binding{}
	if got := adaptiveChoose(p, c.Provider, "opus", nil, nil, []candidate{c}, now); got["AuthID"] != c.ID {
		t.Fatal("fresh header account omitted", got)
	}
	evidence.Lock()
	actual := evidence.Recent[len(evidence.Recent)-1].Candidates[0].adaptiveRank
	evidence.Unlock()
	q.DemandRate = .2
	want, _ := rankAccount(p, c, "opus", q, now, 0)
	if math.Abs(actual.Score-want.Score) > 1e-9 || actual.Opportunities != want.Opportunities {
		t.Fatal("demand aggregation ignored fresh header account", actual, want)
	}
}
