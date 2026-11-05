import { GateAPI, APIError } from './api.js';
import { confirmAction, dismissConfirmation } from './confirm.js';

const api = new GateAPI();
const root = document.getElementById('tabsgate');
const content = document.getElementById('tg-content');
const connectionLabel = document.getElementById('tg-connection');
const alertBox = document.getElementById('tg-error');
const feedback = document.getElementById('tg-feedback');
const numbers = new Intl.NumberFormat('en-GB');
const encoder = new TextEncoder();
let generation = 0;
let focusNext = false;
const state = {
  screen: 'entry', providers: [], provider: '', apiKey: '', pass: '', connection: null,
  keys: [], keysLoaded: false, models: [], modelsLoaded: false, modelsLoading: false,
  modelError: '', selected: null, editing: null, draft: null, connectionDraft: null, issued: null,
  busy: false, pending: '', error: '', notice: '', confirm: false, createUncertain: false,
};

const escape = value => String(value ?? '').replace(/[&<>"']/g,
  character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[character]));
const compact = value => value >= 1000000 ? numbers.format(value / 1000000) + 'M'
  : value >= 1000 ? numbers.format(value / 1000) + 'K' : numbers.format(value);
const shortDate = value => new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short' }).format(new Date(value));
const fullDate = value => new Intl.DateTimeFormat('en-GB', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value));
const selectedKey = () => state.keys.find(key => key.id === state.selected);
const providerName = () => state.providers.find(provider => provider.id === state.provider)?.name || state.provider;
const isRevoked = key => key.state === 'key_revoked';
const localDate = value => {
  const date = new Date(value);
  const pad = part => String(part).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
};

function errorText(error) {
  if (error.uncertain) return 'The key may have been created, but its response was lost. Return to the list and refresh before creating another. Revoke any key whose value you did not receive.';
  const messages = {
    invalid_credentials: 'The key or pass is invalid. Check the selected provider too.',
    invalid_key: 'The key is invalid.',
    invalid_keyword: 'Enter a valid pass.',
    invalid_provider: 'This provider is no longer available. Reload the page.',
    invalid_limits: 'Check the models, limits and expiry. At least one call, one token and one model are required.',
    connection_closed: 'Reopen the connection with your provider key and pass.',
    connection_update_uncertain: 'The connection may have been updated, but its response was lost. If the current credentials stop working, sign out and reopen it with the new key and pass.',
    configuration_changed: 'The configuration changed. Refresh the list and try again.',
    connection_exists: 'The connection was just created. Select Open to reopen it.',
    replacement_connection_exists: 'This API key is already linked to another connection.',
    key_not_found: 'This key no longer exists. Refresh the list.',
    key_revoked: 'This key has been revoked and cannot be restored.',
    key_capacity_reached: 'This connection has reached its virtual key limit.',
    capacity_reached: 'The gateway is at capacity. Try again later.',
    rate_limited: `Too many requests. Try again in ${Math.max(2, error.retryAfter || 0)} seconds.`,
    network_error: 'The gateway is unreachable. Check your connection and try again.',
    request_timeout: 'The gateway did not respond in time. Try again.',
    invalid_response: 'The gateway returned an invalid response. Reload the page.',
    no_providers: 'No providers are available on this gateway.',
    upstream_error: 'The provider rejected the request. Check your API key.',
    upstream_unavailable: 'The provider is currently unreachable.',
    not_ready: 'The gateway is not ready. Try again shortly.',
    origin_denied: 'This page is not allowed to contact the gateway.',
  };
  return messages[error.code] || 'The operation could not be completed. Try again.';
}

function showScreen(screen) {
  state.screen = screen;
  state.confirm = false;
  state.error = '';
  state.notice = '';
  focusNext = true;
}

