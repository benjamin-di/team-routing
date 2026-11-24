package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func priorityPolicy(t *testing.T, p policy, values map[string]int) policy {
	t.Helper()
	data := jsonObject(p)
	if values != nil {
		data["account_priority"] = values
	}
	return policyFromObject(t, data)
}

func priorityFixture(t *testing.T) (policy, []candidate, time.Time) {
	t.Helper()
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	now := localTime("2026-11-02 14:00")
	p := protectedPolicy()
	p.Owned = nil
	resetTraffic := func() {
		traffic.Lock()
		traffic.Active = map[string]activeRequest{}
		traffic.Pending = map[string]activeRequest{}
		traffic.Unlock()
	}
	resetTraffic()
	t.Cleanup(resetTraffic)
	state.Pins = map[string]binding{}
	state.Quota = map[string]snapshot{"a": sample(now, .9, .9, time.Hour), "b": sample(now, .9, .9, time.Hour)}
	return p, []candidate{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}, now
}

func TestPriorityAppliesAfterEligibilityBeforeAffinityAndSpreading(t *testing.T) {
	p, candidates, now := priorityFixture(t)
	// Establish a warm binding, then change preference without restarting.
	if got := choose(p, "claude", "opus", nil, nil, candidates[1:], now)["AuthID"]; got != "b" {
		t.Fatal(got)
	}
	p = priorityPolicy(t, p, map[string]int{"a": 100})
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
		t.Fatal("affinity overrode priority", got)
	}
	traffic.Lock()
	traffic.Active["busy"] = activeRequest{Auth: "a", At: now}
	traffic.Unlock()
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
		t.Fatal("spreading crossed priority tier", got)
	}
	state.Quota["a"] = sample(now, .9, .01, time.Hour)
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "b" {
		t.Fatal("priority bypassed reserve", got)
	}
	state.Quota["b"] = sample(now, 0, 0, time.Hour)
	if got := choose(p, "claude", "opus", nil, nil, candidates, now); got["Reject"] != true {
		t.Fatal("all-held pool served", got)
	}
}

func TestPriorityCannotBypassEligibility(t *testing.T) {
	for _, reason := range []string{"five-hour reserve", "weekly reserve", "stale", "billing enabled", "billing unknown", "exhausted", "paid fence"} {
		t.Run(reason, func(t *testing.T) {
			p, candidates, now := priorityFixture(t)
			p = priorityPolicy(t, p, map[string]int{"a": 1000000, "b": -1000000})
			q := sample(now, .9, .9, time.Hour)
			switch reason {
			case "five-hour reserve":
				q.Groups[0].Buckets[0].Remaining = .1
			case "weekly reserve":
				q.Groups[0].Buckets[1].Remaining = .01
			case "stale":
				q.At = now.Add(-time.Hour)
				q.RequestedAt = q.At
			case "billing enabled":
				on := true
				q.ExtraUsage.Enabled = &on
			case "billing unknown":
				q.ExtraUsage = nil
			case "exhausted":
				q.Groups[0].Buckets[0].Remaining = 0
			case "paid fence":
				creditFences.Lock()
				loadCreditFences()
				creditFences.Denied["a"] = now
				creditFences.Unlock()
			}
			state.Quota["a"] = q
			if _, ok := rankAccount(p, candidates[0], "opus", q, now, 0); ok {
				t.Fatal("fixture must be ineligible")
			}
			if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "b" {
				t.Fatal("priority bypassed eligibility", got)
			}
		})
	}
}

func TestPriorityDefaultsAndEqualTiersKeepDynamicRanking(t *testing.T) {
	for _, values := range []map[string]int{nil, {}, {"a": 0, "b": 0}, {"a": 5, "b": 5}} {
		t.Run(fmt.Sprint(values), func(t *testing.T) {
			p, candidates, now := priorityFixture(t)
			p = priorityPolicy(t, p, values)
			state.Quota["a"] = sample(now, .8, .3, 3*time.Hour)
			state.Quota["b"] = sample(now, .9, .9, time.Hour)
			if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "b" {
				t.Fatal("dynamic ranking changed", got)
			}
			traffic.Lock()
			traffic.Active["busy"] = activeRequest{Auth: "b", At: now}
			traffic.Unlock()
			if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
				t.Fatal("equal-tier spreading changed", got)
			}
		})
	}
}

