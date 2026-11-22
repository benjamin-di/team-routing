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
	Auth  string    `json:"auth"`
	Model string    `json:"model,omitempty"`
	At    time.Time `json:"at"`
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
	// Completion bounds when a non-rejected response could have been sampled.
	// Only an endpoint request starting after it can clear the warning.
	AvailableAt time.Time `json:"available_at,omitempty"`
}
type admissionData struct {
	Completed map[string]completedDebit         `json:"completed"`
	Signals   map[string]map[string]quotaSignal `json:"signals"`
	Billing   map[string]billingSignal          `json:"billing,omitempty"`
}

var admission = struct {
	sync.Mutex
	Path       string
	Broken     bool
	Data       admissionData
	Reconciled map[string]time.Time
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
	admission.Reconciled = map[string]time.Time{}
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
			continue
		}
		if cut := admission.Reconciled[d.Auth]; !cut.IsZero() && !d.At.After(cut.Add(-settlementLagSeconds*time.Second)) {
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
	// Keep the newest available status: it is the only evidence that credits
	// may apply after the endpoint reading, until a newer reading supersedes it.
	for auth, s := range admission.Data.Billing {
		if s.availableAt().IsZero() && now.Sub(s.At) > ledgerRetentionSeconds*time.Second {
			delete(admission.Data.Billing, auth)
		}
	}
}

func (b billingSignal) availableAt() time.Time {
	if b.Status != "rejected" && b.At.After(b.AvailableAt) {
		return b.At
	}
	return b.AvailableAt
}

