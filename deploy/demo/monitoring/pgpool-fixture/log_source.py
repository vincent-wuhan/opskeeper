#!/usr/bin/env python3
"""Continuously push labeled logical logs to Loki for the public demo."""

import json
import math
import os
import time
import urllib.request

LOKI_URL = os.environ.get("LOKI_URL", "http://loki:3100").rstrip("/")
DEVICE_ID = os.environ.get("DEVICE_ID", "900001")
HOSTNAME = os.environ.get("DEVICE_HOSTNAME", "pool-fixture")
INTERVAL_SECONDS = int(os.environ.get("INTERVAL_SECONDS", "15"))
STREAM = {
    "device_id": DEVICE_ID,
    "host": HOSTNAME,
    "identifier": "pg-pool-fixture",
    "unit": "opskeeper-demo.service",
    "filename": "/var/log/opskeeper/pg-pool-demo.log",
    "service_name": "opskeeper",
    "opskeeper_source": "embedded",
    "fixture": "pg-pool-demo",
}


def push(values):
    payload = {"streams": [{"stream": STREAM, "values": values}]}
    request = urllib.request.Request(
        f"{LOKI_URL}/loki/api/v1/push",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=5):
        pass


def line(timestamp, level, message):
    return (
        f"time={timestamp} level={level} service=opskeeper "
        f"device_id={DEVICE_ID} fixture=pg-pool-demo message=\"{message}\""
    )


def bootstrap_history():
    now = int(time.time())
    values = []
    for offset in range(55 * 60, -1, -30):
        timestamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - offset))
        usage = 30 + 12 * math.sin((55 * 60 - offset) / 420)
        level = "warning" if offset in (900, 1800) else "info"
        if offset == 900:
            message = "PG pool historical rehearsal reached 2/2 capacity; probe returned pool_exhausted"
        elif offset == 1800:
            message = "PG pool historical rehearsal recovered to 0/4; recovery.verify passed"
        else:
            message = f"PG pool logical source heartbeat; estimated utilization={usage:.1f}%"
        values.append([str((now - offset) * 1_000_000_000), line(timestamp, level, message)])
    push(values)


def main():
    bootstrap_history()
    while True:
        now = time.time()
        timestamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now))
        usage = 28 + 8 * math.sin(now / 420)
        level = "warning" if int(now) % 3600 < INTERVAL_SECONDS else "info"
        message = (
            f"PG pool logical source healthy; estimated utilization={usage:.1f}%"
            if level == "info"
            else "PG pool logical source periodic warning; capacity remains below exhaustion threshold"
        )
        try:
            push([[str(int(now) * 1_000_000_000), line(timestamp, level, message)]])
        except Exception as error:
            print(f"log push failed: {error}", flush=True)
        time.sleep(INTERVAL_SECONDS)


if __name__ == "__main__":
    main()
