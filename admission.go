package main

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Completed work remains reserved until a later quota reading covers it.
// Previously only active work was reserved, so a fast burst could repeatedly
// spend the same cached allowance between provider polls.
type completedDebit struct {
	Auth string    `json:"auth"`
	At   time.Time `json:"at"`
}

// A definite upstream rejection is not completed inference. Unknown/canceled
// outcomes remain conservative, and positive measured usage always wins.
func definiteRejection(outcome string, status int) bool {
	if outcome != "failed" {
		return false
	}
	switch status {
	case 400, 401, 402, 403, 404, 405, 406, 410, 411, 413, 414, 415, 422, 429, 431:
		return true
	}
	return false
}

type quotaSignal struct {
	Window    string    `json:"window"`
	Remaining float64   `json:"remaining"`
	Reset     time.Time `json:"reset"`
	At        time.Time `json:"at"`
}
type billingSignal struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}
type admissionData struct {
	Completed map[string]completedDebit         `json:"completed"`
	Signals   map[string]map[string]quotaSignal `json:"signals"`
	Billing   map[string]billingSignal          `json:"billing,omitempty"`
}

var admission = struct {
	sync.Mutex
	Path   string
	Broken bool
	Data   admissionData
}{}

func loadAdmission() {
	path, err := filepath.Abs(statePath("team-routing-admission.json"))
	if err != nil {
		admission.Broken = true
		return
	}
	if admission.Path == path && admission.Data.Completed != nil {
		return
	}
	admission.Path, admission.Broken = path, false
	admission.Data = admissionData{Completed: map[string]completedDebit{}, Signals: map[string]map[string]quotaSignal{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || json.Unmarshal(raw, &admission.Data) != nil || admission.Data.Completed == nil || admission.Data.Signals == nil {
		admission.Broken = true
	}
	if admission.Data.Billing == nil {
		admission.Data.Billing = map[string]billingSignal{}
	}
}
func persistAdmission() {
	// Unit lifecycle callbacks can run without an installed policy. Production
	// always has one; do not create state files for those isolated callbacks.
	if _, err := os.Stat(statePath("team-routing.json")); err != nil {
		return
	}
	raw, err := json.Marshal(admission.Data)
	if err != nil {
		admission.Broken = true
		return
	}
	f, err := os.CreateTemp(filepath.Dir(admission.Path), ".admission-*.tmp")
	if err != nil {
		admission.Broken = true
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), admission.Path)
	}
	if err != nil {
		admission.Broken = true
	}
}
func pruneAdmission(now time.Time) {
	for id, d := range admission.Data.Completed {
		if now.Sub(d.At) > ledgerRetentionSeconds*time.Second {
			delete(admission.Data.Completed, id)
		}
	}
	for auth, windows := range admission.Data.Signals {
		for window, s := range windows {
			if now.Sub(s.Reset) > quotaResetJitterSeconds*time.Second {
				delete(windows, window)
			}
		}
		if len(windows) == 0 {
			delete(admission.Data.Signals, auth)
		}
	}
	for auth, s := range admission.Data.Billing {
		if now.Sub(s.At) > ledgerRetentionSeconds*time.Second {
			delete(admission.Data.Billing, auth)
		}
	}
}

func admissionSignalTime(at, now time.Time) (time.Time, bool) {
	if at.IsZero() {
		at = now
	}
	if at.After(now.Add(quotaResetJitterSeconds*time.Second)) || now.Sub(at) > ledgerRetentionSeconds*time.Second {
		return at, false
	}
	return at, true
}

func parseQuotaSignals(h http.Header, at, now time.Time) []quotaSignal {
	at, valid := admissionSignalTime(at, now)
	if !valid {
		return nil
	}
	out := []quotaSignal{}
	for _, spec := range []struct {
		window, prefix string
		duration       time.Duration
	}{{"five_hour", "5h", 5 * time.Hour}, {"seven_day", "7d", 7 * 24 * time.Hour}} {
		u, err := strconv.ParseFloat(h.Get("Anthropic-Ratelimit-Unified-"+spec.prefix+"-Utilization"), 64)
		if err != nil || math.IsNaN(u) || math.IsInf(u, 0) || u < 0 || u > 1 {
			continue
		}
		unix, err := strconv.ParseInt(h.Get("Anthropic-Ratelimit-Unified-"+spec.prefix+"-Reset"), 10, 64)
		if err != nil {
			continue
		}
		reset := time.Unix(unix, 0)
		if !reset.After(at) || reset.Add(-spec.duration).After(at.Add(quotaResetJitterSeconds*time.Second)) {
			continue
		}
		// Provider utilization is rounded; keep one percentage point of slack.
		out = append(out, quotaSignal{Window: spec.window, Remaining: math.Max(0, 1-u-headerRoundingFraction), Reset: reset, At: at})
	}
	return out
}