func admissionSignalTime(at, now time.Time) (time.Time, bool) {
	if at.IsZero() || at.After(now.Add(quotaResetJitterSeconds*time.Second)) || now.Sub(at) > ledgerRetentionSeconds*time.Second {
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
func reconcileAdmission(auth string, observedAt time.Time) {
	if auth == "" || observedAt.IsZero() {
		return
	}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	if admission.Broken {
		return
	}
	cutoff := observedAt
	for _, s := range admission.Data.Signals[auth] {
		if !s.At.IsZero() && s.Reset.After(observedAt) && s.At.Before(cutoff) {
			cutoff = s.At
		}
	}
	if admission.Reconciled == nil {
		admission.Reconciled = map[string]time.Time{}
	}
	if cutoff.After(admission.Reconciled[auth]) {
		admission.Reconciled[auth] = cutoff
	}
	limit := admission.Reconciled[auth].Add(-settlementLagSeconds * time.Second)
	pruned := 0
	for id, d := range admission.Data.Completed {
		if d.Auth == auth && !d.At.After(limit) {
			delete(admission.Data.Completed, id)
			pruned++
		}
	}
	if pruned > 0 {
		persistAdmission()
	}
}

func recordAdmissionCompletion(auth, request string, at time.Time) {
	recordAdmissionCompletionForModel(auth, "", request, at)
}
func recordAdmissionCompletionForModel(auth, model, request string, at time.Time) {
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
	if prior, ok := admission.Data.Completed[key]; !ok || at.After(prior.At) || (prior.Model == "" && model != "") {
		if model == "" && ok {
			model = prior.Model
		}
		if ok && prior.At.After(at) {
			at = prior.At
		}
		admission.Data.Completed[key] = completedDebit{Auth: auth, Model: model, At: at}
	}
	pruneAdmission(at)
	persistAdmission()
}
func recordAdmissionSignals(auth string, h http.Header, at, now time.Time) []quotaSignal {
	at, valid := admissionSignalTime(at, now)
	status := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Status")))
	hasBilling := len(h.Values("Anthropic-Ratelimit-Unified-Overage-Status")) > 0
	signals := parseQuotaSignals(h, at, now)
	if !valid {
		if !hasBilling || status == "rejected" {
			return nil
		}
		// Unusable provenance can establish a warning, never fresh quota.
		at = now
	}
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
		// Completions arrive out of order. Within one window keep the lowest
		// observed allowance, and the newer of two equal observations.
		if !ok || s.Reset.After(old.Reset.Add(quotaResetJitterSeconds*time.Second)) || (sameQuotaWindow(s.Reset, old.Reset) && (s.Remaining < old.Remaining || s.Remaining == old.Remaining && s.At.After(old.At))) {
			admission.Data.Signals[auth][s.Window] = s
		}
	}
	if hasBilling {
		if admission.Data.Billing == nil {
			admission.Data.Billing = map[string]billingSignal{}
		}
		next := admission.Data.Billing[auth]
		next.AvailableAt = next.availableAt()
		if at.After(next.At) {
			next.Status, next.At = status, at
		}
		if status != "rejected" && now.After(next.AvailableAt) {
			next.AvailableAt = now
		}
		admission.Data.Billing[auth] = next
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
func quotaObservationTime(p policy, c candidate, model string, q snapshot, now time.Time) time.Time {
	q.At = q.observationTime(c.Provider)
	if c.Provider != "claude" || q.At.IsZero() {
		return q.At
	}
	// Headers cannot reveal newly introduced windows. A successful endpoint
	// reading must renew the schema within the configured quota-age bound.
	if now.Sub(q.At) >= time.Duration(p.MaxAge)*time.Second {
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

func completionMatchesGroup(provider, model, groupName string) bool {
	return model == "" || groupName == "" || relevantGroup(provider, model, groupName)
}

func countCompletedBetween(provider, auth, groupName string, from, to time.Time) int {
	if auth == "" || from.IsZero() || !to.After(from) {
		return 0
	}
	admission.Lock()
	defer admission.Unlock()
	loadAdmission()
	if admission.Broken {
		return 0
	}
	n := 0
	for _, d := range admission.Data.Completed {
		if d.Auth == auth && d.At.After(from) && !d.At.After(to) && completionMatchesGroup(provider, d.Model, groupName) {
			n++
		}
	}
	return n
}

// Learn per-request quota cost per window from paired polls within the same
// window so high-RPM plans do not inherit Claude's fixed 0.5%/request estimate.
func estimateRequestCosts(provider, auth string, old, next snapshot, now time.Time) map[string]float64 {
	out := map[string]float64{}
	if provider == "claude" {
		return out
	}
	for k, v := range old.RequestCosts {
		if v >= 1e-5 && v <= 0.05 {
			out[k] = v
		}
	}
	elapsed := now.Sub(old.At)
	if auth == "" || old.At.IsZero() || elapsed < 30*time.Second || elapsed > ledgerRetentionSeconds*time.Second {
		return out
	}
	previous := map[string]bucket{}
	for _, g := range old.Groups {
		for _, b := range g.Buckets {
			previous[g.Name+"|"+b.Window] = b
		}
	}
	for _, g := range next.Groups {
		n := countCompletedBetween(provider, auth, g.Name, old.At, now)
		if n < 5 {
			continue
		}
		for _, b := range g.Buckets {
			k := g.Name + "|" + b.Window
			prior, ok := previous[k]
			if !ok || !sameQuotaWindow(prior.Reset, b.Reset) || b.Remaining > prior.Remaining+1e-9 {
				continue
			}
			delta := math.Max(0, prior.Remaining-b.Remaining)
			if delta < 0.005 && n < 20 {
				continue
			}
			defaultUnit := fiveRequestFraction
			if weeklyWindow(b.Window) {
				defaultUnit = weeklyRequestFraction
			}
			var observed float64
			if delta > 1e-9 {
				observed = delta / float64(n)
			} else {
				// Over >=20 requests with zero observable delta, bound cost below
				// the smallest rounding step so bursts stop over-reserving.
				observed = math.Min(defaultUnit, 0.005/float64(n))
			}
			observed = clamp(observed, 1e-5, 0.05)
			if prev, ok := out[k]; ok {
				observed = .5*prev + .5*observed
			}
			out[k] = observed
		}
	}
	return out
}

func requestUnit(c candidate, q snapshot, groupName, window string) float64 {
	unit := fiveRequestFraction
	if weeklyWindow(window) {
		unit = weeklyRequestFraction
	}
	if c.Provider != "claude" {
		if learned := q.RequestCosts[groupName+"|"+window]; learned >= 1e-5 && learned <= 0.05 {
			return learned
		}
	}
	return unit
}

type admissionProjection struct {
	Projected, CompletedReserve float64
	CompletedCount              int
	Reset                       time.Time
	Signal                      *quotaSignal
	Reason                      string
}

func projectAdmission(c candidate, q snapshot, groupName string, b bucket, rate float64, active int, now time.Time, ignoreForecast bool) admissionProjection {
	q.At = q.observationTime(c.Provider)
	unit := requestUnit(c, q, groupName, b.Window)
	activeReserve := float64(active) * math.Max(unit, rate/60)
	forecastRate := rate
	if ignoreForecast {
		forecastRate = 0
	}
	r := admissionProjection{Reset: b.Reset}
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
			if d.Auth == c.ID && d.At.After(cutoff.Add(-settlementLagSeconds*time.Second)) && !d.At.After(now.Add(quotaResetJitterSeconds*time.Second)) && completionMatchesGroup(c.Provider, d.Model, groupName) {
				n++
			}
		}
		return n
	}
	r.CompletedCount = countSince(q.At)
	r.CompletedReserve = float64(r.CompletedCount) * unit
	r.Projected = b.Remaining - math.Max(forecastRate*math.Max(0, now.Sub(q.At).Hours()), r.CompletedReserve) - activeReserve
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
			r.Projected = math.Min(b.Remaining, s.Remaining) - math.Max(forecastRate*math.Max(0, now.Sub(s.At).Hours()), r.CompletedReserve) - activeReserve
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