async function perform(pending, operation) {
  if (state.busy) return;
  const context = generation;
  state.busy = true;
  state.pending = pending;
  state.error = '';
  state.notice = '';
  render();
  try { await operation(); }
  catch (error) {
    if (context !== generation) return;
    state.error = errorText(error);
    if (error.uncertain) state.createUncertain = true;
  } finally {
    if (context === generation) {
      state.busy = false;
      state.pending = '';
      render();
      if (state.error) alertBox.focus();
    }
  }
}

function clearConnection() {
  dismissConfirmation();
  generation++;
  api.close();
  Object.assign(state, {
    screen: 'entry', apiKey: '', pass: '', connection: null, keys: [], keysLoaded: false,
    models: [], modelsLoaded: false, modelsLoading: false, modelError: '', selected: null,
    editing: null, draft: null, connectionDraft: null, issued: null, busy: false, pending: '', error: '', notice: '',
    confirm: false, createUncertain: false,
  });
  focusNext = true;
  render();
}

function keyStatus(key) {
  const current = key.state === 'active' && Date.parse(key.expires_at) <= Date.now() ? 'key_expired' : key.state;
  return {
    active: ['Active', '', ''],
    key_expired: ['Expired', 'is-exhausted', 'This key has expired.'],
    key_revoked: ['Revoked', 'is-revoked', 'Revocation is permanent.'],
    request_limit_reached: ['Exhausted', 'is-exhausted', 'The call limit has been reached.'],
    token_budget_exhausted: ['Exhausted', 'is-exhausted', 'The token threshold has been reached.'],
  }[current] || ['Unavailable', 'is-revoked', 'Refresh usage.'];
}

function statusHTML(key) {
  const [label, css] = keyStatus(key);
  return `<span class="tg-status ${css}">${label}</span>`;
}

const backButton = () => '<button type="button" class="tg-back" data-action="back">← Virtual keys</button>';
const option = (value, label, chosen) => `<option value="${escape(value)}"${String(value) === String(chosen) ? ' selected' : ''}>${escape(label)}</option>`;
const pendingLabel = label => escape(state.busy ? state.pending : label);

function render() {
  connectionLabel.innerHTML = state.connection
    ? `<div class="tg-provider-settings"><span>${escape(providerName())}</span><button type="button" class="tg-text-button" data-action="connection">Connection</button></div><button type="button" class="tg-text-button tg-signout" data-action="signout">Sign out</button>` : '';
  alertBox.hidden = !state.error;
  alertBox.textContent = state.error;
  feedback.textContent = state.notice;
  root.setAttribute('aria-busy', String(state.busy));
  if (state.screen === 'entry') renderEntry();
  else if (state.screen === 'list') renderList();
  else if (state.screen === 'form') renderForm();
  else if (state.screen === 'detail') renderDetail();
  else if (state.screen === 'success') renderSuccess();
  else if (state.screen === 'connection') renderConnection();
  if (state.busy) root.querySelectorAll('button, input, select, textarea').forEach(control => { if (!control.closest('dialog')) control.disabled = true; });
  if (focusNext && !state.busy) {
    const target = state.screen === 'entry' ? content.querySelector('#tg-original') : content.querySelector('h2, h3');
    if (target) { if (target.tagName !== 'INPUT') target.setAttribute('tabindex', '-1'); target.focus(); }
    focusNext = false;
  }
}

