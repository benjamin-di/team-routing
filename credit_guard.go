package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Retain billing flags separately from included quota. Paid balances can never
// increase headroom, ranking, or eligibility.
type extraUsageStatus struct {
	Enabled       *bool    `json:"is_enabled"`
	MonthlyLimit  *float64 `json:"monthly_limit,omitempty"`
	UsedCredits   *float64 `json:"used_credits,omitempty"`
	Currency      string   `json:"currency,omitempty"`
	DecimalPlaces *int     `json:"decimal_places,omitempty"`
	UserDisabled  *bool    `json:"user_disabled,omitempty"`
}

var creditFences = struct {
	sync.Mutex
	Path   string
	Denied map[string]time.Time
	Broken bool
}{}

func loadCreditFences() {
	path, err := filepath.Abs(statePath("team-routing-paid-usage-blocks.json"))
	if err != nil {
		creditFences.Broken = true
		return
	}
	if creditFences.Path == path && creditFences.Denied != nil {
		return
	}
	creditFences.Path = path
	creditFences.Denied = map[string]time.Time{}
	creditFences.Broken = false
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || json.Unmarshal(raw, &creditFences.Denied) != nil || creditFences.Denied == nil {
		creditFences.Broken = true
	}
}
func creditSafetyReason(p policy, c candidate, model string, q snapshot, now time.Time) string {
	if c.Provider != "claude" {
		return ""
	}
	billingReason := creditBillingReason(p, c, model, q, now)
	creditFences.Lock()
	loadCreditFences()
	if creditFences.Broken {
		creditFences.Unlock()
		return "Credit safety state is unavailable"
	}
	detected, denied := creditFences.Denied[c.ID]
	if !denied {
		creditFences.Unlock()
		return billingReason
	}
	proof := ""
	if billingReason == "" {
		proof = paidFenceRecoveryWindow(q, detected)
	}
	if proof != "" {
		_, eligible := rankIncludedAccount(p, c, model, q, now, 0)
		if eligible {
			next := copyCreditFences()
			delete(next, c.ID)
			if err := persistCreditFences(next); err != nil {
				creditFences.Broken = true
				creditFences.Unlock()
				return "Credit safety state is unavailable"
			}
			creditFences.Denied = next
			creditFences.Unlock()
			appendEvidence("team-routing-credit-events.jsonl", map[string]any{"at": now, "event": "recovered", "auth_id": c.ID, "last_paid_signal": detected, "quota_at": q.At, "proof": proof})
			return ""
		}
	}
	creditFences.Unlock()
	return "Paid usage was detected; waiting for usable included quota in a newer window"
}

func creditBillingReason(p policy, c candidate, model string, q snapshot, now time.Time) string {
	maxAge := time.Duration(p.Poll+quotaAgeGraceSeconds) * time.Second
	if q.At.IsZero() || q.At.After(now.Add(quotaResetJitterSeconds*time.Second)) {
		return "A recent included quota and billing reading is required"
	}
	quotaFresh := now.Sub(quotaObservationTime(c, model, q, now)) <= maxAge
	// Best-effort subscription mode relaxes only the provider billing toggle.
	// Explicit paid requests and paid-use fences remain enforced.
	if !p.RequireCreditsOff {
		if !quotaFresh {
			return "A recent included quota and billing reading is required"
		}
		return ""
	}
	admission.Lock()
	loadAdmission()
	billing := admission.Data.Billing[c.ID]
	admission.Unlock()
	if billing.availableAt().After(q.At) {
		return "Extra usage is available at the provider; account is excluded"
	}
	if !quotaFresh {
		return "A recent included quota and billing reading is required"
	}
	if q.ExtraUsage == nil || q.ExtraUsage.Enabled == nil {
		return "Extra usage setting is unknown; account is excluded"
	}
	if *q.ExtraUsage.Enabled {
		return "Extra usage is enabled at the provider; account is excluded"
	}
	if now.Sub(q.At) > maxAge && (billing.Status != "rejected" || now.Sub(billing.At) > maxAge) {
		return "A recent included quota and billing reading is required"
	}
	return ""
}