func TestPriorityOrdersEligibleAccountsBeforeDynamicRanking(t *testing.T) {
	p, candidates, now := priorityFixture(t)
	state.Quota["a"] = sample(now, .8, .3, 3*time.Hour)
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "b" {
		t.Fatal("fixture must dynamically prefer b", got)
	}
	p = priorityPolicy(t, p, map[string]int{"a": 1, "b": -1})
	if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
		t.Fatal("priority did not order eligible accounts", got)
	}
}

func TestPriorityValidationRejectsEveryInvalidOccurrence(t *testing.T) {
	for _, raw := range []string{
		`{"account_priority":null}`,
		`{"ACCOUNT_PRIORITY":null}`,
		`{"account_priority":[]}`,
		`{"account_priority":{"a":null,"b":50}}`,
		`{"Account_Priority":{"a":null}}`,
		`{"account_priority":{"a":1.5}}`,
		`{"account_priority":{"a":1.0}}`,
		`{"account_priority":{"a":1e2}}`,
		`{"account_priority":{"a":"1"}}`,
		`{"account_priority":{"a":true}}`,
		`{"account_priority":{"a":1000001}}`,
		`{"account_priority":{"a":-1000001}}`,
		`{"account_priority":{"a":9223372036854775808}}`,
		`{"account_priority":{"a":null},"account_priority":{"b":50}}`,
		`{"account_priority":{"a":100},"ACCOUNT_PRIORITY":{"a":null}}`,
		`{"account_priority":{"a":null,"a":100}}`,
		`{"account_priority":{"a":1000001,"a":100}}`,
		`{"account_priority":{"a":1.5},"account_priority":{"a":100}}`,
		`{"account_priority":{"a":1000001},"Account_Priority":{"a":100}}`,
		`{"account_priority":{"":1}}`,
		`{"account_priority":{" spaced":1}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var input dashboardPolicy
			if err := json.Unmarshal([]byte(raw), &input); err == nil {
				p := priorityPolicy(t, defaultPolicy(), map[string]int{})
				if json.Unmarshal([]byte(raw), &p) == nil && validatePolicy(p) == nil {
					t.Error("accepted invalid dashboard priority")
				}
			}
			p := defaultPolicy()
			if json.Unmarshal([]byte(raw), &p) == nil && validatePolicy(p) == nil {
				t.Error("accepted invalid persisted priority")
			}
		})
	}
}

func TestPriorityDecoderPreservesIntegerAndMapSemantics(t *testing.T) {
	for _, raw := range []string{
		`{"account_priority":{"a":0,"b":-5}}`,
		`{"ACCOUNT_PRIORITY":{"a":0,"b":-5}}`,
		`{"account_priority":{"a":0},"account_priority":{"b":-5}}`,
		`{"account_priority":{"a":1},"Account_Priority":{"a":0,"b":-5}}`,
		`{"account_priority":{"a":5,"a":0,"b":-5}}`,
	} {
		var input dashboardPolicy
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			t.Fatal(err)
		}
		var saved policy
		if err := json.Unmarshal([]byte(raw), &saved); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"a": float64(0), "b": float64(-5)}
		for _, value := range []any{input, saved} {
			if got := jsonObject(value)["account_priority"]; !reflect.DeepEqual(got, want) {
				t.Fatal("integer priorities changed", raw, got)
			}
		}
	}
}

func TestPriorityPublicCopyAndPolicyBounds(t *testing.T) {
	p := priorityPolicy(t, defaultPolicy(), map[string]int{"a": -1000000, "b": 1000000})
	if err := validatePolicy(p); err != nil {
		t.Fatal(err)
	}
	public := publicPolicy(p, localSecrets{})
	priorities := reflect.ValueOf(public).FieldByName("AccountPriority")
	if !priorities.IsValid() || priorities.IsNil() {
		t.Fatal("public policy omitted account priorities")
	}
	priorities.SetMapIndex(reflect.ValueOf("a"), reflect.ValueOf(8))
	if got := jsonObject(p)["account_priority"]; !reflect.DeepEqual(got, map[string]any{"a": float64(-1000000), "b": float64(1000000)}) {
		t.Fatal("public policy changed saved priorities", got)
	}
	for _, values := range []map[string]int{{"a": 1000001}, {"a": -1000001}, {"": 1}, {" spaced": 1}} {
		data := jsonObject(defaultPolicy())
		data["account_priority"] = values
		raw, _ := json.Marshal(data)
		var bad policy
		if json.Unmarshal(raw, &bad) == nil && validatePolicy(bad) == nil {
			t.Fatal("invalid priorities accepted", values)
		}
	}
}

func TestPriorityDashboardPolicyAPIRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	// Keep this API test synchronous; quota monitoring is independent.
	state.Running = true
	oldKeys, oldHost := fetchActiveClientKeys, hostCall
	fetchActiveClientKeys = func(...managementCaller) ([]string, error) { return []string{"synthetic-client"}, nil }
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return []byte(`{"files":[{"id":"a","provider":"claude"},{"id":"b","provider":"codex"}]}`), nil
		}
		return nil, fmt.Errorf("unexpected host call: %s", method)
	}
	t.Cleanup(func() { stopWorker(); applyConfig(nil); fetchActiveClientKeys = oldKeys; hostCall = oldHost })
	p := defaultPolicy()
	writePolicyFixture(t, p)
	clients := localSecrets{Clients: []struct{ Name, Key string }{{"test", "synthetic-client"}}}
	input := jsonObject(publicPolicy(p, clients))
	put := func(values map[string]any, status int) dashboardPolicy {
		t.Helper()
		raw, _ := json.Marshal(values)
		response := handleDashboard(dashboardRequest{Method: "PUT", Path: dashboardAPI + "/policy", Body: raw})
		if response.StatusCode != status {
			t.Fatalf("save: got %d want %d: %s", response.StatusCode, status, response.Body)
		}
		var body struct {
			Policy dashboardPolicy `json:"policy"`
		}
		if err := json.Unmarshal(response.Body, &body); err != nil {
			t.Fatal(err)
		}
		return body.Policy
	}
	want := map[string]any{"a": float64(20), "b": float64(-5)}
	input["account_priority"] = want
	saved := put(input, 200)
	got, err := readPolicy()
	if err != nil || !reflect.DeepEqual(jsonObject(got)["account_priority"], want) || !reflect.DeepEqual(jsonObject(saved)["account_priority"], want) {
		t.Fatal("priority did not round-trip through API and disk", err)
	}
	input = jsonObject(saved)
	delete(input, "account_priority")
	saved = put(input, 200)
	if !reflect.DeepEqual(jsonObject(saved)["account_priority"], want) {
		t.Fatal("older client erased priority")
	}
	for _, invalid := range []any{nil, map[string]any{"a": nil}, map[string]any{"a": 1.5}, map[string]any{"a": 1000001}, map[string]any{"unknown": 10}} {
		input = jsonObject(saved)
		input["account_priority"] = invalid
		put(input, 400)
		got, err = readPolicy()
		if err != nil || !reflect.DeepEqual(jsonObject(got)["account_priority"], want) {
			t.Fatal("rejected save changed policy", err)
		}
	}
	// Disconnected rules remain editable; new unknown credential IDs are rejected.
	hostCall = func(string, any) (json.RawMessage, error) { return []byte(`{"files":[]}`), nil }
	input = jsonObject(saved)
	input["account_priority"] = map[string]int{"a": 21, "b": -5}
	saved = put(input, 200)
	input = jsonObject(saved)
	input["account_priority"] = map[string]int{}
	saved = put(input, 200)
	got, err = readPolicy()
	if err != nil || jsonObject(got)["account_priority"] != nil || jsonObject(saved)["account_priority"] != nil {
		t.Fatal("explicit empty map did not clear priority", err)
	}
}

func lastDecisionReason(t *testing.T) string {
	t.Helper()
	evidence.Lock()
	defer evidence.Unlock()
	if len(evidence.Recent) == 0 {
		t.Fatal("no decision recorded")
	}
	return evidence.Recent[len(evidence.Recent)-1].Reason
}

func TestAccountPriorityDecisionReason(t *testing.T) {
	t.Run("pinned_lower_tier", func(t *testing.T) {
		p, candidates, now := priorityFixture(t)
		if got := choose(p, "claude", "opus", nil, nil, candidates[1:], now)["AuthID"]; got != "b" {
			t.Fatalf("setup picked %v", got)
		}
		p = priorityPolicy(t, p, map[string]int{"a": 100})
		if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
			t.Fatalf("picked %v", got)
		}
		if reason := lastDecisionReason(t); reason != "higher_account_priority" {
			t.Fatalf("reason=%q", reason)
		}
	})
	t.Run("fresh_lower_score", func(t *testing.T) {
		p, candidates, now := priorityFixture(t)
		state.Quota["a"] = sample(now, .8, .3, 3*time.Hour)
		p = priorityPolicy(t, p, map[string]int{"a": 1})
		if got := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]; got != "a" {
			t.Fatalf("picked %v", got)
		}
		if reason := lastDecisionReason(t); reason != "higher_account_priority" {
			t.Fatalf("reason=%q", reason)
		}
	})
}

func TestAccountPriorityReasonWhenPriorityChangesPick(t *testing.T) {
	for _, scenario := range []string{"equal_score_tiebreak", "parallel_spreading", "retained_affinity"} {
		t.Run(scenario, func(t *testing.T) {
			p, candidates, now := priorityFixture(t)
			pk := affinityKey(nil, nil, "claude", "opus")
			var initialPin binding
			switch scenario {
			case "equal_score_tiebreak":
				// The equal-score ID tiebreak normally picks a; priority prefers b.
			case "parallel_spreading":
				state.Quota["a"] = sample(now, .8, .3, 3*time.Hour)
				traffic.Lock()
				traffic.Active["busy-b"] = activeRequest{Auth: "b", At: now}
				traffic.Unlock()
			case "retained_affinity":
				state.Quota["b"] = sample(now, .8, .3, 3*time.Hour)
				initialPin = binding{ID: "b", At: now, BoundAt: now}
				state.Pins[pk] = initialPin
			}
			baseline := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]
			baselineReason := lastDecisionReason(t)
			state.Pins = map[string]binding{}
			if initialPin.ID != "" {
				state.Pins[pk] = initialPin
			}
			p = priorityPolicy(t, p, map[string]int{"b": 1})
			selected := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]
			reason := lastDecisionReason(t)
			t.Logf("without priority: selected=%v reason=%s; with b=1: selected=%v reason=%s", baseline, baselineReason, selected, reason)
			if baseline != "a" || selected != "b" {
				t.Fatal("fixture did not make priority decide the selection")
			}
			if reason != "higher_account_priority" {
				t.Errorf("reason=%q; want higher_account_priority because only priority changed the selected account", reason)
			}
		})
	}
}

func TestAccountPriorityReasonWhenPickUnchanged(t *testing.T) {
	p, candidates, now := priorityFixture(t)
	state.Quota["b"] = sample(now, .8, .3, 3*time.Hour)
	pk := affinityKey(nil, nil, "claude", "opus")
	initialPin := binding{ID: "b", At: now, BoundAt: now}
	state.Pins[pk] = initialPin
	baseline := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]
	baselineReason := lastDecisionReason(t)
	state.Pins[pk] = initialPin
	p = priorityPolicy(t, p, map[string]int{"a": 1})
	selected := choose(p, "claude", "opus", nil, nil, candidates, now)["AuthID"]
	reason := lastDecisionReason(t)
	t.Logf("without priority: selected=%v reason=%s; with a=1: selected=%v reason=%s", baseline, baselineReason, selected, reason)
	if baseline != "a" || selected != baseline {
		t.Fatal("fixture should switch from b to a with or without priority")
	}
	if reason != baselineReason {
		t.Errorf("reason=%q; want %q because priority did not decide the pick", reason, baselineReason)
	}
}

func TestAccountPriorityAbsentFromJournalWhenUnset(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	now := localTime("2026-11-02 14:00")
	p := protectedPolicy()
	p.Owned = nil
	state.Pins = map[string]binding{}
	state.Quota = map[string]snapshot{"a": sample(now, .9, .9, time.Hour)}
	choose(p, "claude", "opus", nil, nil, []candidate{{ID: "a", Provider: "claude"}}, now)
	evidence.Lock()
	raw, _ := json.Marshal(evidence.Recent[len(evidence.Recent)-1].Candidates[0])
	evidence.Unlock()
	if strings.Contains(string(raw), `"Priority"`) {
		t.Fatalf("unset priority changed the journal: %s", raw)
	}
}

func TestAccountPriorityThresholdMatchesReason(t *testing.T) {
	p, candidates, now := priorityFixture(t)
	state.Quota["b"] = sample(now, .8, .3, 3*time.Hour)
	pk := affinityKey(nil, nil, "claude", "opus")
	pin := binding{ID: "b", At: now, BoundAt: now}
	last := func() selectionEvidence {
		evidence.Lock()
		defer evidence.Unlock()
		return evidence.Recent[len(evidence.Recent)-1]
	}
	state.Pins[pk] = pin
	choose(p, "claude", "opus", nil, nil, candidates, now)
	base := last()
	state.Pins[pk] = pin
	p = priorityPolicy(t, p, map[string]int{"a": 1})
	choose(p, "claude", "opus", nil, nil, candidates, now)
	got := last()
	t.Logf("no priority: auth=%s previous=%s reason=%s threshold=%v", base.Auth, base.Previous, base.Reason, base.Threshold)
	t.Logf("a=1:         auth=%s previous=%s reason=%s threshold=%v", got.Auth, got.Previous, got.Reason, got.Threshold)
	if got.Reason == base.Reason && got.Threshold != base.Threshold {
		t.Errorf("recorded reason %q comes from the no-priority pick but threshold %v does not (that pick used %v)", got.Reason, got.Threshold, base.Threshold)
	}
}

func TestModelProviderPriorityOverridesGlobalOrderPerModel(t *testing.T) {
	p, _, now := priorityFixture(t)
	candidates := []candidate{
		{ID: "claude-a", Provider: "claude"},
		{ID: "codex-a", Provider: "codex"},
		{ID: "ag-a", Provider: "antigravity"},
	}
	state.Quota = map[string]snapshot{
		"claude-a": sample(now, .9, .9, time.Hour),
		"codex-a":  sample(now, .9, .9, time.Hour),
		"ag-a":     sample(now, .9, .9, time.Hour),
	}
	data := jsonObject(p)
	data["account_priority"] = map[string]int{"claude-a": 0, "codex-a": -1, "ag-a": -2}
	data["model_provider_priority"] = map[string]map[string]int{
		"alias-value": {"codex": 0, "claude": -1, "antigravity": -2},
	}
	p = policyFromObject(t, data)

	// Unmatched model keeps global account_priority (claude > codex > antigravity).
	if got := choose(p, "", "alias-daily", nil, nil, candidates, now)["AuthID"]; got != "claude-a" {
		t.Fatalf("alias-daily selected %v, want claude-a", got)
	}
	// Configured model uses model_provider_priority (codex > claude > antigravity).
	if got := choose(p, "", "alias-value", nil, nil, candidates, now)["AuthID"]; got != "codex-a" {
		t.Fatalf("alias-value selected %v, want codex-a", got)
	}
	// Exhausting codex falls back to claude (second tier), not antigravity.
	state.Quota["codex-a"] = sample(now, 0, 0, time.Hour)
	if got := choose(p, "", "alias-value", nil, nil, candidates, now)["AuthID"]; got != "claude-a" {
		t.Fatalf("alias-value fallback selected %v, want claude-a", got)
	}
	// Exhausting claude too falls back to antigravity (third tier).
	state.Quota["claude-a"] = sample(now, 0, 0, time.Hour)
	if got := choose(p, "", "alias-value", nil, nil, candidates, now)["AuthID"]; got != "ag-a" {
		t.Fatalf("alias-value third-tier fallback selected %v, want ag-a", got)
	}
}

func TestModelProviderPriorityValidationAndSchema(t *testing.T) {
	schema := policySchema()
	props := schema["properties"].(map[string]any)
	if _, ok := props["model_provider_priority"]; !ok {
		t.Fatal("model_provider_priority missing from policySchema")
	}
	base := protectedPolicy()
	if err := validatePolicy(base); err != nil {
		t.Fatalf("base fixture must be valid: %v", err)
	}
	for name, val := range map[string]any{
		"null root":         nil,
		"null model map":    map[string]any{"m": nil},
		"non-integer value": map[string]any{"m": map[string]any{"codex": 1.5}},
		"out of range":      map[string]any{"m": map[string]any{"codex": 1000001}},
		"empty model key":   map[string]any{"": map[string]any{"codex": 0}},
		"padded model key":  map[string]any{" m ": map[string]any{"codex": 0}},
		"nel model key":     map[string]any{"m\u0085": map[string]any{"codex": 0}},
		"bom model key":     map[string]any{"m\ufeff": map[string]any{"codex": 0}},
		"bad provider key":  map[string]any{"m": map[string]any{"bad provider": 0}},
		"nel provider key":  map[string]any{"m": map[string]any{"codex\u0085": 0}},
		"bom provider key":  map[string]any{"m": map[string]any{"codex\ufeff": 0}},
		"long provider key": map[string]any{"m": map[string]any{strings.Repeat("a", 65): 0}},
	} {
		t.Run(name, func(t *testing.T) {
			obj := jsonObject(base)
			obj["model_provider_priority"] = val
			raw, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			var p policy
			err = json.Unmarshal(raw, &p)
			if err == nil {
				err = validatePolicy(p)
			}
			if err == nil {
				t.Fatalf("expected error for %s", name)
			}
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "priority") && !strings.Contains(msg, "int") {
				t.Fatalf("unexpected unrelated error for %s: %v", name, err)
			}
		})
	}
	// Valid empty object clears cleanly.
	data := jsonObject(base)
	data["model_provider_priority"] = map[string]map[string]int{}
	_ = policyFromObject(t, data)
}

func TestModelProviderPriorityDashboardRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	state.Running = true
	oldKeys, oldHost := fetchActiveClientKeys, hostCall
	fetchActiveClientKeys = func(...managementCaller) ([]string, error) { return []string{"synthetic-client"}, nil }
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return []byte(`{"files":[{"id":"a","provider":"claude"},{"id":"b","provider":"codex"}]}`), nil
		}
		return nil, fmt.Errorf("unexpected host call: %s", method)
	}
	t.Cleanup(func() { stopWorker(); applyConfig(nil); fetchActiveClientKeys = oldKeys; hostCall = oldHost })
	p := defaultPolicy()
	writePolicyFixture(t, p)
	clients := localSecrets{Clients: []struct{ Name, Key string }{{"test", "synthetic-client"}}}
	put := func(values map[string]any, status int) dashboardPolicy {
		t.Helper()
		raw, _ := json.Marshal(values)
		response := handleDashboard(dashboardRequest{Method: "PUT", Path: dashboardAPI + "/policy", Body: raw})
		if response.StatusCode != status {
			t.Fatalf("save: got %d want %d: %s", response.StatusCode, status, response.Body)
		}
		var body struct {
			Policy dashboardPolicy `json:"policy"`
		}
		if err := json.Unmarshal(response.Body, &body); err != nil {
			t.Fatal(err)
		}
		return body.Policy
	}
	want := map[string]any{"alias-value": map[string]any{"codex": float64(0), "claude": float64(-1)}}
	input := jsonObject(publicPolicy(p, clients))
	input["model_provider_priority"] = want
	saved := put(input, 200)
	got, err := readPolicy()
	if err != nil || !reflect.DeepEqual(jsonObject(got)["model_provider_priority"], want) || !reflect.DeepEqual(jsonObject(saved)["model_provider_priority"], want) {
		t.Fatal("model_provider_priority did not round-trip", err)
	}
	// Omitting field in an older client preserves existing rules.
	input = jsonObject(saved)
	delete(input, "model_provider_priority")
	saved = put(input, 200)
	if !reflect.DeepEqual(jsonObject(saved)["model_provider_priority"], want) {
		t.Fatal("omitted model_provider_priority erased saved rules")
	}
	// Invalid payloads fail 400 without modifying disk.
	for _, invalid := range []any{nil, map[string]any{"m": nil}, map[string]any{"m": map[string]any{"codex": 1.5}}, map[string]any{"m": map[string]any{"codex": 1000001}}} {
		input = jsonObject(saved)
		input["model_provider_priority"] = invalid
		put(input, 400)
		got, err = readPolicy()
		if err != nil || !reflect.DeepEqual(jsonObject(got)["model_provider_priority"], want) {
			t.Fatal("rejected save modified model_provider_priority", err)
		}
	}
	// Explicit empty map clears rules.
	input = jsonObject(saved)
	input["model_provider_priority"] = map[string]any{}
	saved = put(input, 200)
	got, err = readPolicy()
	if err != nil || jsonObject(got)["model_provider_priority"] != nil || jsonObject(saved)["model_provider_priority"] != nil {
		t.Fatal("explicit empty map did not clear model_provider_priority", err)
	}
}

func TestEvaluateRouteExposesExplicitZeroModelProviderPriority(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	oldHost, oldModels := hostCall, fetchHostModelInventory
	now := time.Now()
	state.Lock()
	state.Quota = map[string]snapshot{
		"a": sample(now, .9, .9, time.Hour),
		"b": sample(now, .9, .9, time.Hour),
	}
	state.Unlock()
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return []byte(`{"files":[{"id":"a","name":"a.json","provider":"claude"},{"id":"b","name":"b.json","provider":"codex"},{"id":"c","name":"c.json","provider":"claude","disabled":true}]}`), nil
		}
		return nil, fmt.Errorf("unexpected host call: %s", method)
	}
	fetchHostModelInventory = func() ([]hostCredentialModels, error) {
		return []hostCredentialModels{
			{ID: "a", Provider: "claude", Models: []string{"alias-value"}},
			{ID: "b", Provider: "codex", Models: []string{"alias-value"}},
			{ID: "c", Provider: "claude", Models: []string{"alias-value"}},
		}, nil
	}
	t.Cleanup(func() { applyConfig(nil); hostCall = oldHost; fetchHostModelInventory = oldModels })
	p := protectedPolicy()
	data := jsonObject(p)
	data["account_priority"] = map[string]int{"a": 10, "b": -1, "c": 10}
	data["model_provider_priority"] = map[string]map[string]int{"alias-value": {"claude": 0, "codex": 5}}
	writePolicyFixture(t, policyFromObject(t, data))

	raw, _ := json.Marshal(routeEvaluationInput{Model: "alias-value"})
	resp := handleDashboard(dashboardRequest{Method: "POST", Path: dashboardAPI + "/evaluate", Body: raw})
	if resp.StatusCode != 200 {
		t.Fatalf("evaluate failed: %d %s", resp.StatusCode, resp.Body)
	}
	var out struct {
		Candidates []map[string]any `json:"candidates"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]float64{}
	for _, row := range out.Candidates {
		id, _ := row["account_id"].(string)
		prio, ok := row["priority"].(float64)
		if !ok {
			t.Fatalf("candidate %s missing explicit priority field: %v", id, row)
		}
		seen[id] = prio
	}
	if seen["a"] != 0 || seen["b"] != 5 || seen["c"] != 0 {
		t.Fatalf("unexpected evaluated priorities: %v", seen)
	}
}
