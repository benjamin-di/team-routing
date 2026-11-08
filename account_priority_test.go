package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPriorityAppliesAfterEligibilityBeforeAffinityAndSpreading(t *testing.T) {
	now := time.Now()
	p := protectedPolicy()
	p.AccountPriority = map[string]int{"a": 100, "b": 0}
	traffic.Lock()
	traffic.Active = map[string]activeRequest{}
	traffic.Pending = map[string]activeRequest{}
	traffic.Unlock()
	state.Pins = map[string]binding{}
	state.Quota = map[string]snapshot{"a": sample(now, .9, .9, time.Hour), "b": sample(now, .9, .9, time.Hour)}
	candidates := []candidate{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}
	// Establish affinity to B, then prefer A. Sticky routing cannot bypass it.
	if got := choose(p, "claude", "opus", nil, nil, candidates[1:], now)["AuthID"]; got != "b" {
		t.Fatal(got)
	}
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
		t.Fatal("affinity overrode priority", got)
	}
	// A busy preferred account still outranks an idle lower tier.
	traffic.Lock()
	traffic.Active["busy"] = activeRequest{Auth: "a", At: now}
	traffic.Unlock()
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
		t.Fatal("spreading crossed priority tier", got)
	}
	// A reserve hold always overrides priority and restores B's eligibility.
	state.Quota["a"] = sample(now, .9, .01, time.Hour)
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "b" {
		t.Fatal("priority bypassed reserve", got)
	}
	state.Quota["b"] = sample(now, 0, 0, time.Hour)
	if got := choose(p, "claude", "opus", nil, nil, candidates, now); got["Reject"] != true {
		t.Fatal("all-held pool served", got)
	}
	t.Cleanup(func() {
		traffic.Lock()
		traffic.Active = map[string]activeRequest{}
		traffic.Pending = map[string]activeRequest{}
		traffic.Unlock()
	})
}

func TestPriorityValidationAndPublicCopy(t *testing.T) {
	p := defaultPolicy()
	p.AccountPriority = map[string]int{"credential-a": -5}
	if err := validatePolicy(p); err != nil {
		t.Fatal(err)
	}
	public := publicPolicy(p, localSecrets{})
	public.AccountPriority["credential-a"] = 8
	if p.AccountPriority["credential-a"] != -5 {
		t.Fatal("public policy aliases saved map")
	}
	for _, raw := range []string{
		`{"account_priority":null}`,
		`{"account_priority":{"a":1.5}}`,
		`{"account_priority":{"a":null,"b":50}}`,
		`{"ACCOUNT_PRIORITY":{"a":null,"b":50}}`,
		`{"Account_Priority":{"a":null,"b":50}}`,
		`{"ACCOUNT_PRIORITY":null}`,
		`{"account_priority":{"a":null},"account_priority":{"b":50}}`,
		`{"account_priority":{"a":100},"ACCOUNT_PRIORITY":{"a":null}}`,
		`{"account_priority":{"a":null,"a":100}}`,
	} {
		var d dashboardPolicy
		if json.Unmarshal([]byte(raw), &d) == nil {
			t.Error("accepted invalid priority", raw)
		}
		var saved policy
		if json.Unmarshal([]byte(raw), &saved) == nil {
			t.Error("accepted invalid persisted priority", raw)
		}
	}
	for id, value := range map[string]int{" spaced": 0, "valid": maximumAccountPriority + 1} {
		if validateAccountPriority(map[string]int{id: value}) == nil {
			t.Fatal("invalid priority accepted", id, value)
		}
	}
}

func TestPriorityDecoderPreservesIntegerAndMapSemantics(t *testing.T) {
	for _, raw := range []string{
		`{"account_priority":{"a":0,"b":-5}}`,
		`{"ACCOUNT_PRIORITY":{"a":0,"b":-5}}`,
		`{"account_priority":{"a":0},"account_priority":{"b":-5}}`,
	} {
		var input dashboardPolicy
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			t.Fatal(err)
		}
		if len(input.AccountPriority) != 2 || input.AccountPriority["a"] != 0 || input.AccountPriority["b"] != -5 {
			t.Fatal("integer priorities changed", raw, input.AccountPriority)
		}
	}
}
