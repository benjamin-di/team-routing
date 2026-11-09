(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const apiBase = '/v0/management/plugins/team-routing';
  const controls = new Map(), routes = new Map();
  let subscriptions = null;
  let state = null, specification = null, basePolicy = null;
  let dirty = false, saving = false, loading = false, timer = null;
  const clone = value => JSON.parse(JSON.stringify(value));
  const get = (object, path) => path.split('.').reduce((value, key) => value?.[key], object);
  function put(object, path, value) {
    const parts = path.split('.'); let node = object;
    for (const part of parts.slice(0, -1)) node = node[part] ||= {};
    node[parts.at(-1)] = value;
  }
  function el(tag, text, className) {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (className) node.className = className;
    return node;
  }
  function option(select, value, label) { const item = el('option', label); item.value = value; select.append(item); }
  function notice(message, kind = '') { $('notice').textContent = message; $('notice').className = 'notice ' + kind; }
  function changed() { dirty = true; $('dirty-state').textContent = 'Unsaved changes'; $('save').disabled = saving; }
  function tab(name) {
    document.querySelectorAll('[data-tab]').forEach(button => button.setAttribute('aria-selected', String(button.dataset.tab === name)));
    document.querySelectorAll('[role=tabpanel]').forEach(panel => panel.hidden = panel.id !== 'panel-' + name);
  }
  function date(value) {
    const d = new Date(value); if (!value || Number.isNaN(+d) || d.getFullYear() < 2000) return 'Not started';
    const zone = state?.policy.shared_claude?.timezone;
    return d.toLocaleString(undefined, zone ? { timeZone: zone, timeZoneName: 'short' } : { timeZoneName: 'short' });
  }
  function percent(value) { return Number.isFinite(value) ? Math.max(0, value * 100).toFixed(0) + '%' : '—'; }
  function badge(status) {
    const labels = { eligible: 'Available', blocked: 'Protected / unavailable', disabled: 'Disabled', provider_managed: 'Provider managed' };
    return el('span', labels[status] || status, 'badge ' + (status === 'eligible' ? 'good' : status === 'blocked' ? 'warn' : 'off'));
  }
  async function api(path, method = 'GET', body) {
    const key = $('management-key').value.trim(); if (!key) throw Error('Enter the management key.');
    const response = await fetch(apiBase + '/' + path, {
      method, headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json', 'X-Team-Routing-Origin': location.origin },
      body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(20000)
    });
    let value; try { value = await response.json(); } catch { throw Error('The proxy returned an unexpected response.'); }
    if (!response.ok) throw Error(value.error || 'Request failed (' + response.status + ').');
    return value;
  }
  function metric(parent, value, label) {
    const item = el('div', undefined, 'metric'); item.append(el('strong', value), el('span', label)); parent.append(item);
  }
  function bucketCell(buckets) {
    const cell = el('td'); if (!buckets.length) { cell.textContent = '—'; return cell; }
    for (const bucket of buckets) {
      const row = el('div'); row.append(el('strong', percent(bucket.remainingFraction)));
      const meter = el('div', undefined, 'meter'); const fill = el('span');
      fill.style.width = Math.max(0, Math.min(100, bucket.remainingFraction * 100)) + '%'; meter.append(fill);
      row.append(meter, el('div', (buckets.length > 1 ? bucket.Window + ' · ' : '') + date(bucket.resetTime), 'muted')); cell.append(row);
    }
    return cell;
  }
  function accountDetails(account) {
    $('account-title').textContent = account.Email || account.Name || account.ID;
    const root = $('account-detail'); root.replaceChildren(); root.append(badge(account.decision.status), el('p', account.decision.detail));
    const list = el('dl', undefined, 'detail-list');
    const add = (label, value) => list.append(el('dt', label), el('dd', value));
    add('Provider', account.Provider); add('Advertised models', (account.models || []).join(', ') || 'None'); add('Last quota reading', date(account.Snapshot?.At)); add('Next due refresh', date(account.Cache?.next_refresh));
    add('Refresh state', account.Cache?.message || account.Cache?.state || 'Not loaded');
    if (account.reserves) add('Configured reserves', account.reserves.enabled ? percent(account.reserves.five_hour_remaining_floor) + ' five-hour / ' + percent(account.reserves.weekly_remaining_floor) + ' weekly · ' + (account.reserves.source === 'account' ? 'Custom' : 'Defaults') : account.reserves.source === 'owned_exempt' ? 'Owned account: exempt' : 'Reserve protection disabled');
    for (const release of account.weekly_reserve_releases || []) add(release.window + ' reserve', (release.locked ? 'Unlocks ' : 'Unlocked ') + date(release.release));
    const extra = account.Snapshot?.extra_usage;
    if (extra) add('Provider credits', extra.is_enabled === true ? 'Enabled at provider' : extra.is_enabled === false ? 'Disabled at provider' : 'Unknown');
    root.append(list); const detail = el('details');
    detail.append(el('summary', 'Numeric assessment and quota windows'), el('pre', JSON.stringify({ quota: account.Snapshot, assessment: account.Deadline || account.decision, native_retry_after: account.next_retry_after }, null, 2)));
    root.append(detail); $('account-dialog').showModal();
  }
  function renderOverview() {
    const metrics = $('overview-metrics'); metrics.replaceChildren(); const accounts = state.accounts || [];
    metric(metrics, accounts.filter(a => a.decision?.status === 'eligible').length, 'Accounts with available measured quota');
    metric(metrics, accounts.reduce((sum, a) => sum + (a.ActiveRequests || 0), 0), 'Active requests'); metric(metrics, state.client_details.length, 'Active client keys');
    const filter = $('account-search').value.trim().toLowerCase(), body = $('accounts'); body.replaceChildren();
    for (const account of accounts.filter(a => [a.Email, a.Name, a.Provider].join(' ').toLowerCase().includes(filter))) {
      const row = el('tr'), identity = el('td');
      identity.append(el('div', account.Email || account.Name || account.ID, 'account-name'), el('div', account.Provider + (account.Owned ? ' · Owned' : '') + ' · ' + (account.ActiveRequests || 0) + ' active', 'muted'));
      const buckets = (account.Snapshot?.Groups || []).flatMap(g => g.Buckets || []), status = el('td');
      status.append(badge(account.decision.status), el('div', account.decision.detail, 'status-detail'));
      const details = el('td'), button = el('button', 'Details'); button.onclick = () => accountDetails(account); details.append(button);
      row.append(identity, bucketCell(buckets.filter(b => b.Window === 'five_hour')), bucketCell(buckets.filter(b => b.Window === 'seven_day' || b.Window === 'weekly')), status, details); body.append(row);
    }
    if (!body.children.length) { const row = el('tr'), cell = el('td', 'No matching accounts.', 'empty'); cell.colSpan = 5; row.append(cell); body.append(row); }
  }
  function renderPolicy() {
    controls.clear(); $('policy-fields').replaceChildren(); $('advanced-fields').replaceChildren();
    for (const group of specification.groups) for (const advanced of [false, true]) {
      const fields = specification.fields.filter(field => !field.provider_scoped && field.group === group.id && (Boolean(group.advanced) || Boolean(field.advanced)) === advanced);
      if (!fields.length) continue;
      const card = el('fieldset', undefined, 'card');
      card.append(el('legend', group.title)); const grid = el('div', undefined, 'field-grid');
      for (const field of fields) {
        const value = get(basePolicy, field.path), wrap = el('div', undefined, 'field' + (field.type === 'array' ? ' wide' : ''));
        const id = 'setting-' + field.path.replaceAll('.', '-'), label = el('label', field.title); label.htmlFor = id; let input;
        if (field.format === 'weekdays') {
          input = el('div', undefined, 'weekdays');
          ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].forEach((day, index) => {
            const item = el('label', day), check = el('input'); check.type = 'checkbox'; check.value = index; check.checked = (value || []).includes(index);
            check.setAttribute('aria-label', field.title + ': ' + day); check.onchange = changed; item.prepend(check); input.append(item);
          });
        } else if (field.type === 'boolean') { input = el('input'); input.type = 'checkbox'; input.checked = !!value; input.onchange = changed; }
        else if (field.enum) { input = el('select'); field.enum.forEach(item => option(input, item, field.enum_labels?.[item] || item)); input.value = value ?? field.enum[0]; input.onchange = changed; }
        else if (field.type === 'array') { input = el('textarea'); input.value = (value || []).join('\n'); input.oninput = changed; }
        else {
          input = el('input'); input.type = ['integer', 'number'].includes(field.type) ? 'number' : 'text'; const scale = field.format === 'percent' ? 100 : 1;
          input.value = value == null ? '' : typeof value === 'number' ? Number((value * scale).toFixed(6)) : value;
          if (field.minimum != null) input.min = field.minimum * scale; if (field.maximum != null) input.max = field.maximum * scale;
          input.step = field.type === 'integer' || scale === 100 ? '1' : '0.1'; input.oninput = () => { changed(); if(field.group==='reserves') renderAccountReserves(true); };
        }
        input.id = id;
        if (field.type === 'boolean') { const line = el('div', undefined, 'check-field'); line.append(input, label); wrap.append(line); } else wrap.append(label, input);
        wrap.append(el('p', field.description, 'help')); grid.append(wrap); controls.set(field.path, { field, input });
      }
      card.append(grid); (advanced ? $('advanced-fields') : $('policy-fields')).append(card);
    }
    $('policy-zone').textContent = 'Proxy time: ' + state.system_timezone; $('engine-parameters').textContent = JSON.stringify(specification.engine, null, 2);
    renderAccountReserves();
  }
  function renderAccountReserves(preserve = false) {
    subscriptions = window.TeamRoutingEditors.subscriptionEditor($('provider-reserves'),$('account-reserves'),state,basePolicy,specification,changed,preserve?subscriptions?.draft():null);
  }
  function renderRules() {
    $('rule-list').replaceChildren();
    for (const rule of state.rules || []) {
      const item = el('li'), title = el('h3', rule.title); title.append(el('span', rule.scope + (rule.enabled ? '' : ' · Disabled'), 'scope')); item.append(title, el('p', rule.summary));
      if (rule.parameters) { const details = el('details'); details.append(el('summary', 'Effective values'), el('pre', JSON.stringify(rule.parameters, null, 2))); item.append(details); }
      $('rule-list').append(item);
    }
  }
  function renderRoutes() {
    routes.clear(); $('client-routes').replaceChildren();
    $('catalog-warning').hidden = !state.catalog.warning; $('catalog-warning').textContent = state.catalog.warning || '';
    for (const client of state.client_details) {
      const editor = window.TeamRoutingEditors.clientEditor(client,basePolicy.routes?.[client.id],basePolicy.model_routes?.[client.id],()=>state.catalog,changed);
      editor.label.value=basePolicy.client_labels?.[client.id]||''; routes.set(client.id,editor); $('client-routes').append(editor.root);
    }
  }
  function renderDiagnostics() {
    const metrics = $('cache-metrics'); metrics.replaceChildren(); const cache = state.cache_efficiency || {}, totals = cache.totals || {};
    metric(metrics, totals.measured_requests ? percent(totals.cache_read_share) : '—', 'Measured input read from cache');
    metric(metrics, (totals.cache_creation_tokens || 0).toLocaleString(), 'Cache-write tokens'); metric(metrics, cache.account_switches || 0, 'Recorded account switches');
    const rows = $('decisions'); rows.replaceChildren();
    for (const decision of (cache.recent_decisions || []).slice(-12).reverse()) { const row = el('tr'), account = state.accounts.find(a => a.ID === decision.auth_id); row.append(el('td', date(decision.at)), el('td', decision.model || '—'), el('td', account?.Email || account?.Name || decision.auth_id || 'No eligible account'), el('td', (decision.reason || '').replaceAll('_', ' '))); rows.append(row); }
    const selectedClient = $('evaluate-client').value, selectedModel = $('evaluate-model').value;
    $('evaluate-client').replaceChildren(); option($('evaluate-client'), '', 'Requested model only'); state.client_details.forEach(c => option($('evaluate-client'), c.id, c.name)); $('evaluate-client').value = selectedClient;
    $('evaluate-model').replaceChildren(); option($('evaluate-model'), '', 'Choose a requested model'); state.catalog.models.forEach(m => option($('evaluate-model'), m.id, m.id)); $('evaluate-model').value = selectedModel;
  }
  async function load(reset = false) {
    if (loading) return; loading = true;
    try {
      const next = await api('state'), changedKeys = state && JSON.stringify(next.clients) !== JSON.stringify(state.clients), changedPolicy = basePolicy && next.policy.revision !== basePolicy.revision, changedCatalog = state && JSON.stringify(state.catalog) !== JSON.stringify(next.catalog), changedAccounts = state && JSON.stringify(next.accounts.map(a=>[a.ID,a.Provider,a.Email,a.Owned,a.quota_features,a.window_options]))!==JSON.stringify(state.accounts.map(a=>[a.ID,a.Provider,a.Email,a.Owned,a.quota_features,a.window_options]));
      state = next; if (!specification || specification.version !== next.version) specification = await api('schema');
      if (reset || !basePolicy || (!dirty && (changedPolicy || changedKeys))) { basePolicy = clone(next.policy); renderPolicy(); renderRoutes(); dirty = false; $('dirty-state').textContent = 'Settings saved'; $('save').disabled = true; }
      else if (changedCatalog) { if (!dirty) renderRoutes(); else routes.forEach(row => row.refreshCatalog()); }
      if(changedAccounts) renderAccountReserves(dirty);
      if (dirty && (changedPolicy || changedKeys)) notice('Settings or active keys changed elsewhere. Reload before saving your edits.', 'error');
      renderOverview(); renderRules(); renderDiagnostics(); $('version').textContent = 'v' + next.version; $('connection').textContent = 'Connected'; $('connection').className = 'badge good'; $('updated').textContent = 'Last checked ' + date(next.now); $('app').hidden = false; $('auth').hidden = true;
      if (!timer) timer = setInterval(() => { if (!saving) load().catch(error => notice(error.message, 'error')); }, specification.ui.refresh_seconds * 1000);
    } finally { loading = false; }
  }
  function collectPolicy() {
    const result = clone(basePolicy); delete result.cutoff_used; delete result.expiring_used_below; delete result.expiring_within_seconds;
    for (const { field, input } of controls.values()) {
      let value;
      if (field.format === 'weekdays') value = [...input.querySelectorAll('input:checked')].map(item => Number(item.value));
      else if (field.type === 'boolean') value = input.checked;
      else if (field.type === 'array') value = input.value.split(/[\n,]+/).map(item => item.trim()).filter(Boolean);
      else if (['integer', 'number'].includes(field.type)) {
        value = Number(input.value); const scale = field.format === 'percent' ? 100 : 1;
        if (input.value === '' || !Number.isFinite(value) || (field.minimum != null && value < field.minimum * scale) || (field.maximum != null && value > field.maximum * scale) || (field.type === 'integer' && !Number.isInteger(value))) throw Error('Check ' + field.title + '.');
        value /= scale;
      } else value = input.value.trim();
      put(result, field.path, value);
    }
    result.routes = {}; result.client_labels = {}; result.model_routes = {};
    Object.assign(result,subscriptions.read());
    for(const [id,row] of routes) {
      const value=row.read(); result.client_labels[id]=value.label;
      if(value.route)result.routes[id]=value.route;
      if(Object.keys(value.rules).length)result.model_routes[id]=value.rules;
    }
    return result;
  }
  $('connect').onclick = async () => { $('connect').disabled = true; try { await load(true); notice('Connected. Account status and policy are loaded from the proxy.', 'success'); } catch (error) { notice(error.message, 'error'); } finally { $('connect').disabled = false; } };
  $('management-key').onkeydown = event => { if (event.key === 'Enter') $('connect').click(); };
  document.querySelectorAll('[data-tab]').forEach(button => button.onclick = () => tab(button.dataset.tab));
  $('account-search').oninput = () => { if (state) renderOverview(); }; $('close-account').onclick = () => $('account-dialog').close();
  $('reload').onclick = async () => { if (dirty && !confirm('Discard unsaved changes and reload settings?')) return; try { await load(true); notice('Loaded the saved policy.', 'success'); } catch (error) { notice(error.message, 'error'); } };
  $('refresh').onclick = async () => { $('refresh').disabled = true; try { const result = await api('refresh', 'POST', {}); notice(result.message); await load(); } catch (error) { notice(error.message, 'error'); } finally { $('refresh').disabled = false; } };
  $('save').onclick = async () => { if (!dirty || saving) return; saving = true; $('save').disabled = true; try { const result = await api('policy', 'PUT', collectPolicy()); basePolicy = clone(result.policy); dirty = false; await load(true); notice('Saved. New requests use the updated policy.', 'success'); } catch (error) { notice(error.message, 'error'); } finally { saving = false; $('save').disabled = !dirty; } };
  $('evaluate').onclick = async () => {
    $('evaluate').disabled = true;
    try {
      const result = await api('evaluate', 'POST', { client_id: $('evaluate-client').value, model: $('evaluate-model').value }), root = $('evaluation'); root.replaceChildren();
      root.append(el('h3', result.eligible_count + ' of ' + result.matching_count + ' matching accounts have usable cached quota'), el('p', 'Effective model: ' + result.model + (result.provider ? ' · ' + result.provider : '')));
      if(result.note)root.append(el('p',result.note));
      root.append(el('p','Routing rule: '+(result.routing_reason||'requested model')));
      if(result.protocol_note)root.append(el('p',result.protocol_note));
      for (const candidate of result.candidates) { const card = el('div', undefined, 'card'); card.append(el('strong', candidate.label), el('p', candidate.reason)); root.append(card); }
      if (!result.matching_count) root.append(el('p', 'No runtime credential advertises this provider/model pair.'));
    } catch (error) { notice(error.message, 'error'); } finally { $('evaluate').disabled = false; }
  };
})();
