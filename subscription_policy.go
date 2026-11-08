package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type windowReserve struct {
	Group          string  `json:"group"`
	Window         string  `json:"window"`
	Floor          float64 `json:"remaining_floor"`
	ReleaseMinutes int     `json:"release_within_minutes"`
}

const maximumWindowReleaseMinutes = 10080

func windowReserveFields() []policyField {
	return []policyField{
		{Path: "remaining_floor", Type: "number", Title: "Remaining reserve", Description: "Percentage to keep for this native quota window.", Minimum: bound(0), Maximum: bound(maximumReserveFraction), Format: "percent"},
		{Path: "release_within_minutes", Type: "integer", Title: "Release before reset (minutes)", Description: "0 always preserves the reserve; a positive value unlocks it before a confirmed reset.", Minimum: bound(0), Maximum: bound(maximumWindowReleaseMinutes)},
	}
}
func quotaFeatures(provider string, q snapshot) map[string]bool {
	out := map[string]bool{"five_hour": len(q.Groups) == 0 && (provider == "claude" || provider == "codex"), "weekly": len(q.Groups) == 0 && (provider == "claude" || provider == "codex")}
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			if b.Window == "five_hour" {
				out["five_hour"] = true
			}
			if weeklyWindow(b.Window) {
				out["weekly"] = true
			}
		}
	}
	return out
}

func quotaWindowKey(group, window string) string { return digest(group + "\x00" + window) }
func quotaSupported(provider string) bool {
	return provider == "claude" || provider == "codex" || provider == "antigravity"
}
func providerSettings(p policy, provider string) *sharedClaudePolicy {
	if provider == "claude" {
		return p.SharedClaude
	}
	return p.ProviderReserves[provider]
}
func ownedAccount(p policy, c candidate) bool {
	if owned, ok := p.AccountOwnership[c.ID]; ok {
		return owned
	}
	return legacyOwnedClaude(p, c)
}
func reserveForBucket(p policy, c candidate, group string, b bucket, now time.Time) float64 {
	s := sharedSettings(p, c)
	if s == nil {
		return 0
	}
	rule, ok := p.WindowReserves[c.ID][quotaWindowKey(group, b.Window)]
	if !ok {
		// Preserve explicitly configured legacy native-window rules after aliases
		// such as Antigravity's 5h become canonical five_hour windows.
		floor, found := 0.0, false
		for _, legacy := range p.WindowReserves[c.ID] {
			if legacy.Group == group && normalizedQuotaWindow(c.Provider, legacy.Window, "") == b.Window {
				found = true
				if legacy.ReleaseMinutes > 0 && b.Reset.After(now) && b.Reset.Sub(now) <= time.Duration(legacy.ReleaseMinutes)*time.Minute {
					continue
				}
				floor = max(floor, legacy.Floor)
			}
		}
		if found {
			return floor
		}
	}
	if ok {
		if rule.ReleaseMinutes > 0 && b.Reset.After(now) && b.Reset.Sub(now) <= time.Duration(rule.ReleaseMinutes)*time.Minute {
			return 0
		}
		return rule.Floor
	}
	return reserveFloor(p, c, b, now)
}
func validateSubscriptionPolicy(p policy) error {
	if len(p.AccountOwnership) > 500 || len(p.ProviderReserves) > 100 || len(p.WindowReserves) > 500 {
		return fmt.Errorf("too many subscription policy entries")
	}
	for id := range p.AccountOwnership {
		if strings.TrimSpace(id) != id || id == "" || len(id) > 512 {
			return fmt.Errorf("invalid ownership identity")
		}
	}
	for provider, s := range p.ProviderReserves {
		if provider == "" || provider == "claude" || len(provider) > 128 || strings.TrimSpace(provider) != provider {
			return fmt.Errorf("invalid provider policy; Claude uses its existing settings field")
		}
		if s == nil {
			return fmt.Errorf("provider reserve settings must be an object")
		}
		if err := validateSharedPolicy(s); err != nil {
			return err
		}
	}
	for id, rules := range p.WindowReserves {
		if id == "" || strings.TrimSpace(id) != id || len(id) > 512 || len(rules) > 100 {
			return fmt.Errorf("invalid account quota-window rules")
		}
		for key, r := range rules {
			if r.Group == "" || r.Window == "" || len(r.Group) > 256 || len(r.Window) > 256 || key != quotaWindowKey(r.Group, r.Window) {
				return fmt.Errorf("quota-window rule does not match its group/window identity")
			}
			if r.Window == "five_hour" || weeklyWindow(r.Window) || r.Window == "extra_usage" {
				return fmt.Errorf("use standard reserves for five-hour/weekly windows; credit balances cannot be reserved as subscription quota")
			}
			if math.IsNaN(r.Floor) || math.IsInf(r.Floor, 0) || r.Floor < 0 || r.Floor > maximumReserveFraction || r.ReleaseMinutes < 0 || r.ReleaseMinutes > maximumWindowReleaseMinutes {
				return fmt.Errorf("invalid native quota-window reserve")
			}
		}
	}
	return validateModelRules(p.ModelRoutes)
}
func cloneOwnership(values map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range values {
		out[k] = v
	}
	return out
}
func cloneProviderReserves(values map[string]*sharedClaudePolicy) map[string]*sharedClaudePolicy {
	out := map[string]*sharedClaudePolicy{}
	for k, v := range values {
		if v != nil {
			c := *v
			c.Weekdays = append([]int(nil), v.Weekdays...)
			out[k] = &c
		}
	}
	return out
}
func cloneWindowReserves(values map[string]map[string]windowReserve) map[string]map[string]windowReserve {
	out := map[string]map[string]windowReserve{}
	for id, rules := range values {
		out[id] = map[string]windowReserve{}
		for k, r := range rules {
			out[id][k] = r
		}
	}
	return out
}
func windowOptions(q snapshot) []map[string]any {
	rows := []map[string]any{}
	seen := map[string]bool{}
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			if b.Window == "extra_usage" {
				continue
			}
			key := quotaWindowKey(g.Name, b.Window)
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, map[string]any{"key": key, "group": g.Name, "window": b.Window, "standard": b.Window == "five_hour" || weeklyWindow(b.Window)})
		}
	}
	return rows
}
func subscriptionSchema() map[string]any {
	settings := map[string]any{}
	for _, f := range policyFields() {
		if !strings.HasPrefix(f.Path, "shared_claude.") {
			continue
		}
		node := map[string]any{"type": f.Type}
		if f.Minimum != nil {
			node["minimum"] = *f.Minimum
		}
		if f.Maximum != nil {
			node["maximum"] = *f.Maximum
		}
		if len(f.Enum) > 0 {
			node["enum"] = f.Enum
		}
		if f.Type == "array" {
			node["items"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 6}
			node["minItems"] = 1
			node["uniqueItems"] = true
		}
		settings[strings.TrimPrefix(f.Path, "shared_claude.")] = node
	}
	return map[string]any{"type": "object", "maxProperties": 100, "additionalProperties": map[string]any{"type": "object", "additionalProperties": false, "properties": settings}}
}
func windowReservesSchema() map[string]any {
	return map[string]any{"type": "object", "maxProperties": 500, "additionalProperties": map[string]any{"type": "object", "maxProperties": 100, "additionalProperties": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"group", "window", "remaining_floor"}, "properties": map[string]any{"group": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "window": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "remaining_floor": map[string]any{"type": "number", "minimum": 0, "maximum": maximumReserveFraction}, "release_within_minutes": map[string]any{"type": "integer", "minimum": 0, "maximum": 10080}}}}}
}
func providerFields() []policyField {
	out := []policyField{}
	for _, f := range policyFields() {
		if strings.HasPrefix(f.Path, "shared_claude.") {
			f.Path = strings.TrimPrefix(f.Path, "shared_claude.")
			f.ProviderScoped = false
			f.Advanced = false
			out = append(out, f)
		}
	}
	return out
}