function renderEntry() {
  content.innerHTML = `<form class="tg-entry" id="tg-open-form">
    <h2>Your virtual keys.</h2><p class="tg-subtitle">Your key, with the limits you choose.</p>
    <div class="tg-provider-line"><label for="tg-provider">Provider</label>${state.providers.length > 1
      ? `<select id="tg-provider" name="provider">${state.providers.map(provider => option(provider.id, provider.name, state.provider)).join('')}</select>`
      : `<span>${escape(providerName() || 'Loading…')}</span>`}</div>
    <div class="tg-field"><label for="tg-original">Provider API key</label><div class="tg-secret-field"><input id="tg-original" name="apiKey" type="password" value="${escape(state.apiKey)}" required maxlength="4096" autocomplete="off" spellcheck="false" autocapitalize="off"><button type="button" data-action="reveal" data-target="tg-original" aria-label="Show API key">Show</button></div></div>
    <div class="tg-field"><label for="tg-pass">Pass</label><div class="tg-secret-field"><input id="tg-pass" name="pass" type="password" value="${escape(state.pass)}" required maxlength="256" autocomplete="off" spellcheck="false" autocapitalize="off" aria-describedby="tg-pass-help"><button type="button" data-action="reveal" data-target="tg-pass" aria-label="Show pass">Show</button></div><p class="tg-hint" id="tg-pass-help">Choose a pass the first time. Use the same one to reopen this connection. There is no recovery.</p></div>
    <button type="submit" class="tg-button tg-primary tg-full"${!state.providers.length ? ' disabled' : ''}>${pendingLabel('Open')}</button>
    ${!state.providers.length && !state.busy ? '<button type="button" class="tg-text-button" data-action="providers">Retry loading providers</button>' : ''}
    <p class="tg-explainer">No account needed.</p>
  </form>`;
}

function renderList() {
  content.innerHTML = `<div class="tg-body"><div class="tg-title-row"><h3>Virtual keys${state.keysLoaded ? `<span class="tg-count">${state.keys.length}</span>` : ''}</h3><div class="tg-action-group"><button type="button" class="tg-text-button" data-action="refresh">${state.busy ? escape(state.pending) : 'Refresh'}</button><button type="button" class="tg-button tg-primary" data-action="create"${!state.keysLoaded || state.createUncertain ? ' disabled' : ''}>+ New key</button></div></div>
    ${!state.keysLoaded ? '<p class="tg-blank-error">Refresh to load the virtual keys for this connection.</p>' : !state.keys.length
      ? '<p class="tg-empty">Create your first virtual key here.</p>'
      : [...state.keys].reverse().map(key => `<button type="button" class="tg-key-row" data-action="detail" data-id="${escape(key.id)}"><span><span class="tg-key-name">${escape(key.name || key.prefix + '…')}</span><span class="tg-row-sub">${numbers.format(key.requests_used)} / ${numbers.format(key.max_requests)} calls</span>${key.in_flight ? '<span class="tg-inflight">Generation in progress</span>' : ''}</span><span class="tg-row-right">${statusHTML(key)}<span class="tg-row-sub">Expires ${shortDate(key.expires_at)}</span></span><span class="tg-chevron" aria-hidden="true">›</span></button>`).join('')}
  </div>`;
}

function readDraft(form) {
  const data = new FormData(form);
  return {
    name: String(data.get('name') || ''), model: String(data.get('model') || ''),
    requests: String(data.get('requests') || ''), tokens: String(data.get('tokens') || ''),
    expiry: String(data.get('expiry') || ''), customExpiry: form.elements.customExpiry.value,
    output: String(data.get('output') || ''), extraModels: String(data.get('extraModels') || ''),
    optionsOpen: form.querySelector('details').open,
  };
}

