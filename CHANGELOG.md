# Changelog

## Unreleased

- Retry transient quota-read rate limits after 30 seconds instead of imposing a 15-minute minimum admission outage. Repeated failures still back off exponentially up to two hours, longer provider retry delays (including Antigravity `google.rpc.RetryInfo`) remain authoritative, and stale quota/billing observations remain blocked. Avoid adding a minimum worker sleep after a slow scan leaves a retry overdue.

## 1.8.2

- Capture correlated request summaries, credential selections, every delivered upstream usage/failure callback, final outcomes and quota-read failures in a bounded private diagnostic journal. Preserve requested/effective models, timing, token/cache counts, structured provider quota reasons and retry hints without raw prompts, tools, credentials or error bodies.
- Expose journal write health in the management state API; report write/rotation failures instead of silently dropping them. Existing routing-event logging controls diagnostics as well. No change to routing decisions or native retry policy.

## 1.8.1

- Make shared quota-window normalization compile on every platform by removing the unintended Windows-only filename suffix.
- Run regression and dashboard checks on pushes and pull requests, and require them before release builds.

## 1.8.0

- Spread parallel requests across the least-active eligible accounts before reset ranking. Busy-account affinity yields without adding a hard concurrency quota or an uncancellable queue.
- Stop treating account-wide native error flags as verified cooldowns for every model. Quota preflight remains strict; the host enforces actual model-specific cooldowns at selection.
- Do not debit subscription estimates for rejected, unmetered requests. Preserve measured partial work and conservative accounting for ambiguous outcomes.
- Normalize Antigravity `5h` windows in fresh and cached data, including rate keys; retain legacy explicit window reserves.
- Add a searchable requested-model dropdown alongside optional manual IDs. Preserve custom IDs, filtering and unsaved selections across catalog refreshes.

## 1.7.1

- Delegate conversation and subagent identity extraction to the native host's canonical session metadata. Remove duplicate header/body/legacy identity parsing and the private affinity-header handoff.
- Preserve caller/provider/model isolation, quota-aware cache retention, reset urgency, reserves and admission safeguards. Native affinity, tiers and weights remain fallback host features, not a replacement for quota-aware scheduling.
- Remove an unused legacy Claude ownership-priority helper and document the native/plugin responsibility boundary.
- Verify the delegation on official CLIProxyAPI 8.0.51; scheduling hosts must supply canonical session metadata for per-conversation affinity.

## 1.7.0

- Add optional exact requested-model rules per client: preserve selected requested models or redirect selected models to any advertised provider/model. Exact rules precede the client default and resolve once, without chaining.
- Add explicit ownership for every connected credential, provider-specific reserve schedules and native daily/hourly quota-window reserves. Preserve legacy Claude email ownership and existing routing settings.
- Extend quota-aware ranking and completed-request accounting to normalized Codex and Antigravity windows. Providers without a quota adapter remain explicitly provider-managed; no five-hour allowance is invented.
- Drive the new editors, bounds, capability visibility and evaluated precedence from the backend contract. Preserve disconnected preferences and fields omitted by older clients.
- Explain Claude Code safeguard compatibility without fabricating classifier verdicts or disabling checks.

## 1.6.0

- Add per-credential Claude five-hour and weekly reserves, editable in Policy > Account reserves.
- Inherit shared defaults or explicitly customize either owned or shared accounts; reuse the existing protection and release schedules.
- Apply the same effective percentages to admission, weekly pacing, future five-hour opportunities, paid-fence recovery and diagnostics.
- Validate both values through backend-supplied bounds, retain disconnected rules and preserve them for older API clients that omit the new field.
- Keep credential identity separate from labels and emails; reconnecting under a new credential ID requires reconfiguring its reserves.

## 1.5.0

- Rebuild the dashboard into account overview, client routing, policy and diagnostics sections, with separate embedded HTML, CSS and JavaScript.
- Generate dashboard settings and JSON Schema from a shared backend field registry. Publish evaluated rule precedence and versioned engine parameters.
- Discover active client keys and actual provider/model pairs dynamically, including custom providers. Validate overrides, preserve unsaved edits and reject stale saves.
- Add model-specific, request-free availability diagnostics and native cooldown/model-support status.
- Schedule included quota against weekly resets, remaining five-hour opportunities and observed demand, with measured prompt-cache affinity and exact Claude ownership matching.
- Protect shared weekly/five-hour reserves using configurable time zones, weekdays and reset-based release schedules.
- Persist active/completed admission estimates and numeric response-quota constraints; reconcile fresh readings, deduplicate lifecycle/usage callbacks and preserve safety across restarts.
- Scope family-specific limits correctly, recover old paid-use fences after confirmed renewed included quota and retain strict/best-effort billing controls.
- Preserve generic public capacity errors while retaining sanitized private diagnostic evidence.

Validation: 87 Go/ABI regression tests, all editable dashboard controls, isolated official-host protocol/streaming/admission tests and a live Claude Messages smoke test. Windows runtime verified; other platforms require runtime testing after their release builds.
