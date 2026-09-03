// /app/wrapper.js — replaces server.js. Spawns Next.js on internal port 3001
// and serves a lightweight proxy on port 3000 that:
//   - /api/v1/plugins/*  → agentteams-plugin-manager:8095/* (plugin SA-token API)
//   - /api/opskeeper/*   → opskeeper-new:28080/api/v1/*       (Manager Bearer-auth API)
//   - everything else    → 127.0.0.1:3001                  (Next.js)
//
// Why this exists:
//   Dashboard plugin talks to opskeeper Manager via BASE=/api/opskeeper.
//   Next.js standalone build does NOT honor `rewrites()` (routes-manifest
//   `rewrites` is empty), so we proxy at the listener boundary instead.

const http = require('http');
const { spawn } = require('child_process');

const PORT = parseInt(process.env.PORT, 10) || 3000;
const NEXT_PORT = parseInt(process.env.NEXT_INTERNAL_PORT, 10) || 3001;
const NEXT_HOST = process.env.NEXT_INTERNAL_HOST || '127.0.0.1';
const OPSKEEPER_BACKEND_URL =
  process.env.OPSKEEPER_BACKEND_URL || 'http://opskeeper-new:28080';
// Default to host's docker bridge gateway (reachable from the Dashboard
// container when plugin-manager runs as a host process). Set
// PLUGIN_MANAGER_BACKEND_URL to override, e.g. when plugin-manager runs
// as a sibling container on the same Docker network.
const PLUGIN_MANAGER_BACKEND_URL =
  process.env.PLUGIN_MANAGER_BACKEND_URL || 'http://172.17.0.1:18095';
const PLUGIN_MANAGER_SA_TOKEN = process.env.PLUGIN_MANAGER_SA_TOKEN || process.env.AGENTTEAMS_AUTH_TOKEN || '';

const OPSKEEPER_PREFIX = '/api/opskeeper/';
const PLUGIN_MANAGER_PREFIX = '/api/v1/plugins';
const TIMEOUT_MS = 5 * 60 * 1000;

const HOP_BY_HOP = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailers',
  'transfer-encoding',
  'upgrade',
]);

function copyHeaders(src, dst, keep = new Set()) {
  for (const [k, v] of Object.entries(src)) {
    const lk = k.toLowerCase();
    if (HOP_BY_HOP.has(lk)) continue;
    if (lk === 'host') continue;
    if (lk === 'content-length') continue;
    dst[k] = v;
  }
  for (const k of keep) {
    if (src[k] != null) dst[k] = src[k];
  }
}

function proxyOpskeeper(req, res) {
  const tail = req.url.slice(OPSKEEPER_PREFIX.length).split('?')[0];
  const search = req.url.includes('?') ? req.url.slice(req.url.indexOf('?')) : '';
  const target = `${OPSKEEPER_BACKEND_URL.replace(/\/$/, '')}/api/v1/${tail}${search}`;

  const headers = {};
  copyHeaders(req.headers, headers, new Set(['authorization', 'cookie', 'content-type']));

  const upstream = http.request(
    target,
    { method: req.method, headers, timeout: TIMEOUT_MS },
    (uRes) => {
      const out = {};
      copyHeaders(uRes.headers, out, new Set(['content-type', 'set-cookie', 'x-request-id']));
      res.writeHead(uRes.statusCode || 502, out);
      uRes.pipe(res);
    },
  );
  upstream.on('error', (err) => {
    process.stderr.write(`[wrapper] opskeeper upstream error ${err.code} target=${target}\n`);
    if (!res.headersSent) {
      res.writeHead(502, { 'content-type': 'application/json' });
    }
    res.end(
      JSON.stringify({
        error: 'opskeeper_proxy_failed',
        target,
        detail: err.code || err.message,
      }),
    );
  });
  upstream.on('timeout', () => upstream.destroy(new Error('upstream timeout')));
  req.pipe(upstream);
}

