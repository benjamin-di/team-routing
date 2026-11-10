# Team Routing

A standalone CLIProxyAPI plugin for reset-aware subscription routing, shared-account reserves, cache affinity and per-client model overrides. The official proxy and management UI stay unmodified.

## Installation

Install through the proxy Plugin Store, or place the library for your operating system in its plugin directory and enable it:

```yaml
plugins:
  enabled: true
  configs:
    team-routing:
      enabled: true
      # state_dir: /path/to/writable/folder
      # management_url: http://127.0.0.1:8317
```

Team Routing 1.8.1 uses ABI 1 / schema 7 and consumes the host's `canonical_session_id` scheduling metadata. Verified against official CLIProxyAPI 8.0.51. State defaults to the proxy working directory. First startup creates a policy; updates preserve existing settings. Keep core `routing.session-affinity` off so only the plugin's quota-aware affinity is configured in normal operation. On the verified host, handled plugin choices precede the native selector even when native affinity is on. Plugin and official proxy updates are independent, subject to ABI and session-metadata compatibility.

Build with Go and a C compiler: `go test -tags abiprobe ./...`, then `go build -buildmode=c-shared -trimpath -o team-routing.so .` (use `.dll` on Windows or `.dylib` on macOS). Windows is exercised locally; other platforms need their own runtime verification.

## Dashboard

Open **Team Routing** under **PLUGINS**, or `/v0/resource/plugins/team-routing/dashboard`. Enter your management key; it stays in page memory.

- **Overview:** global account availability, included allowance, reset times, enabled model support, refresh state and reserve-release details.
- **Client routing:** discovered keys, labels, and requested-model or explicit provider/model overrides. Providers and models come from the runtime registry, including custom providers. Search filters a native selection list.
- **Policy:** fields generated from the backend schema, advanced settings and evaluated rule precedence from the saved policy.
- **Diagnostics:** request-free model/key availability evaluation, measured cache tokens, recent selections and read-only engine parameters.

Save applies to new requests immediately. Unsaved edits survive status refresh. Stale revisions are rejected; reload before retrying. Refresh due quota respects provider backoff. Model-specific eligibility can differ from the overview's global status; use Diagnostics for a particular model. Start a fresh conversation when switching model families.

## Client keys and overrides

The official `access.api-keys` configuration is authoritative. Create, copy or revoke keys in **Config Panel > Access & Authentication > Client API Keys**. Team Routing discovers them dynamically; it does not return full keys or upstream tokens. Optional `secrets.json` supplies local labels but is not required.

Routes and labels use SHA-256 key IDs. Renaming a label cannot move a route to another key. Choose a default of **Use requested model** or **Override unmatched requests**. Under **Optional rules for specific requested models**, add an exact source model ID and choose either pass-through or an advertised provider/model target. Exact rules win over the default, including pass-through exceptions to a default override. Targets are resolved once, without chaining. Unmatched requests follow the default; an unavailable pass-through model fails normally. Revoked-key preferences remain inert on disk and do not authorize access. Rules also affect helper calls matching their model IDs; separate keys can isolate applications.

Without an override, the official host considers providers advertising the exact model ID. Different model generations are not equivalent aliases. Provider-specific tool, context, thread and streaming compatibility still matters. The plugin does not manufacture model support.

## Routing policy

Read [ROUTING-POLICY.md](ROUTING-POLICY.md) for formulas, schedule boundaries, credit safety and limitations. In order:

1. Native availability, exact model support, recent complete quota and billing safeguards determine eligibility.
2. Enabled provider reserve policies constrain reported quota windows. Explicit account reserves replace provider percentages. Owned accounts are exempt from default reserves only.
3. Parallel work first prefers the least-active eligible accounts. Within that pool, ranking compares usable weekly allowance against remaining five-hour opportunities and observed demand. Imminent predicted five-hour waste can take precedence.
4. Warm conversations retain their account when concurrency and urgency are similar. A busier account or material expiry urgency can move the next request; active requests continue. Ownership only breaks equal scores.

There is no 85% avoidance, 70% ownership switch or ten-minute affinity lock. Prompt order, cache-control directives and provider cache keys are preserved. Native usage callbacks measure cache reads/writes; token shares are not subscription billing weights. Missing measurements are not cache misses.