function renderForm() {
  const key = state.keys.find(item => item.id === state.editing);
  const draft = state.draft;
  content.innerHTML = `<div class="tg-body"><form class="tg-form" id="tg-key-form">${backButton()}<h2>${key ? 'Edit key' : 'New virtual key'}</h2>
    <div class="tg-field"><label for="tg-name">Name <span class="tg-label-note">(optional)</span></label><input id="tg-name" name="name" maxlength="160" value="${escape(draft.name)}" placeholder="e.g. Client demo" autocomplete="off"></div>
    <div class="tg-field"><label for="tg-model">Allowed model</label><input id="tg-model" name="model" list="tg-model-options" value="${escape(draft.model)}" placeholder="Choose or enter a model ID" maxlength="200" autocomplete="off" autocapitalize="off" spellcheck="false" required aria-describedby="tg-model-feedback"><datalist id="tg-model-options"></datalist><div id="tg-model-feedback" class="tg-hint tg-model-help" role="status"></div></div>
    <div class="tg-two"><div class="tg-field"><label for="tg-requests">Calls</label><input id="tg-requests" name="requests" type="number" min="1" max="1000000" step="1" value="${escape(draft.requests)}" required></div><div class="tg-field"><label for="tg-tokens">Total tokens</label><input id="tg-tokens" name="tokens" type="number" min="1" max="1000000000000" step="1" value="${escape(draft.tokens)}" required></div></div>
    <div class="tg-field"><label for="tg-expiry">Expiry</label><select id="tg-expiry" name="expiry">${key ? option('keep', 'Keep: ' + fullDate(key.expires_at), draft.expiry) : ''}${option('3600', 'In 1 hour', draft.expiry)}${option('86400', 'In 1 day', draft.expiry)}${option('604800', 'In 7 days', draft.expiry)}${option('2592000', 'In 30 days', draft.expiry)}${option('custom', 'Date and time…', draft.expiry)}</select><input id="tg-custom-expiry" name="customExpiry" type="datetime-local" value="${escape(draft.customExpiry)}" aria-label="Expiry date and time"${draft.expiry === 'custom' ? ' required' : ' hidden disabled'}></div>
    <details${draft.optionsOpen ? ' open' : ''}><summary>Options</summary><div class="tg-field"><label for="tg-output">Maximum output per call</label><input id="tg-output" name="output" type="number" min="1" max="16384" step="1" value="${escape(draft.output)}" placeholder="Gateway default"></div><div class="tg-field"><label for="tg-extra-models">Additional allowed models</label><textarea id="tg-extra-models" name="extraModels" rows="3" placeholder="One ID per line" spellcheck="false" autocapitalize="off">${escape(draft.extraModels)}</textarea></div></details>
    <p class="tg-hint tg-form-note">${key ? 'Recorded usage is preserved.' : 'Calls include model requests, tool calls and retries.'}</p>
    <div class="tg-form-actions"><button type="button" class="tg-button" data-action="cancel">Cancel</button><button type="submit" class="tg-button tg-primary"${state.createUncertain ? ' disabled data-uncertain' : ''}>${pendingLabel(key ? 'Save changes' : 'Create key')}</button></div>
  </form></div>`;
  updateModels();
}

function updateModels() {
  if (state.screen !== 'form') return;
  const options = content.querySelector('#tg-model-options');
  const help = content.querySelector('#tg-model-feedback');
  options.replaceChildren(...state.models.map(model => new Option(model, model)));
  help.replaceChildren();
  const description = document.createElement('span');
  description.textContent = state.modelsLoading ? 'Loading models…' : state.modelError
    ? 'Models are unavailable. You can enter a model ID manually.'
    : state.models.length ? 'Choose a model that supports text chat.' : 'Enter the ID of a model that supports text chat.';
  help.append(description);
  if (state.modelError) {
    const retry = document.createElement('button');
    retry.type = 'button'; retry.dataset.action = 'models'; retry.textContent = 'Retry';
    retry.disabled = state.busy;
    help.append(retry);
  }
}

async function loadModels() {
  if (state.modelsLoaded || state.modelsLoading || !state.connection) return;
  const context = generation;
  const revision = state.connection.revision;
  state.modelsLoading = true;
  state.modelError = '';
  updateModels();
  try {
    const models = await api.models();
    if (context !== generation || state.connection?.revision !== revision) return;
    state.models = models; state.modelsLoaded = true;
  } catch (error) {
    if (context !== generation || state.connection?.revision !== revision) return;
    state.modelError = errorText(error);
  } finally {
    if (context === generation && state.connection?.revision === revision) { state.modelsLoading = false; updateModels(); }
  }
}

