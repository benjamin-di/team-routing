package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type quotaAccount struct {
	ID       string         `json:"auth_id"`
	Index    string         `json:"auth_index"`
	Provider string         `json:"provider"`
	Metadata map[string]any `json:"metadata"`
	Storage  []byte         `json:"storage_json"`
}
type quotaHTTP struct {
	Code   int         `json:"status_code"`
	Header http.Header `json:"header"`
	Body   string      `json:"body"`
}
type quotaProblem struct {
	Code  int
	Retry time.Duration
}

func (q *quotaProblem) Error() string { return fmt.Sprintf("quota request failed (HTTP %d)", q.Code) }

type quotaEntry struct {
	Quota    snapshot
	Next     time.Time
	Checked  time.Time `json:"-"`
	Failures int
	Message  string
	Busy     chan struct{} `json:"-"`
}
type quotaCache struct {
	sync.Mutex
	Entries        map[string]*quotaEntry
	Loaded         bool
	Path           string
	Now            func() time.Time
	Aliases        map[string]string
	Hits, Requests int
}

var monitor = &quotaCache{Entries: map[string]*quotaEntry{}, Path: "team-quota-cache.json", Now: time.Now}

func (q *quotaCache) load() {
	if q.Loaded {
		return
	}
	q.Loaded = true
	raw, e := os.ReadFile(statePath(q.Path))
	if e == nil {
		var saved map[string]*quotaEntry
		if json.Unmarshal(raw, &saved) == nil && len(saved) <= 1000 {
			q.Entries = saved
		}
	}
}
func (q *quotaCache) persist() {
	if q.Path == "" {
		return
	}
	raw, e := json.Marshal(q.Entries)
	if e != nil {
		return
	}
	path := statePath(q.Path)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0600) == nil {
		_ = os.Rename(tmp, path)
	}
}
func (q *quotaCache) get(identity string, ttl time.Duration, fetch func() (snapshot, error)) (snapshot, error) {
	for {
		q.Lock()
		q.load()
		entry := q.Entries[identity]
		if entry == nil {
			entry = &quotaEntry{}
			q.Entries[identity] = entry
		}
		now := q.Now()
		entry.Checked = now
		if now.Before(entry.nextRead(ttl, now)) {
			q.Hits++
			cached, message := entry.Quota, entry.Message
			q.Unlock()
			if cached.At.IsZero() {
				return cached, fmt.Errorf("%s", message)
			}
			return cached, nil
		}
		if entry.Busy != nil {
			done := entry.Busy
			q.Unlock()
			select {
			case <-done:
				continue
			case <-time.After(25 * time.Second):
				return snapshot{}, fmt.Errorf("quota refresh is still in progress")
			}
		}
		entry.Busy = make(chan struct{})
		q.Requests++
		q.Unlock()
		result, err := fetch()
		q.Lock()
		now = q.Now()
		if err == nil && len(result.Groups) > 0 {
			result.LastFiveHourReset = rememberLastFiveHourReset(entry.Quota, result, now)
			result.Rates = estimateRates(entry.Quota, result, now)
			result.Equivalents = estimateEquivalents(entry.Quota, result)
			result.At = now
			entry.Quota = result
			entry.Failures = 0
			entry.Message = ""
			entry.Next = now.Add(ttl)
			for _, g := range result.Groups {
				for _, b := range g.Buckets {
					if b.Window != "extra_usage" && b.Remaining <= 0 && b.Reset.After(now) && b.Reset.Add(time.Second).Before(entry.Next) {
						entry.Next = b.Reset.Add(time.Second)
					}
				}
			}
		} else {
			entry.Failures++
			delay := time.Duration(math.Min(600, 30*math.Pow(2, float64(min(entry.Failures-1, 6))))) * time.Second
			entry.Message = "Quota temporarily unavailable; cached readings retained."
			if p, ok := err.(*quotaProblem); ok {
				if p.Code == 429 {
					// A transient rate limit should retry within the quota freshness
					// grace, rather than imposing a 15-minute admission outage.
					delay = time.Duration(math.Min(7200, 30*math.Pow(2, float64(min(entry.Failures-1, 8))))) * time.Second
					entry.Message = "Provider rate limited quota reads; refresh is paused."
				} else if p.Code == 401 || p.Code == 403 {
					delay = 15 * time.Minute
					entry.Message = "Quota authentication rejected; check account sign-in."
				}
				if p.Retry > delay {
					delay = p.Retry
				}
			}
			entry.Next = now.Add(delay)
		}
		close(entry.Busy)
		entry.Busy = nil
		q.persist()
		cached, message := entry.Quota, entry.Message
		q.Unlock()
		if cached.At.IsZero() {
			if message == "" {
				message = "Quota unavailable."
			}
			return cached, fmt.Errorf("%s", message)
		}
		return cached, nil
	}
}

