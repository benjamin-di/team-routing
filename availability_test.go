package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func inventoryFixture(t *testing.T, files []hostAuthFile, models []hostCredentialModels) {
	old := availabilityInventory
	availabilityInventory = func() ([]hostAuthFile, []hostCredentialModels, error) { return files, models, nil }
	t.Cleanup(func() { availabilityInventory = old })
}
func TestAvailabilityExhaustionReturns429WithoutSecrets(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	files := []hostAuthFile{}
	models := []hostCredentialModels{}
	state.Quota = map[string]snapshot{}
	for i := 0; i < 4; i++ {
		id := fmt.Sprint("claude-", i)
		files = append(files, hostAuthFile{ID: id, Provider: "claude", Email: "private@example.com"})
		models = append(models, hostCredentialModels{ID: id, Provider: "claude", Models: []string{"claude-opus-5-5"}})
		state.Quota[id] = sample(now, 0, .6, time.Hour)
	}
	inventoryFixture(t, files, models)
	r := availabilityPreflight(p, availabilityRequest{Model: "claude-opus-5-5", Headers: http.Header{"Authorization": {"Bearer secret-client"}}}, now)
	if r["StatusCode"] != 429 || r["Terminate"] != true || r["ResponseHeaders"].(http.Header).Get("Retry-After") != "300" {
		t.Fatal("missing capacity response/backoff", r)
	}
	raw := r["ResponseBody"].([]byte)
	if strings.Contains(string(raw), "private@example.com") || strings.Contains(string(raw), "secret-client") {
		t.Fatal("private identity exposed")
	}
	var body struct {
		Error struct {
			Type, Code string
			Reasons    map[string]int `json:"blocking_reasons"`
		}
	}
	if json.Unmarshal(raw, &body) != nil || body.Error.Type != "rate_limit_error" || body.Error.Code != "rate_limit_error" || len(body.Error.Reasons) != 0 {
		t.Fatal("wrong structured error", string(raw))
	}
	for _, internal := range []string{"account", "subscription", "routing", "reserve", "credit", "reset_at", "blocking_reasons", "quota_pending"} {
		if strings.Contains(strings.ToLower(string(raw)), internal) {
			t.Fatal("internal routing detail exposed", internal)
		}
	}
	state.Quota["claude-2"] = sample(now, .5, .6, time.Hour)
	if got := availabilityPreflight(p, availabilityRequest{Model: "claude-opus-5-5"}, now); len(got) != 0 {
		t.Fatal("available account did not reopen pool", got)
	}
}
func TestAvailabilityExactModelAndProviderOverride(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	files := []hostAuthFile{{ID: "native", Provider: "claude"}, {ID: "google", Provider: "antigravity"}}
	models := []hostCredentialModels{{ID: "native", Provider: "claude", Models: []string{"claude-opus-5-5", "claude-sonnet-4-6"}}, {ID: "google", Provider: "antigravity", Models: []string{"gemini-2.5-flash", "claude-sonnet-4-6"}}}
	inventoryFixture(t, files, models)
	state.Quota = map[string]snapshot{"native": sample(now, 0, .5, time.Hour), "google": {At: now, Groups: []group{{Name: "Claude and GPT models", Buckets: []bucket{{Window: "weekly", Remaining: .8, Reset: now.Add(7 * 24 * time.Hour)}}}, {Name: "Gemini Models", Buckets: []bucket{{Window: "weekly", Remaining: .9, Reset: now.Add(7 * 24 * time.Hour)}}}}}}
	if r := availabilityPreflight(p, availabilityRequest{Model: "claude-opus-5-5"}, now); r["StatusCode"] != 429 {
		t.Fatal("unrelated available models falsely rescued Opus", r)
	}
	if r := availabilityPreflight(p, availabilityRequest{Model: "claude-sonnet-4-6"}, now); len(r) != 0 {
		t.Fatal("matching Antigravity quota ignored", r)
	}
	p.Routes[digest("test-client")] = target{Provider: "antigravity", Model: "gemini-2.5-flash"}
	req := availabilityRequest{Model: "claude-opus-5-5", Headers: http.Header{"X-Api-Key": {"test-client"}}}
	if r := availabilityPreflight(p, req, now); len(r) != 0 {
		t.Fatal("per-key target model ignored", r)
	}
	p.Routes[digest("test-client")] = target{Provider: "claude", Model: "claude-opus-5-5"}
	if r := availabilityPreflight(p, req, now); r["StatusCode"] != 429 {
		t.Fatal("provider override escaped quota check", r)
	}
}
func TestAvailabilityDefersIncompleteInventory(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	inventoryFixture(t, nil, []hostCredentialModels{{ID: "unknown", Provider: "claude", Models: []string{"opus"}}})
	if r := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); len(r) != 0 {
		t.Fatal("invented a denial from incomplete inventory")
	}
	availabilityInventory = func() ([]hostAuthFile, []hostCredentialModels, error) {
		return nil, nil, fmt.Errorf("callback unavailable")
	}
	if r := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); len(r) != 0 {
		t.Fatal("callback failure blocked native routing")
	}
}