function meter(label, used, ceiling, hint) {
  return `<div class="tg-meter"><div class="tg-meter-label"><span>${label}</span><strong>${compact(used)} <span>/ ${compact(ceiling)}</span></strong></div><progress aria-label="${label}" max="${ceiling}" value="${Math.min(used, ceiling)}"></progress>${hint ? `<p class="tg-hint">${hint}</p>` : ''}</div>`;
}

function renderDetail() {
  const key = selectedKey();
  if (!key) { state.screen = 'list'; renderList(); return; }
  const description = keyStatus(key)[2];
  content.innerHTML = `<div class="tg-body"><div class="tg-detail">${backButton()}<div class="tg-detail-head"><div><h2>${escape(key.name || 'Virtual key')}</h2><p class="tg-prefix">${escape(key.prefix)}…</p>${description ? `<p class="tg-state-detail">${description}</p>` : ''}</div>${statusHTML(key)}</div>
    ${meter('Calls', key.requests_used, key.max_requests, key.in_flight ? 'A generation is in progress. Refresh to see its final usage.' : '')}
    ${meter('Token', key.tokens_used, key.token_budget, 'Input + output threshold. The last response may exceed it.')}
    ${key.estimated_requests ? `<p class="tg-hint">${numbers.format(key.estimated_requests)} ${key.estimated_requests === 1 ? 'call counted' : 'calls counted'} with a conservative token estimate.</p>` : ''}
    <div class="tg-info-line"><span>Expiry</span><span>${fullDate(key.expires_at)}</span></div><div class="tg-info-line"><span>Models</span><span>${key.allowed_models.map(escape).join(', ')}</span></div>
    <div class="tg-detail-actions"><div class="tg-action-group">${isRevoked(key) ? '' : '<button type="button" class="tg-button" data-action="edit">Edit limits</button>'}<button type="button" class="tg-text-button" data-action="refresh">${state.busy ? escape(state.pending) : 'Refresh'}</button></div>${isRevoked(key) ? '' : '<button type="button" class="tg-danger" data-action="ask-revoke">Revoke key</button>'}</div>
    ${state.confirm ? `<div class="tg-confirm"><p>Permanently revoke this key?</p><div class="tg-confirm-actions"><button type="button" class="tg-button" data-action="cancel-revoke">Cancel</button><button type="button" class="tg-button tg-danger" data-action="revoke">${pendingLabel('Revoke')}</button></div></div>` : ''}
  </div></div>`;
}

function renderSuccess() {
  content.innerHTML = `<div class="tg-body"><div class="tg-detail"><p class="tg-success-label">Virtual key created</p><h2>Your key is ready.</h2><p class="tg-hint">Copy it now. It will not be shown again after you leave this screen.</p>
    <label for="tg-created-secret" class="tg-created-value">Virtual key</label><textarea id="tg-created-secret" class="tg-copy-value" rows="2" readonly spellcheck="false">${escape(state.issued)}</textarea><div class="tg-copy-row"><button type="button" class="tg-button tg-primary" data-action="copy-key">Copy key</button></div>
    <label for="tg-base-url" class="tg-base-url">Base URL for your client</label><input id="tg-base-url" class="tg-copy-value" value="${escape(api.baseURL)}" readonly spellcheck="false"><button type="button" class="tg-text-button" data-action="copy-url">Copy URL</button>
    <div class="tg-form-actions"><button type="button" class="tg-button" data-action="finish">Go to virtual keys</button></div>
  </div></div>`;
}