// proxyPluginManager forwards /api/v1/plugins/* to the standalone plugin
// manager service. Path is preserved as-is (no prefix rewrite). The SA token is
// injected as Authorization because the plugin manager verifies Bearer auth
// using SHA-256 hash comparison against the same SA token secret.
function proxyPluginManager(req, res) {
  const target = `${PLUGIN_MANAGER_BACKEND_URL.replace(/\/$/, '')}${req.url}`;

  const headers = {};
  copyHeaders(req.headers, headers, new Set(['cookie', 'content-type']));
  if (PLUGIN_MANAGER_SA_TOKEN) {
    headers['authorization'] = `Bearer ${PLUGIN_MANAGER_SA_TOKEN}`;
  }

  const upstream = http.request(
    target,
    { method: req.method, headers, timeout: TIMEOUT_MS },
    (uRes) => {
      const out = {};
      copyHeaders(uRes.headers, out, new Set(['content-type', 'set-cookie', 'x-request-id']));
      res.writeHead(uRes.statusCode || 502, out);
      uRes.pipe(res);
    },
  );
  upstream.on('error', (err) => {
    process.stderr.write(`[wrapper] plugin-manager upstream error ${err.code} target=${target}\n`);
    if (!res.headersSent) {
      res.writeHead(502, { 'content-type': 'application/json' });
    }
    res.end(
      JSON.stringify({
        error: 'plugin_manager_proxy_failed',
        target,
        detail: err.code || err.message,
      }),
    );
  });
  upstream.on('timeout', () => upstream.destroy(new Error('upstream timeout')));
  req.pipe(upstream);
}

function proxyNext(req, res) {
  const target = `http://${NEXT_HOST}:${NEXT_PORT}${req.url}`;
  const headers = {};
  copyHeaders(req.headers, headers, new Set(['cookie', 'content-type']));
  const upstream = http.request(
    target,
    { method: req.method, headers, timeout: TIMEOUT_MS },
    (uRes) => {
      const out = {};
      copyHeaders(uRes.headers, out, new Set(['content-type', 'set-cookie']));
      res.writeHead(uRes.statusCode || 502, out);
      uRes.pipe(res);
    },
  );
  upstream.on('error', (err) => {
    if (!res.headersSent) {
      res.writeHead(502, { 'content-type': 'text/plain' });
    }
    res.end(`Dashboard upstream error: ${err.code || err.message}`);
  });
  upstream.on('timeout', () => upstream.destroy(new Error('upstream timeout')));
  req.pipe(upstream);
}

// Spawn Next.js as a child on NEXT_PORT.
const next = spawn(
  process.execPath,
  [require('path').join(__dirname, 'server.real.js')],
  {
    env: { ...process.env, PORT: String(NEXT_PORT), HOSTNAME: NEXT_HOST },
    stdio: ['ignore', 'inherit', 'inherit'],
  },
);

next.on('exit', (code, signal) => {
  console.error(`[wrapper] Next.js child exited code=${code} signal=${signal}`);
  process.exit(code || (signal ? 1 : 0));
});

// Wait for Next.js child to listen on NEXT_PORT before binding our proxy.
async function waitForPort(host, port, timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const ok = await new Promise((resolve) => {
      const req = http.request(
        { host, port, path: '/api/agentteams/healthz', method: 'GET', timeout: 1000 },
        (res) => {
          res.resume();
          resolve(res.statusCode != null);
        },
      );
      req.on('error', () => resolve(false));
      req.end();
    });
    if (ok) return true;
    await new Promise((r) => setTimeout(r, 250));
  }
  return false;
}

const server = http.createServer((req, res) => {
  if (!req.url) {
    proxyNext(req, res);
    return;
  }
  if (req.url === PLUGIN_MANAGER_PREFIX || req.url.startsWith(PLUGIN_MANAGER_PREFIX + '/') || req.url.startsWith(PLUGIN_MANAGER_PREFIX + '?')) {
    proxyPluginManager(req, res);
  } else if (req.url.startsWith(OPSKEEPER_PREFIX)) {
    proxyOpskeeper(req, res);
  } else {
    proxyNext(req, res);
  }
});

(async () => {
  const ok = await waitForPort(NEXT_HOST, NEXT_PORT);
  if (!ok) {
    console.error(`[wrapper] Next.js did not become ready on ${NEXT_HOST}:${NEXT_PORT} within 30s`);
    process.exit(1);
  }
  console.log(`[wrapper] Next.js ready on ${NEXT_HOST}:${NEXT_PORT}; binding proxy on 0.0.0.0:${PORT}`);
  server.listen(PORT, '0.0.0.0', () => {
    console.log(`[wrapper] proxy listening on 0.0.0.0:${PORT}`);
    console.log(`[wrapper]   /api/v1/plugins/*  → ${PLUGIN_MANAGER_BACKEND_URL}`);
    console.log(`[wrapper]   /api/opskeeper/*   → ${OPSKEEPER_BACKEND_URL}`);
    console.log(`[wrapper]   /*                 → Next.js on ${NEXT_HOST}:${NEXT_PORT}`);
  });
})();