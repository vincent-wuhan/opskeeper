#!/usr/bin/env python3
"""P0.2 verification — qwenpaw install-plugin must NOT block the event loop.

Strategy:
1. Find an existing plugin zip in the worker fs.
2. Launch POST /api/opskeeper-teamharness/install-plugin (slow, blocks thread-pool
   but NOT the event loop after the asyncio.to_thread fix).
3. Concurrently, every 200ms hit GET /api/version and record latency.
4. PASS criteria: max /api/version latency during install ≤ 1s.
   Without the fix, every concurrent /api/version would time out 30s.
"""
import asyncio
import json
import os
import sys
import time
from pathlib import Path

import httpx

WORKER_BASE = os.environ.get("WORKER_BASE", "http://127.0.0.1:8088")
SAMPLES = []
INSTALL_RESULT = {}


async def probe_version(idx: int, stop: asyncio.Event) -> None:
    async with httpx.AsyncClient(base_url=WORKER_BASE, timeout=2.0) as c:
        while not stop.is_set():
            t0 = time.perf_counter()
            try:
                r = await c.get("/api/version")
                dt = (time.perf_counter() - t0) * 1000
                SAMPLES.append((idx, dt, r.status_code))
            except Exception as e:
                dt = (time.perf_counter() - t0) * 1000
                SAMPLES.append((idx, dt, f"err:{type(e).__name__}"))
            await asyncio.sleep(0.2)


async def install(zip_path: Path) -> None:
    async with httpx.AsyncClient(base_url=WORKER_BASE, timeout=300.0) as c:
        t0 = time.perf_counter()
        try:
            with zip_path.open("rb") as f:
                r = await c.post(
                    "/api/opskeeper-teamharness/install-plugin",
                    files={"file": (zip_path.name, f, "application/zip")},
                )
            dt = time.perf_counter() - t0
            INSTALL_RESULT["status"] = r.status_code
            INSTALL_RESULT["body"] = r.text[:500]
            INSTALL_RESULT["duration_s"] = dt
        except Exception as e:
            INSTALL_RESULT["error"] = f"{type(e).__name__}: {e}"


async def main() -> int:
    zip_path = Path(sys.argv[1]) if len(sys.argv) > 1 else None
    if zip_path is None or not zip_path.exists():
        # Look for opskeeper-teamharness.zip in known locations
        candidates = [
            Path("/root/agentteams-fs/agents/lumos/.qwenpaw/cache/zips"),
            Path("/root/multica_workspaces/77113af3-bd2e-4c2a-9f11-659117e3ca3d/1b0a46a94849/workdir/opskeeper/scripts/dev"),
        ]
        for d in candidates:
            if d.exists():
                for p in d.glob("*.zip"):
                    zip_path = p
                    break
            if zip_path:
                break
        if not zip_path:
            print(f"usage: {sys.argv[0]} <plugin.zip>")
            return 2

    print(f"plugin zip: {zip_path} size={zip_path.stat().st_size}")

    # Pre-install probe to establish baseline
    async with httpx.AsyncClient(base_url=WORKER_BASE, timeout=2.0) as c:
        for _ in range(3):
            r = await c.get("/api/version")
            print(f"baseline /api/version: status={r.status_code}")

    stop = asyncio.Event()
    probes = [asyncio.create_task(probe_version(i, stop)) for i in range(3)]
    t_install_start = time.perf_counter()
    install_task = asyncio.create_task(install(zip_path))
    while not install_task.done():
        await asyncio.sleep(0.5)
    await install_task
    t_install_end = time.perf_counter()
    stop.set()
    await asyncio.gather(*probes)

    print(f"\ninstall result: {INSTALL_RESULT}")
    print(f"install duration: {INSTALL_RESULT.get('duration_s', '?')}s")

    # Filter samples that were taken DURING the install
    during = [s for s in SAMPLES]
    print(f"\nprobes during install: {len(during)}")
    for s in during[:30]:
        print(f"  probe#{s[0]} latency={s[1]:.0f}ms status={s[2]}")

    if not during:
        print("no samples — install may have been instant")
        return 1
    max_latency_ms = max(s[1] for s in during)
    ok_count = sum(1 for s in during if isinstance(s[2], int) and s[2] == 200)
    print(f"\nmax latency: {max_latency_ms:.0f}ms ok_count={ok_count}/{len(during)}")
    if max_latency_ms > 1000:
        print("FAIL: event loop blocked (max latency > 1s)")
        return 1
    if ok_count < len(during) * 0.5:
        print("FAIL: more than half of probes failed")
        return 1
    print("PASS: event loop stayed responsive during install")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))