// Shared HTTP helper for plugin endpoints.
//
// Routing:
//   Dashboard plugin fetch → /api/v1/plugins/*
//   wrapper.js → http://agentteams-plugin-manager:8095/api/v1/plugins/*
//   plugin-manager → independent Go microservice (no controller/opskeeper dep)
//
// Auth: wrapper.js injects Authorization: Bearer <SA token>; plugin-manager
// compares SHA-256 against its configured SA token.

const BASE =
  window.location.port === '13000'
    ? `${window.location.protocol}//${window.location.hostname}/api/v1/plugins`
    : '/api/v1/plugins';

async function jsonFetch(path, init) {
  const res = await fetch(BASE + path, {
    credentials: 'same-origin',
    ...init,
    headers: {
      ...(init && init.body ? { 'Content-Type': 'application/json' } : {}),
      ...(init && init.headers ? init.headers : {}),
    },
  });
  const ct = res.headers.get('content-type') || '';
  const data = ct.includes('application/json') ? await res.json() : await res.text();
  if (!res.ok) {
    const msg = (data && data.error) || (typeof data === 'string' ? data : `HTTP ${res.status}`);
    const err = new Error(msg);
    err.status = res.status;
    err.body = data;
    throw err;
  }
  return data;
}

export const pluginApi = {
  list() {
    return jsonFetch('');
  },
  get(id) {
    return jsonFetch('/' + encodeURIComponent(id));
  },
  install(file, opts = {}) {
    const fd = new FormData();
    fd.append('file', file);
    return fetch(BASE + '/install', {
      method: 'POST',
      credentials: 'same-origin',
      body: fd,
    }).then(async (res) => {
      const data = await res.json().catch(() => null);
      if (!res.ok) {
        throw new Error((data && data.error) || `HTTP ${res.status}`);
      }
      return data;
    });
  },
  uninstall(id, { notifyWorker = false } = {}) {
    const qs = notifyWorker ? '?notify_worker=true' : '';
    return jsonFetch('/' + encodeURIComponent(id) + qs, { method: 'DELETE' });
  },
  enable(id) {
    return jsonFetch('/' + encodeURIComponent(id) + '/enable', { method: 'POST' });
  },
  disable(id) {
    return jsonFetch('/' + encodeURIComponent(id) + '/disable', { method: 'POST' });
  },
  sync(id) {
    return jsonFetch('/' + encodeURIComponent(id) + '/sync', { method: 'POST' });
  },
  // POST /v1/plugins/{id}/sync?force=true — worker 重装（force=true 时覆盖已 loaded）
  push(id, { force = true } = {}) {
    const qs = force ? '?force=true' : '';
    return jsonFetch('/' + encodeURIComponent(id) + '/sync' + qs, { method: 'POST' });
  },
  // GET /v1/plugins/{id}/health — plugin-manager DB vs worker 实际状态 diff
  health(id) {
    return jsonFetch('/' + encodeURIComponent(id) + '/health');
  },
  // POST /v1/plugins/{id}/register — 把已安装在 Dashboard 的插件（不在 plugin-manager DB）
  // 导入到 DB，之后就能用 Push / Sync / Enable / Disable 管理。
  register(id) {
    return jsonFetch('/' + encodeURIComponent(id) + '/register', { method: 'POST' });
  },
  // GET /v1/operations?tail=N — 操作日志（环形缓冲，500 条上限），最新在前
  operations({ tail = 200 } = {}) {
    return jsonFetch('/operations?tail=' + encodeURIComponent(tail));
  },
};
