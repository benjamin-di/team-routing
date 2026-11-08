package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type policy struct {
	Owned            []string                            `json:"owned_emails"`
	Routes           map[string]target                   `json:"routes"`
	ClientLabels     map[string]string                   `json:"client_labels,omitempty"`
	AccountPriority  accountPriorities                   `json:"account_priority,omitempty"`
	AccountReserves  map[string]accountReserve           `json:"account_reserves,omitempty"`
	ModelRoutes      map[string]map[string]modelRule     `json:"model_routes,omitempty"`
	AccountOwnership map[string]bool                     `json:"account_ownership,omitempty"`
	ProviderReserves map[string]*sharedClaudePolicy      `json:"provider_reserves,omitempty"`
	WindowReserves   map[string]map[string]windowReserve `json:"window_reserves,omitempty"`
	// Legacy fields are accepted for old clients, ignored by deadline routing.
	Cutoff            float64             `json:"cutoff_used,omitempty"`
	ExpiringUsed      float64             `json:"expiring_used_below,omitempty"`
	ExpiringSeconds   int                 `json:"expiring_within_seconds,omitempty"`
	Poll              int                 `json:"poll_seconds"`
	MaxAge            int                 `json:"max_quota_age_seconds"`
	SharedClaude      *sharedClaudePolicy `json:"shared_claude,omitempty"`
	StatelessThreads  bool                `json:"claude_stateless_threads"`
	RecordRouting     bool                `json:"record_routing_events"`
	WeeklyRatio       float64             `json:"weekly_five_hour_ratio"`
	RequireCreditsOff bool                `json:"require_extra_usage_disabled"`
}
type bucket struct {
	Window      string
	Description string    `json:"description,omitempty"`
	Remaining   float64   `json:"remainingFraction"`
	Reset       time.Time `json:"resetTime"`
}
type group struct {
	Name    string `json:"displayName"`
	Buckets []bucket
}
type snapshot struct {
	At                time.Time
	Groups            []group
	Rates             map[string]float64 `json:"rates,omitempty"`
	Equivalents       map[string]float64 `json:"five_hour_per_weekly,omitempty"`
	DemandRate        float64            `json:"-"`
	ExtraUsage        *extraUsageStatus  `json:"extra_usage,omitempty"`
	LastFiveHourReset time.Time          `json:"last_five_hour_reset,omitempty"`
}
type candidate struct {
	ID, Provider string
	Metadata     map[string]any
	Attributes   map[string]string
}
type binding struct {
	ID      string
	At      time.Time
	BoundAt time.Time
}

var state = struct {
	sync.Mutex
	Quota   map[string]snapshot
	Pins    map[string]binding
	Running bool
	Stop    chan struct{}
}{Quota: map[string]snapshot{}, Pins: map[string]binding{}, Stop: make(chan struct{})}

// pluginVersion is the release version reported to the host.
const pluginVersion = "1.8.2"

