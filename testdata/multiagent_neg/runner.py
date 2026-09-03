"""Negative-path multi-agent RBAC validation.

Probes the auth + RBAC chain to find real problems in agent-teams scenarios:
  T1  cross-role privilege escalation: alerter tries recovery.execute (mutating)
  T2  cross-role privilege escalation: critic tries recovery.execute (mutating)
  T3  cross-role privilege escalation: verifier tries recovery.execute (mutating)
  T4  cross-role read: critic tries loop.investigate (read-only; should pass if critic has the tool)
  T5  signature tampering: body modified after signing
  T6  missing signature header
  T7  expired timestamp (negative skew)
  T8  Worker-Identity / Bearer mismatch (use alerter key + investigator identity)
  T9  unknown consumer (key not in Higress stub)
  T10 unknown role (signature has invalid role suffix)
  T11 cross-tenant (tenant_id != default)
"""
import json, time, hmac, hashlib, urllib.request, urllib.error, os, sys

BACKEND_URL = "http://127.0.0.1:8080"
os.makedirs("/tmp/7stage_multineg", exist_ok=True)

ROLES = {
    "alerter":      "opskeeper-alerter",
    "investigator": "opskeeper-investigator",
    "critic":       "opskeeper-critic",
    "reviewer":     "opskeeper-reviewer",
    "repairer":     "opskeeper-repairer",
    "verifier":     "opskeeper-verifier",
    "reporter":     "opskeeper-reporter",
}

def key_for(role):
    return f"{role[:6]}e34a8b3d9c7f2a1b4e6d8c9f0a1b2c3d:opskeeper-{role}:ak-{role}-001:{role}"

def call(role, body_dict, *, traceparent=None, identity_override=None,
         mutate_body_after_sign=False, drop_sig=False, skew_ts=None,
         key_override=None, tenant="default"):
    key = key_override or key_for(role)
    body = json.dumps(body_dict).encode()
    ts = str(int(time.time()) + (skew_ts or 0))
    # New protocol: HMAC over ts + "." + body_sha256_hex; backend checks
    # X-Opskeeper-Signature against X-Opskeeper-Body-SHA256 header.
    body_sha = hashlib.sha256(body).hexdigest()
    msg = ts.encode() + b"." + body_sha.encode()
    sig = hmac.new(key.encode(), msg, hashlib.sha256).hexdigest()
    wid = identity_override or {
        "tenant_id": tenant,
        "service": "agentteams",
        "worker": ROLES[role],
        "role": role,
    }
    h = {
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
        "X-Opskeeper-Version": "v1",
        "X-Opskeeper-Timestamp": ts,
        "X-Opskeeper-Tenant": tenant,
        "X-Opskeeper-Worker-Identity": json.dumps(wid),
        "X-Opskeeper-Body-SHA256": body_sha,
        "traceparent": traceparent or f"00-negnnnnnnnnnnnnnnnnnnnnnn01-{role[:8]:0<8}-01",
    }
    if not drop_sig:
        if mutate_body_after_sign:
            # Tamper signature: send original hash in header but sign over a DIFFERENT
            # hash so backend's recompute mismatches. (Body bytes are ignored at the
            # middleware layer; only the body-sha header counts.)
            wrong_sha = hashlib.sha256(b"tampered").hexdigest()
            sig = hmac.new(key.encode(), ts.encode() + b"." + wrong_sha.encode(), hashlib.sha256).hexdigest()
        h["X-Opskeeper-Signature"] = sig
    try:
        r = urllib.request.urlopen(
            urllib.request.Request(f"{BACKEND_URL}/api/v1/mcp", data=body, headers=h, method="POST"),
            timeout=30,
        )
        return r.status, json.loads(r.read()), dict(h)
    except urllib.error.HTTPError as e:
        try:
            err_body = e.read().decode()
        except Exception:
            err_body = ""
        return e.code, err_body, dict(h)


CASES = []

# ----- T1: alerter calls recovery.execute (mutating) -----
CASES.append(("T1: alerter → recovery.execute (mutating)", lambda: call("alerter", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "recovery.execute", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "skill": "pg", "action": "vacuum_analyze", "target": "pg:primary",
    }},
})))