Session/header/body/subagent identity extraction belongs to the native host. The plugin consumes that canonical identity, keeps caller/provider/model isolation and applies quota-aware retention. It does not parse conversation IDs again or inject a private affinity header. Clients without a host-resolved identity use a caller/provider/model-scoped fallback. See [NATIVE-FEATURES.md](NATIVE-FEATURES.md) for the native/plugin feature comparison.

Strict billing mode (new-install default) requires provider credits explicitly disabled. Best-effort mode permits enabled/unknown provider credit settings while included quota is usable, rejects explicit paid requests and fences observed paid use. It cannot guarantee zero spillover charges while provider credits are enabled.

## Policy API

All routes require the management key and use base `/v0/management/plugins/team-routing`.

| Route | Purpose |
| --- | --- |
| `GET /schema` | JSON Schema draft 2020-12, editable field metadata/groups, read-only engine parameters |
| `GET /state` | Saved policy/revision, evaluated rules, active keys, runtime catalog and diagnostics |
| `PUT /policy` | Validate and atomically save using the current revision, with a private backup |
| `POST /evaluate` | Explain cached eligibility for `model`, optional `client_id` and `provider`; no inference |
| `POST /refresh` | Check due readings without bypassing cache or provider backoff |

Runtime validation and UI bounds share one backend field registry. Semantic checks cover time zones, weekday uniqueness, hour ordering and freshness relationships. Unknown settings and invalid provider/model pairs are rejected. The plugin reloads saved policy on every request; the browser defines no routing thresholds.

## Reliability and quota cache

Auth Files, the dashboard and background monitor share one persisted quota cache. Only quota endpoints are read through native authenticated HTTP callbacks; no promotional or reset-credit requests are sent. Concurrent reads coalesce. Cache hits and failed refreshes do not advance successful sample timestamps. Provider 429 backs off from 15 minutes up to two hours; longer Retry-After is honored. Freshness safeguards remain enforced.

Claude admission retains completed-request reservations until a fresh reading covers them and constrains headroom with numeric provider response quota headers. Both survive restarts. Variable request cost, delayed counters, already-running requests and external usage prevent exact provider-side reserve guarantees.

Claude response headers count as a fresh quota reading when newer matching signals cover every included window reported by the last successful endpoint reading for the requested model. Freshness uses the oldest covering signal's request start; headers only lower cached allowance. Uncovered five-hour or model-specific weekly windows still need a recent endpoint reading, and headers cannot replace a missing initial reading. In strict billing mode, fresh `Overage-Status: rejected` also refreshes billing only when the endpoint last reported extra usage disabled. A non-rejected status newer than the endpoint reading excludes the account immediately and until a newer endpoint reading, even if later responses report rejected; enabled or unknown endpoint settings remain excluded. Billing observations survive restarts.

Known unmetered client/auth/validation/rate-limit rejections do not invent completed quota use. Positive measured usage, including failed partial output, remains reserved. Timeouts, cancellations, conflicts and unknown failures remain conservative. Model-specific quota preflight does not promote an account's last error flag into a cooldown for unrelated models; the native host applies actual model cooldowns during selection. Provider rate limits can still reject parallel workloads despite remaining subscription quota; the plugin does not fabricate throughput capacity or clear native cooldowns.

For larger pools, native `routing.retry.max-retry-credentials: 0` allows every eligible credential to be attempted within a retry round. A positive cap can stop a round before a healthy later account is reached. Configure retry rounds and wait intervals in the official Routing page; Team Routing does not implement a second retry loop or change those settings automatically.

`claude_stateless_threads` negotiates Message Threads beta off before inference. Clients must support unsupported-beta fallback and retry with full history. The plugin never guesses missing context or removes continuation fields. Normal provider prompt caching remains available. Select requested models from the searchable dropdown or optionally type an exact custom ID; both use the same rule validation.

Public capacity errors are generic HTTP 429 with Retry-After. Private bounded journals contain sanitized selection reasons, cache/token usage and host outcomes, without raw prompts, credentials or headers. A host cancellation does not independently prove provider failure. Corrupt/unwritable admission or billing state fails closed.

## Private diagnostics

