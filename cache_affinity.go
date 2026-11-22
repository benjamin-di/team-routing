package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"sync"
	"time"
)

func affinityKey(h http.Header, meta map[string]any, provider, model string) string {
	scope, _ := meta["caller_scope"].(string)
	if scope == "" {
		scope = key(h)
	}
	// Model and provider isolation prevent unsafe reuse across overrides/families.
	return digest(scope + "|" + canonicalSession(meta) + "|" + provider + "|" + model)
}

type rankEvidence struct {
	adaptiveRank
	Eligible bool
}
type selectionEvidence struct {
	At         time.Time      `json:"at"`
	Selection  string         `json:"selection_id"`
	Session    string         `json:"session_hash"`
	Model      string         `json:"model"`
	Auth       string         `json:"auth_id"`
	Previous   string         `json:"previous_auth_id,omitempty"`
	Reason     string         `json:"reason"`
	Threshold  float64        `json:"switch_threshold,omitempty"`
	Candidates []rankEvidence `json:"candidates,omitempty"`
}
type warmCache struct {
	At       time.Time
	Auth     string
	Fraction float64
}
type tokenCounters struct {
	Requests uint64  `json:"requests"`
	Measured uint64  `json:"measured_requests"`
	Failed   uint64  `json:"failed_usage_records"`
	Input    int64   `json:"input_tokens"`
	Read     int64   `json:"cache_read_tokens"`
	Write    int64   `json:"cache_creation_tokens"`
	Output   int64   `json:"output_tokens"`
	Share    float64 `json:"cache_read_share"`
}

var evidence = struct {
	sync.Mutex
	Selections                    map[string]selectionEvidence
	Warm                          map[string]warmCache
	Seen                          map[string]time.Time
	Counters                      map[string]tokenCounters
	Recent                        []selectionEvidence
	Pruned                        time.Time
	Decisions, Retained, Switched uint64
}{Selections: map[string]selectionEvidence{}, Warm: map[string]warmCache{}, Seen: map[string]time.Time{}, Counters: map[string]tokenCounters{}}
var evidenceFiles sync.Mutex

func cacheWarmth(pk, auth string, now time.Time) float64 {
	evidence.Lock()
	defer evidence.Unlock()
	w := evidence.Warm[pk]
	// Conservative standard lifetime, measured from request START, not completion.
	if w.Auth != auth || now.Before(w.At) || now.Sub(w.At) >= warmCacheSeconds*time.Second {
		return 0
	}
	return w.Fraction
}
func pruneEvidence(now time.Time) {
	if !evidence.Pruned.IsZero() && now.Sub(evidence.Pruned) < time.Minute {
		return
	}
	evidence.Pruned = now
	for id, r := range evidence.Selections {
		if now.Sub(r.At) > 2*time.Hour {
			delete(evidence.Selections, id)
		}
	}
	for id, r := range evidence.Seen {
		if now.Sub(r) > 2*time.Hour {
			delete(evidence.Seen, id)
		}
	}
	for id, r := range evidence.Warm {
		if now.Sub(r.At) >= warmCacheSeconds*time.Second {
			delete(evidence.Warm, id)
		}
	}
	// Bound even unusually high traffic without evicting randomly on every turn.
	if len(evidence.Selections) > 10000 {
		evidence.Selections = map[string]selectionEvidence{}
	}
	if len(evidence.Seen) > 20000 {
		evidence.Seen = map[string]time.Time{}
	}
	if len(evidence.Warm) > 10000 {
		evidence.Warm = map[string]warmCache{}
	}
}
func recordDecision(p policy, h http.Header, meta map[string]any, provider, model, auth, prior, reason string, threshold float64, ranks []rankEvidence, now time.Time) {
	selection, _ := meta["selection_id"].(string)
	r := selectionEvidence{At: now, Selection: selection, Session: affinityKey(h, meta, provider, model), Model: model, Auth: auth, Previous: prior, Reason: reason, Threshold: threshold}
	// Bound per-record size while recording the selected candidate even in large pools.
	if len(ranks) > 32 {
		ranks = append([]rankEvidence(nil), ranks...)
		for i, v := range ranks {
			if v.ID == auth {
				ranks[0], ranks[i] = ranks[i], ranks[0]
				break
			}
		}
		ranks = ranks[:32]
	}
	r.Candidates = ranks
	evidence.Lock()
	pruneEvidence(now)
	if selection != "" {
		evidence.Selections[selection] = r
	}
	evidence.Decisions++
	if prior != "" && prior == auth {
		evidence.Retained++
	}
	if prior != "" && auth != "" && prior != auth {
		evidence.Switched++
	}
	evidence.Recent = append(evidence.Recent, r)
	if len(evidence.Recent) > 20 {
		evidence.Recent = evidence.Recent[len(evidence.Recent)-20:]
	}
	evidence.Unlock()
	if p.RecordRouting {
		appendEvidence("team-routing-decisions.jsonl", r)
	}
}

type usageEvidence struct {
	RequestID, SelectionID, Provider, Model, AuthID string
	TraceID, BaseURL, ExecutorType, ResponseModel   string
	Stream                                          bool
	TTFT                                            time.Duration
	Failure                                         struct {
		StatusCode int
		Body       string
	}
	RequestedAt     time.Time
	Latency         time.Duration
	Failed          bool
	ResponseHeaders http.Header
	Detail          struct{ InputTokens, OutputTokens, CachedTokens, CacheReadTokens, CacheCreationTokens, ReasoningTokens int64 }
}