function renderConnection() {
  const draft = state.connectionDraft;
  content.innerHTML = `<div class="tg-body"><div class="tg-form">${backButton()}<h2>Connection</h2>
    <p class="tg-subtitle">${escape(providerName())}. Update your provider key or pass while keeping your virtual keys and their usage.</p>
    <form id="tg-connection-form" autocomplete="off">
      <div class="tg-field"><label for="tg-replacement-key">Replacement provider API key</label><div class="tg-secret-field"><input id="tg-replacement-key" name="apiKey" type="password" value="${escape(draft.apiKey)}" maxlength="4096" autocomplete="off" spellcheck="false" autocapitalize="off" placeholder="Leave blank to keep the current key"><button type="button" data-action="reveal" data-target="tg-replacement-key" aria-label="Show replacement API key">Show</button></div></div>
      <div class="tg-field"><label for="tg-new-pass">New pass</label><div class="tg-secret-field"><input id="tg-new-pass" name="pass" type="password" value="${escape(draft.pass)}" maxlength="256" autocomplete="off" spellcheck="false" autocapitalize="off" placeholder="Leave blank to keep the current pass"><button type="button" data-action="reveal" data-target="tg-new-pass" aria-label="Show new pass">Show</button></div></div>
      <div class="tg-form-actions"><button type="button" class="tg-button" data-action="back">Cancel</button><button type="submit" class="tg-button tg-primary">${pendingLabel('Save changes')}</button></div>
    </form>
    <div class="tg-delete-connection"><p class="tg-hint">Remove this saved provider key and all its virtual keys from TabsGate.</p><button type="button" class="tg-danger" data-action="delete-connection">Delete connection</button></div>
  </div></div>`;
}

async function refreshKeys() {
  state.keys = await api.keys();
  state.keysLoaded = true;
  state.createUncertain = false;
  if (state.selected && !selectedKey()) showScreen('list');
}

function openForm(editing = null) {
  const key = state.keys.find(item => item.id === editing);
  if (key && isRevoked(key)) return;
  state.editing = editing;
  state.draft = {
    name: key?.name || '', model: key?.allowed_models[0] || '',
    requests: String(key?.max_requests ?? 30), tokens: String(key?.token_budget ?? 1000000),
    expiry: key ? 'keep' : '604800', customExpiry: localDate(key?.expires_at || Date.now() + 7 * 86400000),
    output: key ? String(key.max_output_tokens) : '', extraModels: key?.allowed_models.slice(1).join('\n') || '',
    optionsOpen: false,
  };
  showScreen('form'); render();
  void loadModels();
}

function limitsFromDraft(draft) {
  const models = [...new Set([draft.model.trim(), ...draft.extraModels.split(/\r?\n/).map(model => model.trim()).filter(Boolean)])];
  if (models.length > 100 || models.some(model => !/^[\x21-\x7e]{1,200}$/.test(model))) throw new Error('Enter 1 to 100 valid model IDs without spaces.');
  if (encoder.encode(draft.name).length > 160) throw new Error('The name is too long. The maximum is 160 bytes.');
  const limits = { name: draft.name, max_requests: Number(draft.requests), token_budget: Number(draft.tokens), allowed_models: models };
  if (draft.output !== '') limits.max_output_tokens = Number(draft.output);
  if (draft.expiry === 'custom') {
    const expires = new Date(draft.customExpiry);
    const difference = expires.getTime() - Date.now();
    if (!Number.isFinite(difference) || difference <= 0 || difference > 365 * 86400000) throw new Error('Choose a future expiry within one year.');
    limits.expires_at = expires.toISOString();
  } else if (draft.expiry !== 'keep') limits.ttl_seconds = Number(draft.expiry);
  // Keeping expiry omits both fields. An edit never restarts the TTL or resets usage.
  return limits;
}

function validationError(message) {
  state.error = message;
  alertBox.hidden = false; alertBox.textContent = message; alertBox.focus();
}