func main() {}
func readPolicy() (policy, error) {
	p := policy{}
	raw, e := os.ReadFile(statePath("team-routing.json"))
	if e != nil {
		return p, e
	}
	raw = bytes.TrimPrefix(raw, []byte{239, 187, 191})
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	e = decoder.Decode(&p)
	if e != nil {
		return p, e
	}
	if p.WeeklyRatio == 0 {
		p.WeeklyRatio = 5
	}
	if err := validatePolicy(p); err != nil {
		return p, err
	}
	return p, nil
}
func digest(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
func readClientPolicy() (policy, error) {
	p, err := readPolicy()
	if err != nil {
		return p, fmt.Errorf("service temporarily unavailable; please try again later")
	}
	return p, nil
}
func key(h http.Header) string {
	v := h.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return h.Get("X-Api-Key")
}

// Native session.Enrich resolves headers, body IDs, subagents and inferred sessions
// before scheduling. Keep only caller/provider/model isolation in this plugin.
func canonicalSession(meta map[string]any) string {
	if id, _ := meta["canonical_session_id"].(string); strings.TrimSpace(id) != "" {
		return "canonical:" + id
	}
	return "key-fallback"
}
func stopWorker() {
	state.Lock()
	defer state.Unlock()
	select {
	case <-state.Stop:
	default:
		close(state.Stop)
	}
	state.Running = false
}
func startWorker() {
	state.Lock()
	defer state.Unlock()
	if state.Running {
		return
	}
	select {
	case <-state.Stop:
		state.Stop = make(chan struct{})
	default:
	}
	state.Running = true
	go poll(state.Stop)
}
func dispatch(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var lifecycle struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		_ = json.Unmarshal(raw, &lifecycle)
		if err := applyConfig(lifecycle.ConfigYAML); err != nil {
			return nil, err
		}
		if err := ensurePolicyFile(); err != nil {
			return nil, err
		}
		if _, err := readPolicy(); err != nil {
			return nil, err
		}
		startWorker()
		return map[string]any{"schema_version": 7, "metadata": map[string]any{"Name": "Team Routing", "Version": pluginVersion, "Author": "Yousha-dev", "GitHubRepository": "https://github.com/Yousha-dev/team-routing", "ConfigFields": []map[string]string{
			{"Name": "state_dir", "Type": "string", "Description": "Folder for team-routing.json, the quota cache and backups (default: the proxy's working directory)."},
			{"Name": "management_url", "Type": "string", "Description": "Optional management API base URL override. The browser dashboard sends its own origin, including port and HTTPS."},
		}}, "capabilities": map[string]bool{"scheduler": true, "scheduler_across_priorities": true, "model_router": true, "management_api": true, "quota_provider": true, "request_interceptor": true, "request_interceptor_omit_body_before": false, "request_interceptor_omit_body_after": true, "request_lifecycle_plugin": true, "usage_plugin": true}}, nil
	case "usage.handle":
		return observeUsage(raw)
	case "request.intercept_before", "request.intercept_after", "request.complete":
		recordRequestDiagnostic(method, raw)
		if method == "request.intercept_before" {
			return interceptBefore(raw)
		}
		return trackRequest(method, raw)
	case "quota.identifier":
		return map[string]string{"identifier": "team-routing"}, nil
	case "quota.describe":
		return map[string]any{"supported_providers": []string{"claude", "codex", "antigravity"}, "display_name": "Team Routing Quota", "supports_reset": false}, nil
	case "quota.fetch":
		return quotaRPC(raw)
	case "management.register":
		return map[string]any{"routes": []map[string]string{{"Method": "GET", "Path": dashboardAPI + "/state"}, {"Method": "GET", "Path": dashboardAPI + "/schema"}, {"Method": "PUT", "Path": dashboardAPI + "/policy"}, {"Method": "POST", "Path": dashboardAPI + "/evaluate"}, {"Method": "POST", "Path": dashboardAPI + "/refresh"}}, "resources": []map[string]string{{"Path": "/dashboard", "Menu": "Team Routing", "Description": "Client routing, availability and validated policy."}, {"Path": "/dashboard.css"}, {"Path": "/dashboard.js"}, {"Path": "/editors.js"}}}, nil
	case "management.handle":
		var req dashboardRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return handleDashboard(req), nil
	case "model.route":
		startWorker()
		var req struct {
			Headers            http.Header
			AvailableProviders []string
			RequestedModel     string
		}
		if e := json.Unmarshal(raw, &req); e != nil {
			return nil, e
		}
		p, e := readClientPolicy()
		if e != nil {
			return nil, e
		}
		t, ok, reason := resolveClientRoute(p, digest(key(req.Headers)), req.RequestedModel)
		if !ok {
			return map[string]bool{"Handled": false}, nil
		}
		found := false
		for _, provider := range req.AvailableProviders {
			if provider == t.Provider {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("requested model is currently unavailable")
		}
		return map[string]any{"Handled": true, "TargetKind": "provider", "Target": t.Provider, "TargetModel": t.Model, "Reason": reason}, nil
	case "scheduler.pick":
		startWorker()
		var req struct {
			Provider, Model string
			Options         struct {
				Headers  http.Header
				Metadata map[string]any
			}
			Candidates []candidate
		}
		if e := json.Unmarshal(raw, &req); e != nil {
			return nil, e
		}
		p, e := readClientPolicy()
		if e != nil {
			return nil, e
		}
		state.Lock()
		defer state.Unlock()
		return choose(p, req.Provider, req.Model, req.Options.Headers, req.Options.Metadata, req.Candidates, time.Now()), nil
	default:
		return nil, fmt.Errorf("unsupported method")
	}
}

// Ownership is only a final equal-score tie-break, never a numerical bonus.
func choose(p policy, provider, model string, h http.Header, meta map[string]any, candidates []candidate, now time.Time) map[string]any {
	return adaptiveChoose(p, provider, model, h, meta, candidates, now)
}
func poll(stop chan struct{}) {
	for {
		found := refresh()
		p, e := readPolicy()
		delay := time.Minute
		if e == nil {
			delay = nextQuotaPoll(p.Poll)
		}
		// The first refresh can run while the host is still loading accounts;
		// retry soon instead of leaving every account without a reading.
		if found == 0 && delay > 15*time.Second {
			delay = 15 * time.Second
		}
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
	}
}
func refresh() int { return refreshQuotaMonitor() }