func TestConfiguredClaudeKeyWithKnownQuotaUsesPreflight429(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	now := time.Now()
	p := defaultPolicy()
	inventoryFixture(t, nil, []hostCredentialModels{{ID: "configured-claude", Provider: "claude", Models: []string{"opus"}}})
	state.Quota = map[string]snapshot{"configured-claude": sample(now, 0, .5, time.Hour)}
	r := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now)
	if r["StatusCode"] != 429 || r["Terminate"] != true {
		t.Fatal("known configured quota fell through to scheduler 500", r)
	}
}
func TestAvailabilityReservesFreshnessDisabledAndCredits(t *testing.T) {
	now := localTime("2026-11-02 12:00")
	p := protectedPolicy()
	files := []hostAuthFile{{ID: "shared", Provider: "claude"}, {ID: "disabled", Provider: "claude", Disabled: true}}
	models := []hostCredentialModels{{ID: "shared", Provider: "claude", Models: []string{"opus"}}, {ID: "disabled", Provider: "claude", Models: []string{"opus"}}}
	inventoryFixture(t, files, models)
	state.Quota = map[string]snapshot{"shared": sample(now, .2, .8, time.Hour), "disabled": sample(now, 1, 1, time.Hour)}
	if r := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); r["StatusCode"] != 429 || r["ResponseHeaders"].(http.Header).Get("Retry-After") != "30" {
		t.Fatal("reserve bypassed by disabled account", r)
	}
	state.Quota["shared"] = snapshot{}
	if r := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); r["StatusCode"] != 429 || !strings.Contains(string(r["ResponseBody"].([]byte)), "Usage rate limited") {
		t.Fatal("missing reading not explained", r)
	}
	q := sample(now, 0, .8, time.Hour)
	q.Groups[0].Buckets = append(q.Groups[0].Buckets, bucket{Window: "extra_usage", Remaining: 1})
	state.Quota["shared"] = q
	if r := availabilityPreflight(p, availabilityRequest{Model: "opus"}, now); r["StatusCode"] != 429 {
		t.Fatal("credits supplied eligibility")
	}
}

func TestAvailabilityThinkingSuffixUsesExactRegisteredPool(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	files := []hostAuthFile{{ID: "held", Provider: "claude"}, {ID: "other", Provider: "claude"}}
	models := []hostCredentialModels{{ID: "held", Provider: "claude", Models: []string{"claude-sonnet-4-6", "synthetic-alias"}}, {ID: "other", Provider: "claude", Models: []string{"claude-other"}}}
	inventoryFixture(t, files, models)
	state.Quota = map[string]snapshot{"held": sample(now, 0, .6, time.Hour), "other": sample(now, .9, .9, time.Hour)}
	for _, model := range []string{"claude-sonnet-4-6(high)", "synthetic-alias(high)", "claude-sonnet-4-6(8192)", "claude-sonnet-4-6(auto)"} {
		t.Run(model, func(t *testing.T) {
			r := availabilityPreflight(p, availabilityRequest{RequestedModel: model}, now)
			if r["StatusCode"] != 429 || r["ResponseHeaders"].(http.Header).Get("Retry-After") == "" {
				t.Fatalf("suffix lost capacity response: %v", r)
			}
		})
	}
	for _, model := range []string{"unknown(high)", "claude-sonnet-4-6(high", "claude-other(high)"} {
		if r := availabilityPreflight(p, availabilityRequest{RequestedModel: model}, now); len(r) != 0 {
			t.Fatalf("unrelated pool blocked for %s: %v", model, r)
		}
	}
	state.Quota["held"] = sample(now, .9, .9, time.Hour)
	if r := availabilityPreflight(p, availabilityRequest{RequestedModel: "synthetic-alias(high)"}, now); len(r) != 0 {
		t.Fatalf("available pool blocked: %v", r)
	}
}

func TestAvailabilityExactParenthesizedModelWins(t *testing.T) {
	now := time.Now()
	p := defaultPolicy()
	inventoryFixture(t, []hostAuthFile{{ID: "base", Provider: "claude"}, {ID: "literal", Provider: "claude"}}, []hostCredentialModels{{ID: "base", Provider: "claude", Models: []string{"model"}}, {ID: "literal", Provider: "claude", Models: []string{"model(high)"}}})
	state.Quota = map[string]snapshot{"base": sample(now, 0, .5, time.Hour), "literal": sample(now, .9, .9, time.Hour)}
	if r := availabilityPreflight(p, availabilityRequest{RequestedModel: "model(high)"}, now); len(r) != 0 {
		t.Fatalf("literal model ignored: %v", r)
	}
	state.Quota["base"], state.Quota["literal"] = state.Quota["literal"], state.Quota["base"]
	if r := availabilityPreflight(p, availabilityRequest{RequestedModel: "model(high)"}, now); r["StatusCode"] != 429 {
		t.Fatalf("literal model rescued by base model: %v", r)
	}
}
