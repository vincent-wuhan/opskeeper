// Shared HTTP helper for opskeeper-teamharness Dashboard extension points.
//
// Proxies through Next.js rewrites (/api/opskeeper/* → opskeeper Manager).
// Each method returns parsed JSON; structured errors include status + body.
//
// All calls use credentials: 'same-origin' so the Higress GatewayKey cookie
// from the Dashboard host is forwarded.

const BASE = '/api/opskeeper';

async function jsonFetch(path, init = {}) {
  const headers = {
    ...(init.body && !(init.body instanceof FormData) ? { 'Content-Type': 'application/json' } : {}),
    ...(init.headers || {}),
  };
  const res = await fetch(BASE + path, {
    credentials: 'same-origin',
    ...init,
    headers,
  });
  const ct = res.headers.get('content-type') || '';
  const data = ct.includes('application/json') ? await res.json().catch(() => null) : await res.text();
  if (!res.ok) {
    const msg = (data && (data.error || data.message)) || (typeof data === 'string' ? data : `HTTP ${res.status}`);
    const err = new Error(msg);
    err.status = res.status;
    err.body = data;
    throw err;
  }
  return data;
}

export const opskeeperApi = {
  // ── 7-stage RCA ────────────────────────────────────────────────────────
  // POST /v1/mcp/investigate → orchestrator.Run → RootCauseJSON
  investigate({ incident_id, alert_group = [], correlation_hints = {} } = {}) {
    return jsonFetch('/investigate', {
      method: 'POST',
      body: JSON.stringify({ incident_id, alert_group, correlation_hints }),
    });
  },

  // ── Incidents ──────────────────────────────────────────────────────────
  // GET /v1/mcp/query_incidents → { incidents, total }
  listIncidents({ status, severity, limit = 20 } = {}) {
    const q = new URLSearchParams();
    if (status) q.set('status', status);
    if (severity) q.set('severity', severity);
    q.set('limit', String(limit));
    return jsonFetch('/incidents?' + q.toString());
  },

  // GET /v1/mcp/get_incident_detail → full incident doc
  getIncident(incident_id) {
    return jsonFetch('/incidents/' + encodeURIComponent(incident_id));
  },

  // ── State (MinIO state.json) ───────────────────────────────────────────
  // GET /v1/state/{task_id}
  getState(task_id) {
    return jsonFetch('/state/' + encodeURIComponent(task_id));
  },

  // PUT /v1/state/{task_id}
  putState(task_id, state) {
    return jsonFetch('/state/' + encodeURIComponent(task_id), {
      method: 'PUT',
      body: JSON.stringify(state),
    });
  },

  // ── Knowledge (plugin-native bridge) ───────────────────────────────────
  // POST /v1/knowledge/docs (proxied via plugin stdio MCP → /v1/knowledge/docs)
  writeKnowledge(doc) {
    return jsonFetch('/knowledge/docs', {
      method: 'POST',
      body: JSON.stringify(doc),
    });
  },

  // GET /v1/mcp/query_knowledge (pgvector + BM25 dual-index)
  queryKnowledge({ query, top_k = 5 } = {}) {
    const q = new URLSearchParams({ query, top_k: String(top_k) });
    return jsonFetch('/knowledge/query?' + q.toString());
  },

  // ── opskeeper plugin install (opskeeper-teamharness-only endpoint) ────
  // POST /api/opskeeper-teamharness/install-plugin — Manager calls this when pushing zip to worker
  // (Dashboard does not normally call this; it is invoked by Manager)
  health() {
    return jsonFetch('/health');
  },

  // ── Plugin registry (Manager) ─────────────────────────────────────────
  // GET /api/v1/plugins — list installed opskeeper plugins
  listPlugins() {
    return jsonFetch('/plugins');
  },

  // POST /api/v1/plugins/install — upload a plugin zip; Manager stores it,
  // dispatches to the worker (via /api/opskeeper-teamharness/install-plugin),
  // and re-syncs on success.
  installPlugin(file, { onProgress } = {}) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', BASE + '/plugins/install', true);
      xhr.withCredentials = true;
      xhr.upload.onprogress = (ev) => {
        if (ev.lengthComputable && onProgress) {
          onProgress({ loaded: ev.loaded, total: ev.total });
        }
      };
      xhr.onload = () => {
        const ct = xhr.getResponseHeader('content-type') || '';
        let data;
        try {
          data = ct.includes('application/json') ? JSON.parse(xhr.responseText) : xhr.responseText;
        } catch {
          data = xhr.responseText;
        }
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve(data);
        } else {
          const msg = (data && (data.error || data.message)) ||
            (typeof data === 'string' ? data : `HTTP ${xhr.status}`);
          const err = new Error(msg);
          err.status = xhr.status;
          err.body = data;
          reject(err);
        }
      };
      xhr.onerror = () => reject(new Error('network error'));
      const fd = new FormData();
      fd.append('plugin', file);
      xhr.send(fd);
    });
  },

  // DELETE /api/v1/plugins/{id} — uninstall a plugin
  uninstallPlugin(pluginId) {
    return jsonFetch('/plugins/' + encodeURIComponent(pluginId), { method: 'DELETE' });
  },
};