root.addEventListener('submit', event => {
  event.preventDefault();
  if (state.busy) return;
  const form = event.target;
  if (!form.reportValidity()) return;
  if (form.id === 'tg-connection-form') {
    const apiKey = form.elements.apiKey.value.trim();
    const pass = form.elements.pass.value;
    state.connectionDraft = { apiKey, pass };
    if (!apiKey && !pass) return validationError('Enter a replacement API key or a new pass.');
    if (apiKey && !/^[\x21-\x7e]{1,4096}$/.test(apiKey)) return validationError('Enter a valid API key without spaces.');
    if (pass && (!pass.trim() || pass !== pass.trim() || /[\x00-\x1f\x7f]/.test(pass) || encoder.encode(pass).length > 256)) return validationError('Enter a valid pass without leading or trailing spaces or control characters.');
    const changes = {};
    if (apiKey) changes.api_key = apiKey;
    if (pass) changes.keyword = pass;
    void perform('Saving…', async () => {
      try { state.connection = await api.updateConnection(changes); }
      catch (error) {
        if (error.code === 'connection_exists') error.code = 'replacement_connection_exists';
        if (['network_error', 'request_timeout', 'invalid_response'].includes(error.code) || error.status >= 500) {
          error.code = 'connection_update_uncertain';
        }
        throw error;
      }
      state.connectionDraft = null;
      state.models = []; state.modelsLoaded = false; state.modelsLoading = false; state.modelError = '';
      showScreen('list');
      state.notice = 'Connection updated. Your virtual keys and usage are preserved.';
    });
    return;
  }
  if (form.id === 'tg-open-form') {
    state.apiKey = form.elements.apiKey.value.trim();
    state.pass = form.elements.pass.value;
    state.provider = form.elements.provider?.value || state.provider;
    if (!/^[\x21-\x7e]{1,4096}$/.test(state.apiKey)) return validationError('Enter a valid API key without spaces.');
    if (!state.pass.trim() || state.pass !== state.pass.trim() || /[\x00-\x1f\x7f]/.test(state.pass) || encoder.encode(state.pass).length > 256) return validationError('The pass must contain 1 to 256 bytes, without leading or trailing spaces or control characters.');
    void perform('Opening…', async () => {
      state.connection = await api.open(state.provider, state.apiKey, state.pass);
      state.apiKey = ''; state.pass = '';
      showScreen('list');
      await refreshKeys();
    });
    return;
  }
  if (form.id === 'tg-key-form' && !state.createUncertain) {
    state.draft = readDraft(form);
    let limits;
    try { limits = limitsFromDraft(state.draft); } catch (error) { validationError(error.message); return; }
    const editing = state.editing;
    void perform(editing ? 'Saving…' : 'Creating…', async () => {
      if (editing) {
        const updated = await api.edit(editing, limits);
        state.keys = state.keys.map(key => key.id === editing ? updated : key);
        state.selected = editing;
        showScreen('detail');
        state.notice = 'Limits updated. Usage preserved.';
      } else {
        const issued = await api.create(limits);
        if (typeof issued.key !== 'string' || typeof issued.id !== 'string') throw new APIError('invalid_response', 200, 0, true);
        const { key, ...metadata } = issued;
        state.issued = key;
        state.keys.push(metadata);
        state.selected = metadata.id;
        showScreen('success');
      }
      state.draft = null; state.editing = null;
    });
  }
});

root.addEventListener('input', event => {
  const form = event.target.closest('form');
  if (form?.id === 'tg-key-form' && !state.busy) state.draft = readDraft(form);
  if (form?.id === 'tg-connection-form' && !state.busy) state.connectionDraft = { apiKey: form.elements.apiKey.value, pass: form.elements.pass.value };
});

root.addEventListener('change', event => {
  if (event.target.id === 'tg-expiry') {
    const custom = content.querySelector('#tg-custom-expiry');
    custom.hidden = event.target.value !== 'custom';
    custom.disabled = custom.hidden; custom.required = !custom.hidden;
    state.draft = readDraft(event.target.form);
  }
});

async function copyValue(id, message) {
  const field = document.getElementById(id);
  if (!field) return;
  try {
    if (!navigator.clipboard) throw new Error();
    await navigator.clipboard.writeText(field.value);
    state.notice = message;
  } catch {
    field.focus(); field.select();
    state.notice = 'Value selected. Use the Copy command on your device.';
  }
  feedback.textContent = state.notice;
}

