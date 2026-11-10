package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rates are fractions of a quota window consumed per hour, not token counts.
// A reset, increased allowance, or long observation gap invalidates the rate.
func estimateRates(old, next snapshot, now time.Time) map[string]float64 {
	rates := map[string]float64{}
	hours := now.Sub(old.At).Hours()
	if old.At.IsZero() || hours < 1.0/120 || hours > 6 {
		return rates
	}
	previous := map[string]bucket{}
	for _, g := range old.Groups {
		for _, b := range g.Buckets {
			previous[g.Name+"|"+b.Window] = b
		}
	}
	for _, g := range next.Groups {
		for _, b := range g.Buckets {
			k := g.Name + "|" + b.Window
			prior, ok := previous[k]
			if !ok || !sameQuotaWindow(prior.Reset, b.Reset) || b.Remaining > prior.Remaining+1e-9 {
				continue
			}
			observed := (prior.Remaining - b.Remaining) / hours
			if rate, ok := old.Rates[k]; ok {
				observed = .5*rate + .5*observed
			}
			rates[k] = observed
		}
	}
	return rates
}

type activeRequest struct {
	Auth      string
	At        time.Time
	Selection string
}

var traffic = struct {
	sync.Mutex
	Active    map[string]activeRequest
	Completed uint64
	Pending   map[string]activeRequest
	Outcomes  map[string]map[string]uint64
}{Active: map[string]activeRequest{}}

func sameQuotaWindow(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() && b.IsZero()
	}
	// Provider reset timestamps jitter by sub-seconds between successful reads.
	// A real five-hour/weekly reset moves much farther than this tolerance.
	return math.Abs(a.Sub(b).Seconds()) <= quotaResetJitterSeconds
}
func reserveSelection(meta map[string]any, id string, now time.Time) {
	selection, _ := meta["selection_id"].(string)
	if selection == "" {
		return
	}
	traffic.Lock()
	defer traffic.Unlock()
	if traffic.Pending == nil {
		traffic.Pending = map[string]activeRequest{}
	}
	traffic.Pending[selection] = activeRequest{Auth: id, At: now, Selection: selection}
}

