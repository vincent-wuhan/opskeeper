// Shared HTTP helper for opskeeper-teamharness Dashboard extension points.
//
// Proxies through Next.js rewrites (/api/opskeeper/* → opskeeper Manager).
// Each method returns parsed JSON; structured errors include status + body.
//
// All calls use credentials: 'same-origin' so the Higress GatewayKey cookie
// from the Dashboard host is forwarded.

const BASE = '/api/opskeeper';

export function resolvePluginManagerBase(location = globalThis.location) {
  if (!location || location.port !== '13000') return '/api/v1/plugins';
  return `${location.protocol}//${location.hostname}/api/v1/plugins`;
}

const PLUGIN_MANAGER_BASE = resolvePluginManagerBase();
const investigationRequests = new Map();

export function buildInvestigationRequest(incident = {}) {
  const incidentId = String(incident.id ?? incident.incident_id ?? '').trim();
  const labels = incident.labels && typeof incident.labels === 'object' ? incident.labels : {};
  const existingGroup = Array.isArray(incident.alert_group) ? incident.alert_group.filter(Boolean) : [];
  const alertGroup = existingGroup.length
    ? existingGroup
    : [incident.rule_key || incident.alertname || labels.alertname || incidentId].filter(Boolean);
  const existingHints = incident.correlation_hints && typeof incident.correlation_hints === 'object'
    ? incident.correlation_hints
    : {};
  const correlationHints = Object.keys(existingHints).length
    ? existingHints
    : {
      source_id: labels.source_id || incident.source_id || 'dashboard',
      device_id: labels.device_id || incident.target_id || incidentId,
      resource_type: labels.resource_type || incident.target_type || 'unknown',
    };
  return { incident_id: incidentId, alert_group: alertGroup, correlation_hints: correlationHints };
}

async function pluginManagerFetch(path, init = {}) {
  const res = await fetch(PLUGIN_MANAGER_BASE + path, {
    credentials: 'same-origin',
    ...init,
    headers: {
      ...(init.body && !(init.body instanceof FormData) ? { 'Content-Type': 'application/json' } : {}),
      ...(init.headers || {}),
    },
  });
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const err = new Error(data?.error || `Plugin Manager HTTP ${res.status}`);
    err.status = res.status;
    err.body = data;
    throw err;
  }
  return data;
}

export const xhrTransport = {
  createRequest() {
    return new XMLHttpRequest();
  },
};

async function jsonFetch(path, init = {}) {
  const headers = {
    ...(init.body && !(init.body instanceof FormData) ? { 'Content-Type': 'application/json' } : {}),
    ...(init.headers || {}),
  };
  return new Promise((resolve, reject) => {
    const request = xhrTransport.createRequest();
    request.open(init.method || 'GET', BASE + path, true);
    request.withCredentials = true;
    for (const [name, value] of Object.entries(headers)) {
      request.setRequestHeader(name, value);
    }
    request.onload = () => {
      const contentType = request.getResponseHeader('content-type') || '';
      let data;
      try {
        data = contentType.includes('application/json') ? JSON.parse(request.responseText) : request.responseText;
      } catch {
        data = null;
      }
      if (request.status >= 200 && request.status < 300) {
        resolve(data);
        return;
      }
      const message = (data && (data.detail || data.error || data.message))
        || (typeof data === 'string' && data ? data : `HTTP ${request.status}`);
      const error = new Error(message);
      error.status = request.status;
      error.body = data;
      reject(error);
    };
    request.onerror = () => reject(new Error('network error'));
    request.onabort = () => reject(new Error('request aborted'));
    request.send(init.body);
  });
}

export const opskeeperApi = {
  // ── 7-stage RCA ────────────────────────────────────────────────────────
  // POST /v1/mcp/investigate → orchestrator.Run → RootCauseJSON
  investigate({ incident_id, alert_group = [], correlation_hints = {} } = {}) {
    const normalizedIncidentId = String(incident_id ?? '').trim();
    if (!normalizedIncidentId) {
      return Promise.reject(new Error('incident_id is required'));
    }
    if (investigationRequests.has(normalizedIncidentId)) {
      return investigationRequests.get(normalizedIncidentId);
    }

    const request = jsonFetch('/investigate', {
      method: 'POST',
      body: JSON.stringify({
        incident_id: normalizedIncidentId,
        alert_group,
        correlation_hints,
      }),
    }).finally(() => investigationRequests.delete(normalizedIncidentId));
    investigationRequests.set(normalizedIncidentId, request);
    return request;
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

  getSystemHealth() {
    return jsonFetch('/system/health');
  },

  getVersion() {
    return jsonFetch('/version');
  },

  getIncidentMetrics() {
    return jsonFetch('/incidents/metrics');
  },

  // ── Plugin registry (Manager) ─────────────────────────────────────────
  // GET /api/v1/plugins — list installed opskeeper plugins
  listPlugins() {
    return pluginManagerFetch('');
  },

  // POST /api/v1/plugins/install — upload a plugin package; Manager stores it,
  // dispatches to the worker (via /api/opskeeper-teamharness/install-plugin),
  // and re-syncs on success.
  installPlugin(file, { onProgress } = {}) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', PLUGIN_MANAGER_BASE + '/install', true);
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
      fd.append('file', file);
      xhr.send(fd);
    });
  },

  // DELETE /api/v1/plugins/{id} — uninstall a plugin
  uninstallPlugin(pluginId) {
    return pluginManagerFetch('/' + encodeURIComponent(pluginId), { method: 'DELETE' });
  },
};