Enable **Record routing events** (`record_routing_events`) to write `team-routing-diagnostics.jsonl` under the plugin's `state_dir`. It contains request-received, credential-selected, upstream-usage, request-complete and quota-read events. All providers using the native callbacks are covered, including requests that fail before account selection. Hashes of native request/trace IDs correlate attempts, final outcomes and concurrent requests; `selection_id` joins the existing ranking journal. One upstream callback can represent a retry, so callback counts must not be reported as independent client requests.

Diagnostics retain requested/effective/response model IDs, request shape (sizes and counts only), selected account, timing/TTFT, cache/token counters, available Retry-After/rate headers, and structured Google ErrorInfo/RetryInfo/QuotaFailure fields. Failure-time quota observations come from the existing cache and include its timestamp; logging never forces quota reads. Free-text error messages, raw error bodies, provider descriptions/subjects, tool definitions, prompts, cookies, arbitrary headers and credentials are omitted. Error body hashes and byte counts support grouping; `structured_details_available: false` explicitly means the provider supplied no recognized structured reason. A generic 429 cannot establish which provider limit was reached. This privacy tradeoff means arbitrary provider error text cannot be reconstructed from the diagnostic journal.

Each private journal rotates at 8 MiB with one previous generation. The authenticated state API includes `logging` with enabled/write-health status, per-file write counts, failure classes and timestamps since process start. Inspect it when expected records are missing. The host's independent raw error-log files may contain sensitive requests; this feature does not enable full native request logging or debug output. Preserve relevant diagnostic generations promptly during long workloads.

## Portable operation

Self-calls reuse the authenticated dashboard caller and its origin. If the origin cannot be reached internally, set `management_url` to the proxy's internal base URL. Non-browser clients may send `X-Team-Routing-Origin`; otherwise localhost:8317 is the fallback. Redirects and URL user-info are rejected. Reverse proxies must expose the standard root API/resource paths.

Preserve `team-routing.json`, `team-quota-cache.json`, `team-routing-admission.json`, paid-use fences and policy backups across upgrades. No management key is required on disk. The ABI shim derives from upstream's MIT example and Claude Remote Login; see UPSTREAM-LICENSE.


## Per-account reserves

Under **Policy > Accounts and reserves**, every discovered credential has an ownership selector. Credentials with normalized quota can inherit provider defaults or use custom percentages for reported five-hour/weekly windows. Other reported windows have their own floor and optional release-before-reset interval. Custom rules use the native credential ID, survive token refreshes and remain stored while disconnected. Reconnecting under a different ID uses defaults until configured again. Labels and matching email text cannot move a rule to a different credential.

Custom percentages replace the default percentages; they use the existing protected weekdays/hours, whole-session protection, weekly-release schedule and request safety buffers. Defaults still apply to other shared accounts. Owned accounts stay exempt unless explicitly assigned a custom rule. The master reserve-protection switch applies to both defaults and custom rules.

The policy field `account_reserves` maps credential IDs to pairs of `five_hour_remaining_floor` and `weekly_remaining_floor` (fractions from 0 to 0.9). Zero is explicit. `account_ownership` maps IDs to booleans; removing an entry restores defaults. `provider_reserves` stores each non-Claude provider's defaults and schedule; `shared_claude` remains the compatible Claude field. `window_reserves` references native group/window identities supplied by state. The schema endpoint supplies editor metadata and bounds. Omitted fields in older clients preserve existing rules; `{}` explicitly clears them. New entries must reference connected credentials/providers and observed quota windows.

Quota adapters currently normalize Claude, Codex and Antigravity. Ownership and model rules work for all connected providers. Other providers retain native availability/cooldowns and are marked provider-managed; percentage reserves cannot be guaranteed without quota data. Legacy `owned_emails` continues to match Claude only, while explicit credential ownership is provider-independent.

## Claude Code auto-mode classifier billing

The [official safeguard protocol](https://code.claude.com/docs/en/auto-mode-classifier-billing) requires genuine upstream `safeguard_results`, in addition to forwarding `safeguards` and preserving tool IDs. A Gemini/Antigravity override does not establish support for Anthropic's classifier billing exemption. Claude Code can fall back to its existing classifier requests; this notice alone does not establish paid credit use. Team Routing does not fabricate results, claim unsupported eligibility or disable safety checks. Model translation can also limit provider-specific protocol features.