func validateNewSubscriptionEntries(next, current policy) error {
	ids, oldIDs := map[string]accountReserve{}, map[string]accountReserve{}
	for id := range next.AccountPriority {
		ids[id] = accountReserve{}
	}
	for id := range current.AccountPriority {
		oldIDs[id] = accountReserve{}
	}
	for id := range next.AccountOwnership {
		ids[id] = accountReserve{}
	}
	for id := range current.AccountOwnership {
		oldIDs[id] = accountReserve{}
	}
	for id := range next.WindowReserves {
		ids[id] = accountReserve{}
	}
	for id := range current.WindowReserves {
		oldIDs[id] = accountReserve{}
	}
	for id := range current.AccountReserves {
		oldIDs[id] = accountReserve{}
	}
	if err := validateNewAccountReserves(ids, oldIDs); err != nil {
		return err
	}
	for id, rules := range next.WindowReserves {
		for key := range rules {
			if _, exists := current.WindowReserves[id][key]; exists {
				continue
			}
			state.Lock()
			q := state.Quota[id]
			state.Unlock()
			found := false
			for _, option := range windowOptions(q) {
				if option["key"] == key {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("new window rules must reference a reported quota window; reload the account")
			}
		}
	}
	newProviders := map[string]bool{}
	for provider := range next.ProviderReserves {
		if _, ok := current.ProviderReserves[provider]; !ok {
			newProviders[provider] = true
		}
	}
	if len(newProviders) > 0 {
		files, err := hostAuthList()
		if err != nil {
			return fmt.Errorf("unable to verify provider settings")
		}
		for _, f := range files {
			delete(newProviders, f.provider())
		}
		entries, _ := fetchHostModelInventory()
		for _, entry := range entries {
			delete(newProviders, entry.Provider)
		}
		if len(newProviders) > 0 {
			return fmt.Errorf("new provider policies must reference connected providers")
		}
	}
	return nil
}

// Native daily/hourly/weekly-only quotas have no five-hour opportunities.
// Compare spendable fraction per remaining day, without inventing that window.
func rankNativeWindows(rank adaptiveRank, windows []deadlineWindow, now time.Time, active int, age time.Duration, maxAge int) adaptiveRank {
	pressure := 0.0
	for _, w := range windows {
		if !w.b.Reset.After(now) {
			continue
		}
		value := w.spendable * 24 / math.Max(1.0/60, w.b.Reset.Sub(now).Hours())
		if value > pressure {
			pressure = value
			rank.PacingDeadline = w.b.Reset
			rank.WeeklyReset = w.b.Reset
		}
	}
	rank.WeeklyPressure = pressure
	rank.Score = 2*pressure/(1+pressure) + .02*rank.Remaining - .001*float64(min(active, 20)) - .002*clamp(age.Seconds()/float64(maxAge), 0, 1)
	rank.Reason = "Available native quota windows; no future reset reported"
	if !rank.PacingDeadline.IsZero() {
		rank.Reason = fmt.Sprintf("Native quota deadline: %.0f%% usable before %s", rank.Remaining*100, rank.PacingDeadline.Local().Format("Jan 02 15:04"))
	}
	return rank
}
