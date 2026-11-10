package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T) (policy, candidate, snapshot, time.Time, time.Time) {
	t.Helper()
	t.Chdir(t.TempDir())
	applyConfig(nil)
	t.Cleanup(func() { applyConfig(nil) })
	now := localTime("2026-11-01 14:00")
	detected := now.Add(-6 * time.Hour)
	p := scheduledPolicy()
	p.RequireCreditsOff = false
	c := candidate{ID: "recovery-shared", Provider: "claude"}
	q := sample(now, .97, .13, 4*time.Hour)
	q.Groups[0].Buckets[1].Reset = localTime("2026-11-01 18:00")
	on := true
	q.ExtraUsage = &extraUsageStatus{Enabled: &on}
	fencePaidAccount(c.ID, detected)
	return p, c, q, now, detected
}

func TestPaidFenceAutomaticallyRecoversAndPersists(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	if _, ok := rankAccount(p, c, "opus", q, now, 0); !ok {
		t.Fatal("fresh usable subscription in a newer window stayed fenced")
	}
	creditFences.Lock()
	creditFences.Path = ""
	creditFences.Unlock()
	if _, ok := rankAccount(p, c, "opus", q, now, 0); !ok {
		t.Fatal("restart restored cleared fence")
	}
	raw, err := os.ReadFile("team-routing-credit-events.jsonl")
	if err != nil || strings.Count(string(raw), `"event":"recovered"`) != 1 || !strings.Contains(string(raw), `"proof":"new_five_hour_window"`) {
		t.Fatal("missing or repeated recovery audit", string(raw), err)
	}
	// A fresh paid response must immediately fence again, despite cached quota.
	fencePaidAccount(c.ID, now)
	if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
		t.Fatal("new paid signal did not reblock")
	}
}

func TestPaidFenceCannotRecoverFromRefreshOrJitter(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	fencePaidAccount(c.ID, now.Add(-time.Hour))
	for _, reset := range []time.Time{now.Add(4 * time.Hour), now.Add(4*time.Hour + time.Minute), now.Add(6 * time.Hour)} {
		q.Groups[0].Buckets[0].Reset = reset
		if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
			t.Fatal("same window, jitter, or impossible future start cleared fence", reset)
		}
	}
}

func TestPaidFenceRecoveryRespectsQuotaAndBillingGuards(t *testing.T) {
	for _, kind := range []string{"weekly_reserve", "five_hour_reserve", "exhausted", "stale", "missing_weekly", "strict_credits_on", "unknown_billing_strict"} {
		t.Run(kind, func(t *testing.T) {
			p, c, q, now, _ := recoveryFixture(t)
			switch kind {
			case "weekly_reserve":
				q.Groups[0].Buckets[1].Reset = localTime("2026-11-01 20:00") // Not unlocked until 15:00.
			case "five_hour_reserve":
				q.Groups[0].Buckets[0].Remaining = .2
			case "exhausted":
				q.Groups[0].Buckets[1].Remaining = 0
			case "stale":
				q.At = now.Add(-time.Duration(p.Poll+61) * time.Second)
			case "missing_weekly":
				q.Groups[0].Buckets = q.Groups[0].Buckets[:1]
			case "strict_credits_on":
				p.RequireCreditsOff = true
			case "unknown_billing_strict":
				p.RequireCreditsOff = true
				q.ExtraUsage = nil
			}
			if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
				t.Fatal("unsafe recovery", kind)
			}
			var persisted map[string]time.Time
			raw, _ := os.ReadFile("team-routing-paid-usage-blocks.json")
			if json.Unmarshal(raw, &persisted) != nil || persisted[c.ID].IsZero() {
				t.Fatal("blocked recovery removed durable fence")
			}
		})
	}
}