# ----- T2: critic calls recovery.execute (mutating) -----
CASES.append(("T2: critic → recovery.execute (mutating)", lambda: call("critic", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "recovery.execute", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "skill": "pg", "action": "vacuum_analyze", "target": "pg:primary",
    }},
})))

# ----- T3: verifier calls recovery.execute (mutating) -----
CASES.append(("T3: verifier → recovery.execute (mutating)", lambda: call("verifier", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "recovery.execute", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "skill": "pg", "action": "vacuum_analyze", "target": "pg:primary",
    }},
})))

# ----- T4: critic calls loop.investigate (read-only; critic may or may not have it) -----
CASES.append(("T4: critic → loop.investigate (read)", lambda: call("critic", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.investigate", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "alert_group": ["AL-MULTIAGENT-NEG-A"],
        "correlation_hints": {"fingerprint": "neg-fp"},
    }},
})))

# ----- T5: signature mismatch via body tampering -----
CASES.append(("T5: alerter → loop.correlate (tampered body)", lambda: call("alerter", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.correlate", "arguments": {"raw_alerts": [], "window": "5m"}},
}, mutate_body_after_sign=True)))

# ----- T6: missing signature header -----
CASES.append(("T6: investigator → loop.correlate (no signature)", lambda: call("investigator", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.correlate", "arguments": {"raw_alerts": [], "window": "5m"}},
}, drop_sig=True)))

# ----- T7: timestamp skewed 1h in the past -----
CASES.append(("T7: investigator → loop.correlate (ts -3600)", lambda: call("investigator", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.correlate", "arguments": {"raw_alerts": [], "window": "5m"}},
}, skew_ts=-3600)))

# ----- T8: Bearer alerter key + Worker-Identity investigator (mismatch) -----
CASES.append(("T8: alerter key + investigator identity", lambda: call("alerter", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.investigate", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "alert_group": ["AL-MULTIAGENT-NEG-A"],
        "correlation_hints": {"fingerprint": "neg-fp"},
    }},
}, identity_override={
    "tenant_id": "default", "service": "agentteams",
    "worker": "opskeeper-investigator", "role": "investigator",
})))

# ----- T9: unknown consumer (random key) -----
CASES.append(("T9: unknown apiKey", lambda: call("investigator", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.investigate", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "alert_group": ["AL-MULTIAGENT-NEG-A"],
        "correlation_hints": {"fingerprint": "neg-fp"},
    }},
}, key_override="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:opskeeper-unknown:ak-unknown-001:unknown")))

# ----- T10: signature has invalid role suffix (e.g. 'repairer' as key role) -----
CASES.append(("T10: malformed role suffix", lambda: call("investigator", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.investigate", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "alert_group": ["AL-MULTIAGENT-NEG-A"],
        "correlation_hints": {"fingerprint": "neg-fp"},
    }},
}, key_override="invest3aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:opskeeper-investigator:ak-investigator-001:bogus")))

# ----- T11: cross-tenant (different tenant_id in identity) -----
CASES.append(("T11: investigator → loop.investigate (tenant=tenant-b)", lambda: call("investigator", {
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {"name": "loop.investigate", "arguments": {
        "incident_id": "INC-MULTIAGENT-NEG",
        "alert_group": ["AL-MULTIAGENT-NEG-A"],
        "correlation_hints": {"fingerprint": "neg-fp"},
    }},
}, tenant="tenant-b")))

results = []
for name, fn in CASES:
    print(f"\n--- {name}")
    try:
        code, body, headers = fn()
    except Exception as e:
        code, body = -1, repr(e)
        headers = {}
    if isinstance(body, str):
        snippet = body[:120]
    elif isinstance(body, dict):
        snippet = json.dumps(body)[:200]
    else:
        snippet = str(body)[:120]
    print(f"  HTTP {code} :: {snippet}")
    results.append({"name": name, "http": code, "body": body if isinstance(body, (str, dict)) else str(body)[:300]})

with open("/tmp/7stage_multineg/results.json", "w", encoding="utf-8") as f:
    json.dump(results, f, ensure_ascii=False, indent=2)
print("\nSaved to /tmp/7stage_multineg/results.json")