// Credits may apply unless the endpoint reported them disabled and no newer
// response reported a different overage status.
func providerCreditsMayApply(c candidate, q snapshot) bool {
	if q.ExtraUsage == nil || q.ExtraUsage.Enabled == nil || *q.ExtraUsage.Enabled {
		return true
	}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	billing := admission.Data.Billing[c.ID]
	return admission.Broken || billing.availableAt().After(q.At)
}

// A newer reading alone is insufficient. Require evidence that an included
// window changed since the LAST paid response, allowing 90s provider jitter.
// An idle/full five-hour allowance has no reset timestamp; require at least a
// full five hours since the signal before accepting that reset evidence.
func paidFenceRecoveryWindow(q snapshot, detected time.Time) string {
	grace := quotaResetJitterSeconds * time.Second
	if detected.IsZero() || !q.At.After(detected.Add(grace)) {
		return ""
	}
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			if b.Window == "five_hour" && b.Remaining >= .99 && b.Reset.IsZero() && q.LastFiveHourReset.After(detected) && !q.At.Before(q.LastFiveHourReset.Add(grace)) {
				return "confirmed_idle_five_hour_reset"
			}
			if b.Window == "five_hour" && b.Remaining >= .99 && b.Reset.IsZero() && !q.At.Before(detected.Add(5*time.Hour+grace)) {
				return "idle_full_five_hour_after_reset"
			}
			duration := 5 * time.Hour
			proof := "new_five_hour_window"
			if weeklyWindow(b.Window) {
				duration, proof = 7*24*time.Hour, "new_weekly_window"
			} else if b.Window != "five_hour" {
				continue
			}
			start := b.Reset.Add(-duration)
			if b.Reset.After(q.At) && !start.After(q.At.Add(grace)) && start.After(detected.Add(grace)) {
				return proof
			}
		}
	}
	return ""
}

// Call with creditFences locked. Do not change memory before persistence
// succeeds: a failed recovery must retain the old denial across restarts.
func copyCreditFences() map[string]time.Time {
	next := make(map[string]time.Time, len(creditFences.Denied))
	for id, at := range creditFences.Denied {
		next[id] = at
	}
	return next
}
func persistCreditFences(next map[string]time.Time) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(creditFences.Path), ".paid-use-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), creditFences.Path)
	}
	return err
}

func fencePaidAccount(auth string, at time.Time) {
	creditFences.Lock()
	loadCreditFences()
	if creditFences.Broken {
		creditFences.Unlock()
		return
	}
	if previous, exists := creditFences.Denied[auth]; exists && !at.After(previous) {
		creditFences.Unlock()
		return
	}
	next := copyCreditFences()
	next[auth] = at
	if persistCreditFences(next) != nil {
		creditFences.Broken = true
	}
	creditFences.Denied = next
	creditFences.Unlock()
	appendEvidence("team-routing-credit-events.jsonl", map[string]any{"at": at, "event": "blocked", "auth_id": auth})
}
func overageInUse(h http.Header) bool {
	v := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-In-Use")))
	claim := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim")))
	return v == "true" || v == "1" || claim == "overage" || claim == "extra_usage"
}
func paidRequest(body []byte) bool {
	var r struct {
		Speed       string          `json:"speed"`
		ServiceTier string          `json:"service_tier"`
		Credit      json.RawMessage `json:"fallback_credit_token"`
		Fallbacks   json.RawMessage `json:"fallbacks"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	nonempty := func(v json.RawMessage) bool {
		s := strings.TrimSpace(string(v))
		return s != "" && s != "null" && s != "\"\"" && s != "[]"
	}
	return strings.EqualFold(r.Speed, "fast") || strings.EqualFold(r.ServiceTier, "priority") || nonempty(r.Credit) || nonempty(r.Fallbacks)
}
func genericCreditRejection() map[string]any {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "code": "rate_limit_error", "message": "Usage rate limited. Please try again later."}})
	return map[string]any{"Terminate": true, "StatusCode": 429, "ResponseHeaders": http.Header{"Content-Type": {"application/json"}, "Retry-After": {"300"}}, "ResponseBody": body}
}