func observeUsage(raw []byte) (any, error) {
	var u usageEvidence
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	if u.AuthID == "" || u.RequestID == "" {
		return map[string]any{}, nil
	}
	recordUsageDiagnostic(u)
	paid := u.Provider == "claude" && overageInUse(u.ResponseHeaders)
	if paid {
		fencePaidAccount(u.AuthID, time.Now())
	}
	now := time.Now()
	start := u.RequestedAt
	read := max(int64(0), u.Detail.CacheReadTokens, u.Detail.CachedTokens)
	write := max(int64(0), u.Detail.CacheCreationTokens)
	input := max(int64(0), u.Detail.InputTokens)
	// Native Claude input excludes cached tokens; OpenAI-compatible input includes them.
	if u.Provider == "claude" {
		input += read + write
	} else {
		input = max(input, read+write)
	}
	output := max(int64(0), u.Detail.OutputTokens)
	evidence.Lock()
	pruneEvidence(now)
	// One usage record per execution/account/model, independent of terminal callbacks.
	id := digest(u.RequestID + "|" + u.AuthID + "|" + u.Model)
	if _, ok := evidence.Seen[id]; ok {
		evidence.Unlock()
		return map[string]any{}, nil
	}
	evidence.Seen[id] = now
	d, matched := evidence.Selections[u.SelectionID]
	if !matched || d.Auth != u.AuthID {
		matched = false
	}
	if matched && !u.Failed && read+write > 0 && input > 0 && now.Sub(start) < warmCacheSeconds*time.Second && !start.After(now.Add(quotaResetJitterSeconds*time.Second)) {
		old := evidence.Warm[d.Session]
		if !start.Before(old.At) {
			evidence.Warm[d.Session] = warmCache{start, u.AuthID, clamp(float64(read+write)/float64(input), 0, 1)}
		}
	}
	group := u.AuthID + "|" + u.Model
	c := evidence.Counters[group]
	c.Requests++
	if u.Failed {
		c.Failed++
	}
	if input+output > 0 {
		c.Measured++
	}
	c.Input += input
	c.Read += read
	c.Write += write
	c.Output += output
	if c.Input > 0 {
		c.Share = float64(c.Read) / float64(c.Input)
	}
	evidence.Counters[group] = c
	evidence.Unlock()
	var signals []quotaSignal
	if !u.Failed || input+output > 0 {
		recordAdmissionCompletionForModel(u.AuthID, u.Model, completionIdentity(u.RequestID, u.SelectionID), now)
	}
	if u.Provider == "claude" {
		signals = recordAdmissionSignals(u.AuthID, u.ResponseHeaders, start, now)
	}
	if p, err := readPolicy(); err == nil && p.RecordRouting {
		event := map[string]any{"at": now, "requested_at": start, "request_hash": digest(u.RequestID), "selection_id": u.SelectionID, "auth_id": u.AuthID, "provider": u.Provider, "model": u.Model, "failed": u.Failed, "input_tokens": input, "cache_read_tokens": read, "cache_creation_tokens": write, "output_tokens": output, "latency_ms": u.Latency.Milliseconds(), "matched_selection": matched, "paid_usage_observed": paid}
		if len(signals) > 0 {
			event["included_quota_feedback"] = signals
		}
		if matched {
			event["session_hash"] = d.Session
			event["selection_reason"] = d.Reason
		}
		appendEvidence("team-routing-usage.jsonl", event)
	}
	return map[string]any{}, nil
}
func appendEvidence(name string, event any) {
	raw, err := json.Marshal(event)
	if err != nil {
		noteEvidenceWrite(name, err)
		return
	}
	evidenceFiles.Lock()
	defer evidenceFiles.Unlock()
	path := statePath(name)
	if info, err := os.Stat(path); err == nil && info.Size() > 8<<20 {
		if err := os.Remove(path + ".previous"); err != nil && !os.IsNotExist(err) {
			noteEvidenceWrite(name, err)
			return
		}
		if err := os.Rename(path, path+".previous"); err != nil {
			noteEvidenceWrite(name, err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		noteEvidenceWrite(name, err)
		return
	}
	written, err := f.Write(append(raw, '\n'))
	if err == nil && written != len(raw)+1 {
		err = io.ErrShortWrite
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	noteEvidenceWrite(name, err)
}
func cacheDiagnostics() map[string]any {
	evidence.Lock()
	defer evidence.Unlock()
	counters := map[string]tokenCounters{}
	var totals tokenCounters
	for id, c := range evidence.Counters {
		counters[id] = c
		totals.Requests += c.Requests
		totals.Measured += c.Measured
		totals.Failed += c.Failed
		totals.Input += c.Input
		totals.Read += c.Read
		totals.Write += c.Write
		totals.Output += c.Output
	}
	if totals.Input > 0 {
		totals.Share = math.Min(1, float64(totals.Read)/float64(totals.Input))
	}
	recent := append([]selectionEvidence(nil), evidence.Recent...)
	return map[string]any{"totals": totals, "by_account_model": counters, "decisions": evidence.Decisions, "affinity_retained": evidence.Retained, "account_switches": evidence.Switched, "recent_decisions": recent, "warm_cache_seconds": 300, "scope": "since process start; raw counters, not subscription billing weights"}
}