func rememberLastFiveHourReset(old, next snapshot, now time.Time) time.Time {
	last := old.LastFiveHourReset
	for _, q := range []snapshot{old, next} {
		for _, g := range q.Groups {
			for _, b := range g.Buckets {
				if b.Window == "five_hour" && b.Reset.After(last) && !b.Reset.After(now.Add(5*time.Hour+90*time.Second)) {
					last = b.Reset
				}
			}
		}
	}
	return last
}

func exhaustedResetPassed(q snapshot, now time.Time) bool {
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			if b.Window != "extra_usage" && b.Remaining <= 0 && b.Reset.After(q.At) && !b.Reset.After(now) {
				return true
			}
		}
	}
	return false
}

func (entry *quotaEntry) nextRead(ttl time.Duration, now time.Time) time.Time {
	due := entry.Next
	if !entry.Quota.At.IsZero() && !exhaustedResetPassed(entry.Quota, now) {
		freshUntil := entry.Quota.At.Add(ttl)
		for _, g := range entry.Quota.Groups {
			for _, b := range g.Buckets {
				if b.Window != "extra_usage" && b.Remaining <= 0 && b.Reset.After(now) && b.Reset.Add(time.Second).Before(freshUntil) {
					freshUntil = b.Reset.Add(time.Second)
				}
			}
		}
		if freshUntil.After(due) {
			due = freshUntil
		}
	}
	return due
}

// Sleep until the next cached due time, rather than adding a full polling
// interval after a sequential scan. This preserves backoff and wakes promptly
// after a confirmed exhausted window resets.
func (q *quotaCache) nextPoll(poll int, scanStarted time.Time) time.Duration {
	delay := time.Duration(poll) * time.Second
	q.Lock()
	defer q.Unlock()
	now := q.Now()
	for _, entry := range q.Entries {
		if entry.Next.IsZero() || entry.Checked.Before(scanStarted) {
			continue
		}
		until := entry.nextRead(time.Duration(poll)*time.Second, now).Sub(now)
		if until < delay {
			delay = until
		}
	}
	return max(0, delay)
}
func accountIdentity(a quotaAccount) string {
	email, _ := a.Metadata["email"].(string)
	account, _ := a.Metadata["account_id"].(string)
	org, _ := a.Metadata["organization_uuid"].(string)
	if email != "" {
		account = strings.ToLower(strings.TrimSpace(email))
	}
	if account == "" {
		account = a.ID
	}
	return digest(a.Provider + "|" + account + "|" + org)
}
func hydrateAccount(a quotaAccount) quotaAccount {
	if len(a.Storage) > 0 {
		var stored map[string]any
		if json.Unmarshal(a.Storage, &stored) == nil {
			if a.Metadata == nil {
				a.Metadata = map[string]any{}
			}
			for k, v := range stored {
				if _, ok := a.Metadata[k]; !ok {
					a.Metadata[k] = v
				}
			}
		}
	}
	return a
}
func monitoredQuota(a quotaAccount) (snapshot, error) {
	a = hydrateAccount(a)
	p, e := readPolicy()
	if e != nil {
		return snapshot{}, e
	}
	ttl := time.Duration(p.Poll) * time.Second
	identity := accountIdentity(a)
	monitor.Lock()
	if monitor.Aliases == nil {
		monitor.Aliases = map[string]string{}
	}
	monitor.Aliases[a.ID] = identity
	monitor.Unlock()
	result, e := monitor.get(identity, ttl, func() (snapshot, error) { return fetchProviderQuota(a, providerQuotaHTTP) })
	result = normalizeQuotaSnapshot(a.Provider, result)
	if !result.At.IsZero() {
		state.Lock()
		state.Quota[a.ID] = result
		state.Unlock()
	}
	return result, e
}
func cacheDetails(a quotaAccount) map[string]any {
	a = hydrateAccount(a)
	monitor.Lock()
	defer monitor.Unlock()
	monitor.load()
	identity := accountIdentity(a)
	if alias := monitor.Aliases[a.ID]; alias != "" {
		identity = alias
	}
	entry := monitor.Entries[identity]
	if entry == nil {
		return map[string]any{"state": "not_loaded"}
	}
	status := "cached"
	if entry.Busy != nil {
		status = "refreshing"
	} else if entry.Failures > 0 && monitor.Now().Before(entry.Next) {
		status = "backoff"
	}
	return map[string]any{"state": status, "next_refresh": entry.Next, "last_success": entry.Quota.At, "message": entry.Message, "failures": entry.Failures}
}