// Observe lifecycle only; never rewrite prompts, credentials, or responses.
func trackRequest(method string, raw []byte) (any, error) {
	var r struct {
		RequestID              string
		Metadata               map[string]any
		Outcome                string
		Model                  string
		Headers                http.Header
		StatusCode             int
		StartedAt, CompletedAt time.Time
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.RequestID == "" {
		return map[string]any{}, nil
	}
	traffic.Lock()
	defer traffic.Unlock()
	if method == "request.complete" {
		if prior, ok := traffic.Active[r.RequestID]; ok {
			completed := r.CompletedAt
			if completed.IsZero() {
				completed = time.Now()
			}
			// Rejected client/auth/rate-limit requests do not establish quota use.
			// Metered partial work is independently recorded by the usage callback.
			if !definiteRejection(r.Outcome, r.StatusCode) {
				recordAdmissionCompletion(prior.Auth, completionIdentity(r.RequestID, prior.Selection), completed)
			}
			traffic.Completed++
			if traffic.Outcomes == nil {
				traffic.Outcomes = map[string]map[string]uint64{}
			}
			if traffic.Outcomes[prior.Auth] == nil {
				traffic.Outcomes[prior.Auth] = map[string]uint64{}
			}
			outcome := r.Outcome
			if outcome == "" {
				outcome = "unknown"
			}
			traffic.Outcomes[prior.Auth][outcome]++
			writeRoutingEvent(prior.Auth, r.Model, outcome, r.StatusCode, r.StartedAt, r.CompletedAt)
		}
		delete(traffic.Active, r.RequestID)
	}
	if method == "request.intercept_after" {
		if selection, ok := r.Metadata["selection_id"].(string); ok {
			delete(traffic.Pending, selection)
		}
		if id, ok := r.Metadata["selected_auth_id"].(string); ok && id != "" {
			selection, _ := r.Metadata["selection_id"].(string)
			traffic.Active[r.RequestID] = activeRequest{Auth: id, At: time.Now(), Selection: selection}
		}
	}
	return map[string]any{}, nil
}
func activeCounts(now time.Time) map[string]int {
	traffic.Lock()
	defer traffic.Unlock()
	counts := map[string]int{}
	for k, r := range traffic.Active {
		if now.Sub(r.At) > 2*time.Hour {
			delete(traffic.Active, k)
		} else {
			counts[r.Auth]++
		}
	}
	for k, r := range traffic.Pending {
		if now.Sub(r.At) > time.Minute {
			delete(traffic.Pending, k)
		} else {
			counts[r.Auth]++
		}
	}
	return counts
}

type adaptiveRank struct {
	Priority                                                            int
	ActiveRequests                                                      int
	ID                                                                  string
	Score, Remaining                                                    float64
	Known                                                               bool
	Owned                                                               bool
	Expiring                                                            bool
	WeeklyPressure, WeeklyUnits, SpendableWeeklyUnits, Opportunities    float64
	Ratio                                                               float64
	RatioSource                                                         string
	WeeklyReset, PacingDeadline                                         time.Time
	FiveHourWaste                                                       float64
	Reason                                                              string
	QuotaAgeSeconds, FiveHourReported, FiveHourProjected, FiveHourFloor float64
	CompletedReserve                                                    float64
	UnreconciledCompletions                                             int
	FiveHourReset                                                       time.Time
	QuotaSignal                                                         *quotaSignal `json:",omitempty"`
}

func weeklyWindow(w string) bool {
	return w == "weekly" || w == "seven_day" || strings.HasPrefix(w, "seven_day_")
}

func relevantQuotaBucket(provider, model, window string) bool {
	if window == "extra_usage" {
		return false
	}
	if provider != "claude" || !strings.HasPrefix(window, "seven_day_") || window == "seven_day_oauth_apps" {
		return true
	}
	// Provider-reported model/surface limits are not global limits. Overview
	// and recovery evaluate global allowance; a request also checks its family.
	family := strings.TrimPrefix(window, "seven_day_")
	return model != "" && strings.Contains(strings.ToLower(model), strings.ToLower(family))
}
func relevantGroup(provider, model, name string) bool {
	if provider != "antigravity" {
		return true
	}
	name, model = strings.ToLower(name), strings.ToLower(model)
	return strings.Contains(name, model) || (strings.HasPrefix(model, "gemini") && strings.Contains(name, "gemini")) || ((strings.HasPrefix(model, "claude") || strings.HasPrefix(model, "gpt")) && strings.Contains(name, "claude"))
}
func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func adaptiveChoose(p policy, provider, model string, h http.Header, meta map[string]any, candidates []candidate, now time.Time) map[string]any {
	counts := activeCounts(now)
	demand := 0.0
	for _, c := range candidates {
		q := state.Quota[c.ID]
		if now.Sub(quotaObservationTime(c, model, q, now)) > time.Duration(p.Poll+quotaAgeGraceSeconds)*time.Second {
			continue
		}
		for _, g := range q.Groups {
			if relevantGroup(c.Provider, model, g.Name) {
				demand += q.Rates[g.Name+"|five_hour"]
			}
		}
	}
	ranks := []adaptiveRank{}
	diagnostics := []rankEvidence{}
	for _, c := range candidates {
		q := state.Quota[c.ID]
		q.DemandRate = demand
		r, ok := rankAccount(p, c, model, q, now, counts[c.ID])
		r.Priority = p.AccountPriority[c.ID]
		r.ActiveRequests = counts[c.ID]
		diagnostics = append(diagnostics, rankEvidence{adaptiveRank: r, Eligible: ok})
		if ok {
			ranks = append(ranks, r)
		}
	}
	if len(ranks) == 0 {
		recordDecision(p, h, meta, provider, model, "", "", "no_eligible_quota", 0, diagnostics, now)
		return map[string]any{"Handled": true, "Reject": true, "RejectCode": "rate_limit_error", "RejectReason": "Usage rate limited. Please try again later."}
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		if ranks[i].Priority != ranks[j].Priority {
			return ranks[i].Priority > ranks[j].Priority
		}
		if math.Abs(ranks[i].Score-ranks[j].Score) > 1e-9 {
			return ranks[i].Score > ranks[j].Score
		}
		if ranks[i].Owned != ranks[j].Owned {
			return ranks[i].Owned
		}
		return ranks[i].ID < ranks[j].ID
	})
	// All later affinity, deadline and spreading decisions stay in this eligible tier.
	for i := 1; i < len(ranks); i++ {
		if ranks[i].Priority != ranks[0].Priority {
			ranks = ranks[:i]
			break
		}
	}
	pk := affinityKey(h, meta, provider, model)
	// Schedule parallel work across eligible accounts before concentrating it
	// on one deadline score. No artificial concurrency quota or wait loop is added.
	minimumActive := ranks[0].ActiveRequests
	for _, r := range ranks[1:] {
		minimumActive = min(minimumActive, r.ActiveRequests)
	}
	best := ranks[0]
	for _, r := range ranks {
		if r.ActiveRequests == minimumActive {
			best = r
			break
		}
	}
	switchThreshold := 0.0
	pin, exists := state.Pins[pk]
	if exists && now.Sub(pin.At) < time.Hour {
		for _, r := range ranks {
			if r.ID != pin.ID {
				continue
			}
			boundAt := pin.BoundAt
			if boundAt.IsZero() {
				boundAt = pin.At
			}
			// Keep cache affinity only for similar urgency; never hold an account
			// for ten minutes while a substantially more urgent budget waits.
			warmth := cacheWarmth(pk, pin.ID, now)
			threshold := math.Max(ordinarySwitchAbsolute+(warmSwitchAbsolute-ordinarySwitchAbsolute)*warmth, (ordinarySwitchFraction+(warmSwitchFraction-ordinarySwitchFraction)*warmth)*math.Max(math.Abs(r.Score), .05))
			switchThreshold = threshold
			move := r.ActiveRequests > minimumActive || best.Known && r.Known && ((best.Expiring && !r.Expiring) || weeklyDeadlineRescue(best, r, now) || best.Score-r.Score > threshold)
			if !move {
				state.Pins[pk] = binding{ID: pin.ID, At: now, BoundAt: boundAt}
				reserveSelection(meta, pin.ID, now)
				reason := "affinity_similar_urgency"
				if warmth > 0 {
					reason = "warm_cache_similar_urgency"
				}
				recordDecision(p, h, meta, provider, model, pin.ID, pin.ID, reason, threshold, diagnostics, now)
				return map[string]any{"Handled": true, "AuthID": pin.ID}
			}
			break
		}
	}
	if len(state.Pins) > 10000 {
		for k, v := range state.Pins {
			if now.Sub(v.At) > time.Hour {
				delete(state.Pins, k)
			}
		}
	}
	state.Pins[pk] = binding{ID: best.ID, At: now, BoundAt: now}
	reserveSelection(meta, best.ID, now)
	reason := "best_deadline_score"
	if best.ID != ranks[0].ID {
		reason = "parallel_work_spread"
	}
	if exists && pin.ID != best.ID {
		reason = "previous_account_ineligible_or_unavailable"
		for _, r := range ranks {
			if r.ID == pin.ID {
				if r.ActiveRequests > minimumActive {
					reason = "parallel_work_spread"
					break
				}
				reason = "material_deadline_advantage"
				if weeklyDeadlineRescue(best, r, now) {
					reason = "weekly_deadline_at_risk"
				}
				if best.Expiring && !r.Expiring {
					reason = "imminent_five_hour_waste"
				}
				break
			}
		}
	}
	recordDecision(p, h, meta, provider, model, best.ID, pin.ID, reason, switchThreshold, diagnostics, now)
	return map[string]any{"Handled": true, "AuthID": best.ID}
}
