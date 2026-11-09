package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureQuota() snapshot {
	return snapshot{Groups: []group{{Name: "Claude", Buckets: []bucket{{Window: "five_hour", Remaining: .8}}}}}
}

func TestTransientQuotaRateLimitRecoversBeforeClaudeSampleExpires(t *testing.T) {
	for _, retry := range []time.Duration{0, 30 * time.Second, 45 * time.Second} {
		t.Run(retry.String(), func(t *testing.T) {
			t.Chdir(t.TempDir())
			applyConfig(nil)
			t.Cleanup(func() { applyConfig(nil) })
			now := time.Now()
			p := defaultPolicy()
			p.Poll, p.MaxAge = 60, 180
			c := candidate{ID: "transient-quota-limit", Provider: "claude"}
			cache := &quotaCache{Entries: map[string]*quotaEntry{}, Now: func() time.Time { return now }}
			calls := 0
			fetch := func() (snapshot, error) {
				calls++
				if calls == 2 {
					return snapshot{}, &quotaProblem{429, retry}
				}
				return sample(now, .8, .8, 5*time.Hour), nil
			}
			ttl := time.Duration(p.Poll) * time.Second
			first, err := cache.get(c.ID, ttl, fetch)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(ttl)
			cached, err := cache.get(c.ID, ttl, fetch)
			if err != nil || !cached.At.Equal(first.At) || calls != 2 {
				t.Fatal("rate limit must retain the original observation", cached, err, calls)
			}
			now = now.Add(max(30*time.Second, retry))
			recovered, err := cache.get(c.ID, ttl, fetch)
			if err != nil || calls != 3 || !recovered.At.Equal(now) {
				t.Fatalf("transient rate limit prevented a timely fresh observation: calls=%d age=%s err=%v", calls, now.Sub(recovered.At), err)
			}
			if _, eligible := rankAccount(p, c, "opus", recovered, now, 0); !eligible {
				t.Fatal("recovered included quota was not admitted")
			}
		})
	}
}

func TestQuotaRateLimitDoesNotRelaxProviderDelayOrAdmissionFreshness(t *testing.T) {
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	now := time.Now()
	p := defaultPolicy()
	p.Poll, p.MaxAge = 60, 180
	c := candidate{ID: "long-quota-limit", Provider: "claude"}
	cache := &quotaCache{Entries: map[string]*quotaEntry{}, Now: func() time.Time { return now }}
	calls := 0
	fetch := func() (snapshot, error) {
		calls++
		if calls == 2 {
			return snapshot{}, &quotaProblem{429, 30 * time.Minute}
		}
		return sample(now, .8, .8, 5*time.Hour), nil
	}
	ttl := time.Duration(p.Poll) * time.Second
	first, _ := cache.get(c.ID, ttl, fetch)
	now = now.Add(ttl)
	cache.get(c.ID, ttl, fetch)
	now = now.Add(5 * time.Minute)
	cached, err := cache.get(c.ID, ttl, fetch)
	if err != nil || calls != 2 || !cached.At.Equal(first.At) {
		t.Fatal("provider delay bypassed or stale observation timestamp renewed", err, calls)
	}
	if _, eligible := rankAccount(p, c, "opus", cached, now, 0); eligible {
		t.Fatal("rate-limit backoff admitted stale quota and billing data")
	}
	now = now.Add(25 * time.Minute)
	recovered, err := cache.get(c.ID, ttl, fetch)
	if err != nil || calls != 3 {
		t.Fatal("quota did not recover after the provider delay", err, calls)
	}
	if _, eligible := rankAccount(p, c, "opus", recovered, now, 0); !eligible {
		t.Fatal("fresh quota stayed blocked after the provider delay")
	}
}

func TestRepeatedQuotaRateLimitsIncreaseDelay(t *testing.T) {
	now := time.Now()
	cache := &quotaCache{Entries: map[string]*quotaEntry{}, Now: func() time.Time { return now }}
	calls := 0
	fetch := func() (snapshot, error) {
		calls++
		return snapshot{}, &quotaProblem{Code: 429}
	}
	delays := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, 64 * time.Minute, 2 * time.Hour, 2 * time.Hour}
	for i, delay := range delays {
		cache.get("limited", time.Minute, fetch)
		now = now.Add(delay - time.Second)
		cache.get("limited", time.Minute, fetch)
		if calls != i+1 {
			t.Fatalf("unexpected provider attempts during backoff: calls=%d want=%d", calls, i+1)
		}
		now = now.Add(time.Second)
	}
	cache.get("limited", time.Minute, fetch)
	if calls != len(delays)+1 {
		t.Fatalf("rate-limited quota did not retry with exponential backoff: calls=%d", calls)
	}
}

func TestQuotaRetryHonorsGoogleRetryInfo(t *testing.T) {
	for _, test := range []struct {
		name, header, details string
		delay                 time.Duration
	}{
		{"fractional", "", `{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"128.5s"}`, 128500 * time.Millisecond},
		{"longer header", "180", `{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"128.5s"}`, 3 * time.Minute},
		{"longer body", "45", `{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"128.5s"}`, 128500 * time.Millisecond},
		{"beyond cap", "", `{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"10800s"}`, 3 * time.Hour},
		{"multiple", "", `{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"180s"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"128.5s"}`, 3 * time.Minute},
		{"invalid details", "", `null,{}, {"@type":"other","retryDelay":"180s"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"-180s"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"invalid"}`, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			now := time.Now()
			cache := &quotaCache{Entries: map[string]*quotaEntry{}, Now: func() time.Time { return now }}
			calls := 0
			fetch := func() (snapshot, error) {
				return fetchProviderQuota(quotaAccount{Provider: "antigravity", Metadata: map[string]any{"project_id": "synthetic-project"}}, func(quotaAccount, string, string, map[string]string, string) (quotaHTTP, error) {
					calls++
					return quotaHTTP{Code: 429, Header: http.Header{"Retry-After": []string{test.header}}, Body: `{"error":{"details":[` + test.details + `]}}`}, nil
				})
			}
			cache.get("limited", time.Minute, fetch)
			now = now.Add(test.delay - time.Nanosecond)
			cache.get("limited", time.Minute, fetch)
			if calls != 1 {
				t.Fatal("quota retried before the provider cooldown expired", calls)
			}
			now = now.Add(time.Nanosecond)
			cache.get("limited", time.Minute, fetch)
			if calls != 2 {
				t.Fatal("quota did not retry when the cooldown expired", calls)
			}
		})
	}
}