// providerQuotaHTTP reads the account's current access token through the
// host and sends the quota request through the host's HTTP bridge, so the
// proxy's outbound proxy settings apply.
func providerQuotaHTTP(a quotaAccount, method, endpoint string, headers map[string]string, body string) (quotaHTTP, error) {
	storage := a.Storage
	if len(storage) == 0 && a.Index != "" {
		raw, e := hostAuthJSON(a.Index)
		if e != nil {
			return quotaHTTP{}, fmt.Errorf("credential unavailable")
		}
		storage = raw
	}
	var stored struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(storage, &stored) != nil || stored.AccessToken == "" {
		return quotaHTTP{}, fmt.Errorf("credential has no access token")
	}
	resolved := map[string]string{}
	for name, value := range headers {
		resolved[name] = strings.ReplaceAll(value, "$TOKEN$", stored.AccessToken)
	}
	return hostHTTP(method, endpoint, resolved, body)
}
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if n, e := strconv.Atoi(v); e == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if when, e := http.ParseTime(v); e == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func googleQuotaRetry(body string) time.Duration {
	var envelope struct {
		Error struct {
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil {
		return 0
	}
	var retry time.Duration
	for _, raw := range envelope.Error.Details {
		var detail struct {
			Type  string `json:"@type"`
			Delay string `json:"retryDelay"`
		}
		if json.Unmarshal(raw, &detail) != nil || detail.Type != "type.googleapis.com/google.rpc.RetryInfo" {
			continue
		}
		if delay, err := time.ParseDuration(detail.Delay); err == nil && delay > retry {
			retry = delay
		}
	}
	return retry
}

type quotaTransport func(quotaAccount, string, string, map[string]string, string) (quotaHTTP, error)

func fetchProviderQuota(a quotaAccount, send quotaTransport) (snapshot, error) {
	method, endpoint, body := "GET", "", ""
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json", "Content-Type": "application/json"}
	switch a.Provider {
	case "claude":
		endpoint = "https://api.anthropic.com/api/oauth/usage"
		headers["anthropic-beta"] = "oauth-2025-04-20"
		headers["User-Agent"] = "claude-cli/2.1.312 (external, cli)"
	case "codex":
		endpoint = "https://chatgpt.com/backend-api/wham/usage"
		headers["User-Agent"] = "codex-cli/0.165.0"
		for _, k := range []string{"chatgpt_account_id", "account_id"} {
			if id, ok := a.Metadata[k].(string); ok && id != "" {
				headers["Chatgpt-Account-Id"] = id
				break
			}
		}
	case "antigravity":
		endpoint = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
		method = "POST"
		project, _ := a.Metadata["project_id"].(string)
		if project == "" {
			return snapshot{}, fmt.Errorf("Antigravity project identity unavailable")
		}
		data, _ := json.Marshal(map[string]string{"project": project})
		body = string(data)
		headers["User-Agent"] = "antigravity/2.28.1 windows/amd64"
	default:
		return snapshot{}, fmt.Errorf("quota monitoring unsupported for this provider")
	}
	started := time.Now()
	response, e := send(a, method, endpoint, headers, body)
	if e != nil {
		recordQuotaDiagnostic(a, response, e, started)
		return snapshot{}, e
	}
	if response.Code < 200 || response.Code >= 300 {
		recordQuotaDiagnostic(a, response, nil, started)
		retry := retryAfter(response.Header, time.Now())
		if a.Provider == "antigravity" {
			retry = max(retry, googleQuotaRetry(response.Body))
		}
		return snapshot{}, &quotaProblem{response.Code, retry}
	}
	q, parseErr := parseProviderQuota(a.Provider, []byte(response.Body), time.Now())
	recordQuotaDiagnostic(a, response, parseErr, started)
	return q, parseErr
}
func readNumber(value any) (float64, bool) {
	n, ok := value.(float64)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func parseReset(value any) time.Time {
	switch v := value.(type) {
	case string:
		t, _ := time.Parse(time.RFC3339Nano, v)
		return t
	case float64:
		return time.Unix(int64(v), 0).UTC()
	}
	return time.Time{}
}
func fraction(used float64) float64 { return math.Max(0, math.Min(1, 1-used/100)) }
func parseProviderQuota(provider string, raw []byte, now time.Time) (snapshot, error) {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return snapshot{}, fmt.Errorf("invalid quota response")
	}
	out := snapshot{}
	switch provider {
	case "claude":
		g := group{Name: "Claude"}
		if extra, ok := payload["extra_usage"].(map[string]any); ok {
			data, _ := json.Marshal(extra)
			var status extraUsageStatus
			if json.Unmarshal(data, &status) == nil {
				out.ExtraUsage = &status
			}
		}
		names := []string{}
		for name := range payload {
			if name == "five_hour" || name == "seven_day" || strings.HasPrefix(name, "seven_day_") {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			v, ok := payload[name].(map[string]any)
			if !ok {
				continue
			}
			if used, ok := readNumber(v["utilization"]); ok {
				g.Buckets = append(g.Buckets, bucket{Window: name, Remaining: fraction(used), Reset: parseReset(v["resets_at"])})
			}
		}
		if len(g.Buckets) > 0 {
			out.Groups = append(out.Groups, g)
		}
	case "codex":
		g := group{Name: "Codex"}
		limit, _ := payload["rate_limit"].(map[string]any)
		for _, name := range []string{"primary_window", "secondary_window"} {
			v, ok := limit[name].(map[string]any)
			if !ok {
				continue
			}
			used, ok := readNumber(v["used_percent"])
			if !ok {
				continue
			}
			seconds, _ := readNumber(v["limit_window_seconds"])
			window := name
			if seconds == 18000 {
				window = "five_hour"
			} else if seconds == 604800 {
				window = "weekly"
			}
			reset := parseReset(v["reset_at"])
			if reset.IsZero() {
				after, _ := readNumber(v["reset_after_seconds"])
				if after > 0 {
					reset = now.Add(time.Duration(after) * time.Second)
				}
			}
			g.Buckets = append(g.Buckets, bucket{Window: window, Remaining: fraction(used), Reset: reset})
		}
		if len(g.Buckets) > 0 {
			out.Groups = append(out.Groups, g)
		}
	case "antigravity":
		groups, _ := payload["groups"].([]any)
		for _, item := range groups {
			v, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := v["displayName"].(string)
			g := group{Name: name}
			buckets, _ := v["buckets"].([]any)
			for _, item := range buckets {
				b, ok := item.(map[string]any)
				if !ok {
					continue
				}
				remaining, ok := readNumber(b["remainingFraction"])
				if !ok {
					continue
				}
				window, _ := b["window"].(string)
				label, _ := b["displayName"].(string)
				window = normalizedQuotaWindow(provider, window, label)
				g.Buckets = append(g.Buckets, bucket{Window: window, Remaining: math.Max(0, math.Min(1, remaining)), Reset: parseReset(b["resetTime"])})
			}
			if len(g.Buckets) > 0 {
				out.Groups = append(out.Groups, g)
			}
		}
	}
	if len(out.Groups) == 0 {
		return out, fmt.Errorf("no recognized quota windows returned")
	}
	return out, nil
}

// refreshQuotaMonitor reads quota for every enabled subscription account
// and returns how many accounts it found.
func refreshQuotaMonitor() int {
	files, e := hostAuthList()
	if e != nil {
		hostLog("warn", "team-routing: listing accounts failed: "+e.Error())
		return 0
	}
	found := 0
	for _, f := range files {
		select {
		case <-state.Stop:
			return found
		default:
		}
		provider := f.provider()
		if f.Disabled || (provider != "claude" && provider != "codex" && provider != "antigravity") {
			continue
		}
		// While the host is still starting it lists auth files from disk
		// without runtime indexes; skip them so the poll retries shortly.
		if f.Index == "" {
			continue
		}
		found++
		raw, e := hostAuthJSON(f.Index)
		if e != nil {
			hostLog("warn", "team-routing: reading credential "+f.Name+" failed: "+e.Error())
			continue
		}
		_, _ = monitoredQuota(quotaAccount{ID: f.ID, Index: f.Index, Provider: provider, Storage: raw})
	}
	return found
}
func quotaRPC(raw []byte) (any, error) {
	var a quotaAccount
	if e := json.Unmarshal(raw, &a); e != nil {
		return nil, e
	}
	result, e := monitoredQuota(a)
	if e != nil {
		return nil, e
	}
	details := cacheDetails(a)
	groups := make([]group, len(result.Groups))
	for i, g := range result.Groups {
		groups[i] = g
		groups[i].Buckets = append([]bucket(nil), g.Buckets...)
		for j := range groups[i].Buckets {
			description := "Quota read " + result.At.UTC().Format(time.RFC3339) + ". Shared cache; repeated refreshes do not bypass its cooldown."
			if message, ok := details["message"].(string); ok && message != "" {
				description += " " + message
			}
			groups[i].Buckets[j].Description = description
		}
	}
	return map[string]any{"groups": groups, "cache": details}, nil
}

func monitorStats() map[string]any {
	monitor.Lock()
	defer monitor.Unlock()
	busy := 0
	for _, entry := range monitor.Entries {
		if entry.Busy != nil {
			busy++
		}
	}
	return map[string]any{"cache_hits": monitor.Hits, "upstream_reads": monitor.Requests, "inflight": busy}
}