func completionIdentity(request, selection string) string {
	if selection != "" {
		return "selection:" + selection
	}
	return "request:" + request
}
func recordAdmissionCompletion(auth, request string, at time.Time) {
	if auth == "" || request == "" {
		return
	}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	if admission.Broken {
		return
	}
	key := digest(auth + "|" + request)
	if prior, ok := admission.Data.Completed[key]; !ok || at.After(prior.At) {
		admission.Data.Completed[key] = completedDebit{Auth: auth, At: at}
	}
	pruneAdmission(at)
	persistAdmission()
}
func recordAdmissionSignals(auth string, h http.Header, at, now time.Time) []quotaSignal {
	at, valid := admissionSignalTime(at, now)
	if !valid {
		return nil
	}
	signals := parseQuotaSignals(h, at, now)
	status := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Status")))
	hasBilling := len(h.Values("Anthropic-Ratelimit-Unified-Overage-Status")) > 0
	if len(signals) == 0 && !hasBilling {
		return nil
	}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	if admission.Broken {
		return nil
	}
	if admission.Data.Signals[auth] == nil {
		admission.Data.Signals[auth] = map[string]quotaSignal{}
	}
	for _, s := range signals {
		old, ok := admission.Data.Signals[auth][s.Window]
		if !ok || s.Reset.After(old.Reset.Add(quotaResetJitterSeconds*time.Second)) || (sameQuotaWindow(s.Reset, old.Reset) && s.Remaining <= old.Remaining) {
			admission.Data.Signals[auth][s.Window] = s
		}
	}
	if hasBilling {
		if admission.Data.Billing == nil {
			admission.Data.Billing = map[string]billingSignal{}
		}
		if old, ok := admission.Data.Billing[auth]; !ok || at.After(old.At) {
			admission.Data.Billing[auth] = billingSignal{Status: status, At: at}
		}
	}
	pruneAdmission(now)
	persistAdmission()
	return signals
}

func quotaSignalMatches(s quotaSignal, q snapshot, b bucket, now time.Time) bool {
	return s.Reset.After(now) && (b.Reset.IsZero() || sameQuotaWindow(b.Reset, s.Reset) || (s.Reset.After(b.Reset.Add(quotaResetJitterSeconds*time.Second)) && !s.At.Before(q.At)))
}

// Headers refresh only the included windows they actually report. Keep the
// endpoint time unless every applicable bucket has a newer matching signal.
func quotaObservationTime(c candidate, model string, q snapshot, now time.Time) time.Time {
	if c.Provider != "claude" || q.At.IsZero() {
		return q.At
	}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	if admission.Broken {
		return q.At
	}
	at := time.Time{}
	for _, g := range q.Groups {
		if !relevantGroup(c.Provider, model, g.Name) {
			continue
		}
		for _, b := range g.Buckets {
			if b.Window == "extra_usage" || !relevantQuotaBucket(c.Provider, model, b.Window) {
				continue
			}
			key := b.Window
			if key == "weekly" {
				key = "seven_day"
			}
			s, ok := admission.Data.Signals[c.ID][key]
			if !ok || !s.At.After(q.At) || !quotaSignalMatches(s, q, b, now) {
				return q.At
			}
			if at.IsZero() || s.At.Before(at) {
				at = s.At
			}
		}
	}
	if at.IsZero() {
		return q.At
	}
	return at
}

type admissionProjection struct {
	Projected, CompletedReserve float64
	CompletedCount              int
	Reset                       time.Time
	Signal                      *quotaSignal
	Reason                      string
}

func projectAdmission(c candidate, q snapshot, b bucket, rate float64, active int, now time.Time) admissionProjection {
	unit := fiveRequestFraction
	if weeklyWindow(b.Window) {
		unit = weeklyRequestFraction
	}
	activeReserve := float64(active) * math.Max(unit, rate/60)
	r := admissionProjection{Reset: b.Reset, Projected: b.Remaining - rate*math.Max(0, now.Sub(q.At).Hours()) - activeReserve}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	if admission.Broken {
		r.Reason = "Request admission state is unavailable"
		return r
	}
	// Account for a short settlement lag as well as all completions since the
	// cached read. Do not add the burn-rate forecast twice for the same work.
	countSince := func(cutoff time.Time) int {
		n := 0
		for _, d := range admission.Data.Completed {
			if d.Auth == c.ID && d.At.After(cutoff.Add(-settlementLagSeconds*time.Second)) && !d.At.After(now.Add(quotaResetJitterSeconds*time.Second)) {
				n++
			}
		}
		return n
	}
	r.CompletedCount = countSince(q.At)
	r.CompletedReserve = float64(r.CompletedCount) * unit
	r.Projected = b.Remaining - math.Max(rate*math.Max(0, now.Sub(q.At).Hours()), r.CompletedReserve) - activeReserve
	if c.Provider != "claude" {
		return r
	}
	key := b.Window
	if key == "weekly" {
		key = "seven_day"
	}
	s, ok := admission.Data.Signals[c.ID][key]
	if ok && quotaSignalMatches(s, q, b, now) {
		// Signals can lower a cached allowance, never increase it. An old
		// response cannot reopen quota or replace a newer window.
		// A newer numeric reading settles estimated work preceding it. Keep
		// the cached raw allowance as an upper bound, but do not retain all
		// estimated debits since the old poll as well: many tiny cache-hit
		// requests could otherwise create a false exhaustion despite fresh
		// provider headroom. Recent settlement work stays reserved.
		if !s.At.Before(q.At) && s.Remaining <= b.Remaining {
			r.CompletedCount = countSince(s.At)
			r.CompletedReserve = float64(r.CompletedCount) * unit
			r.Projected = math.Min(b.Remaining, s.Remaining) - math.Max(rate*math.Max(0, now.Sub(s.At).Hours()), r.CompletedReserve) - activeReserve
		}
		remaining := s.Remaining - float64(countSince(s.At))*unit - activeReserve
		if remaining < r.Projected {
			r.Projected = remaining
		}
		r.Reset = s.Reset
		r.Signal = &s
	}
	return r
}
