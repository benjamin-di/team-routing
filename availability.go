package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

type availabilityRequest struct {
	Model, RequestedModel string
	Headers               http.Header
	Body                  []byte
	Metadata              map[string]any
}
type hostCredentialModels struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
}

// These callbacks read native in-memory inventory/registry, never provider
// quota APIs. Failure to establish the exact model pool defers to native routing.
var availabilityInventory = func() ([]hostAuthFile, []hostCredentialModels, error) {
	files, err := hostAuthList()
	if err != nil {
		return nil, nil, err
	}
	models, err := fetchHostModelInventory()
	return files, models, err
}

func availabilityPreflight(p policy, req availabilityRequest, now time.Time) map[string]any {
	pass := map[string]any{}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	provider := ""
	if route, ok, _ := resolveClientRoute(p, digest(key(req.Headers)), model); ok {
		model = route.Model
		provider = route.Provider
	} else if resolved := strings.TrimSpace(req.Model); resolved != "" {
		// The host has already resolved native selectors such as auto. Keep
		// the original selector above for client rules, then use its actual pool.
		model = resolved
	}
	if model == "" {
		return pass
	}
	files, models, err := availabilityInventory()
	if err != nil {
		return pass
	}
	model = availabilityModel(model, provider, models)
	inventory := map[string]hostAuthFile{}
	for _, f := range files {
		inventory[f.ID] = f
	}
	candidates := []candidate{}
	for _, entry := range models {
		if provider != "" && entry.Provider != provider {
			continue
		}
		matches := false
		for _, id := range entry.Models {
			if strings.EqualFold(id, model) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		f, ok := inventory[entry.ID]
		if !ok {
			// Config-backed credentials are in the runtime model registry but
			// not the auth-file inventory. A known quota snapshot is enough to
			// run their preflight; otherwise preserve the native fallback.
			state.Lock()
			_, known := state.Quota[entry.ID]
			state.Unlock()
			if !known {
				return pass
			}
			f = hostAuthFile{ID: entry.ID, Provider: entry.Provider}
		}
		if f.Disabled {
			continue
		}
		// Account-wide status can reflect a different model's last failure. The
		// native scheduler applies the actual model cooldown after this quota check.
		candidates = append(candidates, candidate{ID: entry.ID, Provider: entry.Provider, Metadata: map[string]any{"email": f.Email}})
	}
	if len(candidates) == 0 {
		return pass
	}
	counts := activeCounts(now)
	reasons := map[string]int{}
	earliest := time.Time{}
	allExhausted := true
	state.Lock()
	for _, c := range candidates {
		q := state.Quota[c.ID]
		if _, eligible := rankAccount(p, c, model, q, now, counts[c.ID]); eligible {
			state.Unlock()
			return pass
		}
		exhausted := false
		for _, g := range q.Groups {
			if !relevantGroup(c.Provider, model, g.Name) {
				continue
			}
			for _, b := range g.Buckets {
				if !relevantQuotaBucket(c.Provider, model, b.Window) {
					continue
				}
				if b.Window == "extra_usage" || b.Remaining > 0 {
					continue
				}
				exhausted = true
				if b.Reset.After(now) && (earliest.IsZero() || b.Reset.Before(earliest)) {
					earliest = b.Reset
				}
			}
		}
		reason := "reserve_or_projected_usage"
		if creditSafetyReason(p, c, q, now) != "" {
			reason = "paid_usage_guard"
			allExhausted = false
		} else if exhausted {
			reason = "included_quota_exhausted"
		} else {
			allExhausted = false
			if q.At.IsZero() || now.Sub(q.At) >= time.Duration(p.MaxAge)*time.Second || (sharedSettings(p, c) != nil && now.Sub(q.At) > time.Duration(p.Poll+quotaAgeGraceSeconds)*time.Second) {
				reason = "fresh_quota_pending"
			}
		}
		reasons[reason]++
	}
	state.Unlock()
	retry := 30
	if allExhausted {
		retry = int(clamp(float64(p.Poll), 30, 300))
		if !earliest.IsZero() {
			retry = int(math.Min(float64(retry), math.Max(15, math.Ceil(earliest.Sub(now).Seconds()))))
		}
	}
	private := map[string]any{"at": now, "model": model, "blocking_reasons": reasons, "retry_after_seconds": retry}
	if !earliest.IsZero() {
		private["earliest_observed_reset_at"] = earliest.UTC().Format(time.RFC3339)
	}
	if p.RecordRouting {
		appendEvidence("team-routing-rejections.jsonl", private)
	}
	details := map[string]any{"type": "rate_limit_error", "code": "rate_limit_error", "message": "Usage rate limited. Please try again later.", "retry_after_seconds": retry}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": details})
	return map[string]any{"Terminate": true, "StatusCode": 429, "ResponseHeaders": http.Header{"Content-Type": {"application/json"}, "Retry-After": {fmt.Sprint(retry)}}, "ResponseBody": body}
}

// Match the host's model(value) selection syntax while preserving any exact
// registered model ID, including a literal parenthesized model name.
func availabilityModel(model, provider string, inventory []hostCredentialModels) string {
	for _, entry := range inventory {
		if provider != "" && entry.Provider != provider {
			continue
		}
		for _, id := range entry.Models {
			if strings.EqualFold(id, model) {
				return id
			}
		}
	}
	if i := strings.LastIndex(model, "("); i > 0 && strings.HasSuffix(model, ")") {
		return model[:i]
	}
	return model
}
