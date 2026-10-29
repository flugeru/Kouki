const token = document.querySelector('meta[name="kouki-token"]').content;
const statusNode = document.getElementById('status');
const statusText = document.getElementById('status-text');
const dialog = document.getElementById('simul-dialog');
const form = document.getElementById('simul-form');
const hostField = document.getElementById('host-field');
const hostInput = document.getElementById('simul-host');
const codeInput = document.getElementById('simul-code');
const toastRegion = document.getElementById('toast-region');
let simulAction = 'simul-host';
let lastMessage = 0;
let lastState = null;

async function api(path, data) {
  const method = data === undefined ? 'GET' : 'POST';
  const response = await fetch(path, {
    method,
    cache: 'no-store',
    headers: {'X-Kouki-Token': token, ...(data === undefined ? {} : {'Content-Type': 'application/json'})},
    body: data === undefined ? undefined : JSON.stringify(data)
  });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(result.error || 'Não foi possível completar esta ação.');
  return result;
}

function showToast(title, text, kind = 'info') {
  const node = document.createElement('div');
  node.className = `toast ${kind}`;
  const heading = document.createElement('strong');
  heading.textContent = title;
  const body = document.createElement('div');
  body.textContent = text;
  node.append(heading, body);
  toastRegion.append(node);
  setTimeout(() => node.remove(), 9000);
}

function setText(id, value) {
  document.getElementById(id).textContent = value;
}

function render(state) {
  lastState = state;
  setText('version', `v${state.version}`);
  statusNode.className = `status ${state.statusKind}`;
  statusText.textContent = state.status;
  setText('playing-title', state.title);
  setText('position', state.position);
  setText('mpv-path', state.mpvPath);
  document.getElementById('mpv-path').title = state.mpvPath;
  document.getElementById('progress-fill').style.width = `${Math.max(0, Math.min(100, state.progress || 0))}%`;

  for (const [service, username] of [['anilist', state.aniListUser], ['mal', state.malUser]]) {
    const account = document.getElementById(`${service}-account`);
    account.textContent = username || 'Nenhuma conta conectada';
    account.parentElement.classList.toggle('empty', !username);
  }
  setText('ani-login', state.aniListUser ? 'Reconectar' : 'Entrar');
  setText('mal-login', state.malUser ? 'Reconectar' : 'Entrar');
  for (const node of document.querySelectorAll('[data-correct]')) node.disabled = !state.canCorrect;
  for (const node of document.querySelectorAll('[data-toggle]')) node.checked = Boolean(state.toggles[node.dataset.toggle]);
  setText('simul-status', state.simulStatus || 'Desligado');
  setText('simul-participants', state.participants ? `${state.participants} participante${state.participants === 1 ? '' : 's'}` : '');

  for (const message of state.messages || []) {
    if (message.id <= lastMessage) continue;
    lastMessage = message.id;
    showToast(message.title, message.text, message.kind);
  }
}

async function refresh() {
  try {
    render(await api('/api/state'));
  } catch (error) {
    statusNode.className = 'status muted';
    statusText.textContent = 'Sem conexão';
  }
}

async function sendAction(action, extra = {}) {
  try {
    await api('/api/action', {action, ...extra});
    if (action === 'quit') {
      showToast('Kouki', 'O aplicativo está encerrando.');
      return;
    }
    await refresh();
  } catch (error) {
    showToast('Kouki', error.message, 'error');
  }
}

document.addEventListener('click', event => {
  const button = event.target.closest('button[data-action]');
  if (button) sendAction(button.dataset.action);
});

document.addEventListener('change', async event => {
  const node = event.target.closest('[data-toggle]');
  if (!node) return;
  const prior = !node.checked;
  try {
    await api('/api/toggle', {key: node.dataset.toggle, value: node.checked});
  } catch (error) {
    node.checked = prior;
    showToast('Kouki', error.message, 'error');
  }
  refresh();
});

function newCode() {
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
  const values = new Uint8Array(6);
  crypto.getRandomValues(values);
  return Array.from(values, value => alphabet[value % alphabet.length]).join('');
}

function openSimul(mode) {
  simulAction = mode === 'host' ? 'simul-host' : 'simul-join';
  setText('simul-dialog-title', mode === 'host' ? 'Hospedar sessão' : 'Entrar em sessão');
  setText('simul-dialog-subtitle', mode === 'host' ? 'Compartilhe o convite exibido após criar a sala.' : 'Digite o endereço e o código enviados pelo anfitrião.');
  hostField.hidden = mode === 'host';
  hostInput.required = mode !== 'host';
  hostInput.value = '';
  codeInput.value = mode === 'host' ? newCode() : '';
  dialog.showModal();
  (mode === 'host' ? codeInput : hostInput).focus();
}

document.getElementById('host-button').addEventListener('click', () => openSimul('host'));
document.getElementById('join-button').addEventListener('click', () => openSimul('join'));
document.getElementById('close-dialog').addEventListener('click', () => dialog.close());
document.getElementById('cancel-dialog').addEventListener('click', () => dialog.close());
form.addEventListener('submit', event => {
  event.preventDefault();
  if (!form.reportValidity()) return;
  const payload = {code: codeInput.value.trim(), host: hostInput.value.trim()};
  dialog.close();
  sendAction(simulAction, payload);
});

refresh();
setInterval(refresh, 1500);