root.addEventListener('click', async event => {
  const button = event.target.closest('button[data-action]');
  if (!button || state.busy) return;
  const action = button.dataset.action;
  if (action === 'reveal') {
    const input = document.getElementById(button.dataset.target);
    input.type = input.type === 'password' ? 'text' : 'password';
    button.textContent = input.type === 'password' ? 'Show' : 'Hide';
    button.setAttribute('aria-label', button.textContent + (input.id.endsWith('pass') ? ' pass' : ' API key'));
  } else if (action === 'signout') {
    const context = generation;
    if (await confirmAction({ heading: 'Sign out?', description: 'Your virtual keys will keep working. You will need your provider API key and pass to reopen this connection.', label: 'Sign out' }) && context === generation) clearConnection();
  } else if (action === 'connection') {
    if (state.screen === 'connection') return;
    state.issued = null;
    state.connectionDraft = { apiKey: '', pass: '' };
    showScreen('connection'); render();
  } else if (action === 'delete-connection') {
    const context = generation;
    const confirmed = await confirmAction({ heading: 'Delete connection?', description: 'This removes the saved provider API key, every virtual key and their usage records from TabsGate. It does not revoke your API key with the provider. This cannot be undone.', label: 'Delete connection', destructive: true });
    if (!confirmed || context !== generation) return;
    void perform('Deleting…', async () => {
      await api.deleteConnection();
      clearConnection();
      state.notice = 'Connection and all its virtual keys deleted.';
      render();
    });
  }
  else if (action === 'create') openForm();
  else if (action === 'edit') openForm(state.selected);
  else if (action === 'models') void loadModels();
  else if (action === 'providers') void loadProviders();
  else if (action === 'refresh') void perform('Refreshing…', refreshKeys);
  else if (action === 'detail') {
    state.selected = button.dataset.id;
    showScreen('detail');
    void perform('Refreshing…', refreshKeys);
  } else if (action === 'back' || action === 'cancel' || action === 'finish') {
    state.issued = null;
    const target = action === 'cancel' && state.editing ? 'detail' : 'list';
    state.editing = null; state.draft = null;
    state.connectionDraft = null;
    showScreen(target);
    if (state.createUncertain) void perform('Refreshing…', refreshKeys); else render();
  } else if (action === 'ask-revoke') {
    state.confirm = true; render();
    content.querySelector('[data-action="cancel-revoke"]').focus();
  } else if (action === 'cancel-revoke') {
    state.confirm = false; render();
    content.querySelector('[data-action="ask-revoke"]')?.focus();
  } else if (action === 'revoke') {
    const key = selectedKey();
    if (!key) return;
    void perform('Revoking…', async () => {
      await api.revoke(key.id);
      key.state = 'key_revoked'; state.confirm = false;
      state.notice = 'Key revoked.';
    });
  } else if (action === 'copy-key') void copyValue('tg-created-secret', 'Key copied.');
  else if (action === 'copy-url') void copyValue('tg-base-url', 'URL copied.');
});

root.addEventListener('keydown', event => {
  if (event.key !== 'Escape' || state.busy) return;
  if (document.getElementById('tg-confirm-dialog').open) return;
  if (state.confirm) content.querySelector('[data-action="cancel-revoke"]')?.click();
  else if (state.screen === 'form' || state.screen === 'detail' || state.screen === 'connection') content.querySelector('[data-action="back"]')?.click();
});

async function loadProviders() {
  await perform('Loading…', async () => {
    state.providers = await api.providers();
    if (!state.providers.some(provider => provider.id === state.provider)) state.provider = state.providers[0].id;
    focusNext = true;
  });
}

// A reload or a closed page requires key + pass again. Back/forward cache must
// not restore credentials or a one-time key from the previous page lifetime.
window.addEventListener('pagehide', clearConnection);
window.addEventListener('pageshow', event => { if (event.persisted) clearConnection(); });
void loadProviders();