func TestPaidFenceRecoversAfterWeeklyResetOrIdleFiveHourReset(t *testing.T) {
	for _, kind := range []string{"weekly", "idle"} {
		t.Run(kind, func(t *testing.T) {
			p, c, q, now, _ := recoveryFixture(t)
			if kind == "weekly" {
				fencePaidAccount(c.ID, now.Add(-2*time.Hour))
				q.Groups[0].Buckets[0].Reset = now.Add(time.Hour) // Five-hour window predates the signal.
				q.Groups[0].Buckets[1].Reset = now.Add(7*24*time.Hour - 10*time.Minute)
				q.Groups[0].Buckets[1].Remaining = .99
			} else {
				q.Groups[0].Buckets[0].Reset = time.Time{}
				q.Groups[0].Buckets[0].Remaining = 1
			}
			if _, ok := rankAccount(p, c, "opus", q, now, 0); !ok {
				t.Fatal("confirmed reset did not recover", kind)
			}
		})
	}
}

func TestPaidFenceUsesLatestPaidSignal(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	fencePaidAccount(c.ID, now.Add(-time.Hour))
	fencePaidAccount("another-account", now)
	creditFences.Lock()
	creditFences.Path = ""
	creditFences.Unlock()
	if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
		t.Fatal("recovery used first signal instead of latest in-flight response")
	}
	creditFences.Lock()
	other := creditFences.Denied["another-account"]
	creditFences.Unlock()
	if other.IsZero() {
		t.Fatal("updating an existing block lost another account")
	}
}

func TestPaidFenceRecoveryFailsClosedOnPersistenceFailure(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	creditFences.Lock()
	path := creditFences.Path
	creditFences.Unlock()
	// Keep the loaded map, but replace the destination file with a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
		t.Fatal("failed durable recovery allowed routing")
	}
	creditFences.Lock()
	broken := creditFences.Broken
	blocked := !creditFences.Denied[c.ID].IsZero()
	creditFences.Unlock()
	if !broken || !blocked {
		t.Fatal("failed recovery lost in-memory block")
	}
}

func TestCorruptCreditFenceCannotBeOverwrittenOrRecovered(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	if err := os.WriteFile("team-routing-paid-usage-blocks.json", []byte("invalid fence data"), 0600); err != nil {
		t.Fatal(err)
	}
	creditFences.Lock()
	creditFences.Path = ""
	creditFences.Unlock()
	if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
		t.Fatal("corrupt state recovered")
	}
	fencePaidAccount("another-account", now)
	raw, _ := os.ReadFile("team-routing-paid-usage-blocks.json")
	if string(raw) != "invalid fence data" {
		t.Fatal("new signal overwrote corrupt safety evidence")
	}
}

func TestConcurrentRecoveryIsIdempotent(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reason := creditSafetyReason(p, c, "", q, now); reason != "" {
				t.Error(reason)
			}
		}()
	}
	wg.Wait()
	raw, _ := os.ReadFile("team-routing-credit-events.jsonl")
	if strings.Count(string(raw), `"event":"recovered"`) != 1 {
		t.Fatal("concurrent recovery repeated durable changes", string(raw))
	}
}

func TestIdleResetUsesRememberedActualBoundary(t *testing.T) {
	p, c, q, now, _ := recoveryFixture(t)
	fencePaidAccount(c.ID, now.Add(-30*time.Minute))
	old := q
	old.Groups = []group{{Name: "Claude", Buckets: []bucket{{Window: "five_hour", Remaining: 0, Reset: now.Add(-10 * time.Minute)}}}}
	q.Groups[0].Buckets[0].Remaining = 1
	q.Groups[0].Buckets[0].Reset = time.Time{}
	q.LastFiveHourReset = rememberLastFiveHourReset(old, q, now)
	if _, ok := rankAccount(p, c, "opus", q, now, 0); !ok {
		t.Fatal("known reset unnecessarily waited five hours after paid signal")
	}
	fencePaidAccount(c.ID, now.Add(-5*time.Minute))
	if _, ok := rankAccount(p, c, "opus", q, now, 0); ok {
		t.Fatal("reset predating last paid signal cleared a new block")
	}
}