func TestQuotaCacheTTLAndBackoff(t *testing.T) {
	now := time.Now()
	q := &quotaCache{Entries: map[string]*quotaEntry{}, Path: filepath.Join(t.TempDir(), "cache.json"), Now: func() time.Time { return now }}
	calls := 0
	fetch := func() (snapshot, error) { calls++; return fixtureQuota(), nil }
	first, e := q.get("account", 5*time.Minute, fetch)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	again, e := q.get("account", 5*time.Minute, fetch)
	if e != nil || calls != 1 || !again.At.Equal(first.At) {
		t.Fatal("fresh quota re-fetched or timestamp advanced")
	}
	now = now.Add(5 * time.Minute)
	limited := func() (snapshot, error) { calls++; return snapshot{}, &quotaProblem{429, 30 * time.Minute} }
	stale, e := q.get("account", 5*time.Minute, limited)
	if e != nil || !stale.At.Equal(first.At) {
		t.Fatal("last good quota not retained")
	}
	q.get("account", 5*time.Minute, limited)
	if calls != 2 {
		t.Fatal("backoff bypassed")
	}
	next := q.Entries["account"].Next
	if next.Sub(now) < 30*time.Minute {
		t.Fatal("Retry-After ignored")
	}
	reloaded := &quotaCache{Entries: map[string]*quotaEntry{}, Path: q.Path, Now: func() time.Time { return now }}
	reloaded.get("account", 5*time.Minute, limited)
	if calls != 2 {
		t.Fatal("restart bypassed backoff")
	}
}
func TestQuotaRequestsCoalesce(t *testing.T) {
	q := &quotaCache{Entries: map[string]*quotaEntry{}, Now: time.Now}
	var count atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	fetch := func() (snapshot, error) {
		if count.Add(1) == 1 {
			close(entered)
		}
		<-release
		return fixtureQuota(), nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); q.get("same", time.Minute, fetch) }()
	<-entered
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); q.get("same", time.Minute, fetch) }()
	}
	close(release)
	wg.Wait()
	if count.Load() != 1 {
		t.Fatal("concurrent duplicate provider reads")
	}
}

func TestExhaustedResetRefreshDoesNotBypassBackoff(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Minute)
	q := &quotaCache{Entries: map[string]*quotaEntry{}, Now: func() time.Time { return now }}
	calls := 0
	fetch := func() (snapshot, error) {
		calls++
		return snapshot{Groups: []group{{Name: "Claude", Buckets: []bucket{{Window: "five_hour", Remaining: 0, Reset: reset}}}}}, nil
	}
	q.get("a", 5*time.Minute, fetch)
	now = now.Add(30 * time.Second)
	q.get("a", 5*time.Minute, fetch)
	if calls != 1 {
		t.Fatal("polled before reset")
	}
	now = reset.Add(2 * time.Second)
	limited := func() (snapshot, error) { calls++; return snapshot{}, &quotaProblem{429, 30 * time.Minute} }
	q.get("a", 5*time.Minute, limited)
	if calls != 2 {
		t.Fatal("did not refresh promptly after reset")
	}
	now = now.Add(time.Minute)
	q.get("a", 5*time.Minute, limited)
	if calls != 2 {
		t.Fatal("known reset bypassed provider backoff")
	}
}
func TestQuotaParsersAndIdentity(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		provider, raw, window string
		remaining             float64
	}{{"claude", `{"five_hour":{"utilization":25,"resets_at":"2026-10-29T21:00:00Z"}}`, "five_hour", .75}, {"codex", `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1800000000}}}`, "five_hour", .9}, {"antigravity", `{"groups":[{"displayName":"Gemini Models","buckets":[{"displayName":"Weekly","remainingFraction":0.6,"resetTime":"2026-11-01T00:00:00Z"}]}]}`, "weekly", .6}} {
		q, e := parseProviderQuota(test.provider, []byte(test.raw), now)
		if e != nil || q.Groups[0].Buckets[0].Window != test.window || q.Groups[0].Buckets[0].Remaining != test.remaining {
			t.Fatalf("parser %s: %v", test.provider, e)
		}
	}
	a := quotaAccount{ID: "file-one", Provider: "claude", Metadata: map[string]any{"email": "Owner@example.com", "organization_uuid": "org"}}
	b := a
	b.ID = "file-two"
	if accountIdentity(a) != accountIdentity(b) {
		t.Fatal("duplicate account not deduplicated")
	}
	if _, e := parseProviderQuota("claude", []byte(`{"five_hour":{}}`), now); e == nil {
		t.Fatal("missing quota treated as zero usage")
	}
	if retryAfter(http.Header{"Retry-After": []string{"120"}}, now) != 2*time.Minute {
		t.Fatal("retry header")
	}
	if _, e := (&quotaCache{Entries: map[string]*quotaEntry{}, Now: time.Now}).get("bad", time.Minute, func() (snapshot, error) { return snapshot{}, fmt.Errorf("network") }); e == nil {
		t.Fatal("failed read created quota")
	}
}
