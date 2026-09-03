#!/usr/bin/env python3
"""Prometheus text endpoint for the public PG pool logical monitoring fixture."""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import math
import os
import time

DEVICE_ID = os.environ.get("DEVICE_ID", "900001")
HOSTNAME = os.environ.get("DEVICE_HOSTNAME", "pool-fixture")
PORT = int(os.environ.get("PORT", "8095"))
STARTED_AT = time.time()


def metric(name, labels, value, metric_type, help_text):
    label_text = ",".join(f'{key}="{value}"' for key, value in labels.items())
    return (
        f"# HELP {name} {help_text}\n"
        f"# TYPE {name} {metric_type}\n"
        f"{name}{{{label_text}}} {value}\n"
    )


def render_metrics():
    elapsed = time.time() - STARTED_AT
    phase = elapsed / 60.0
    common = {
        "device_id": DEVICE_ID,
        "host": HOSTNAME,
        "instance": f"{HOSTNAME}:8095",
        "job": "opskeeper-demo-node-logical",
        "fixture": "pg-pool-demo",
    }
    chunks = []

    for cpu in range(2):
        for mode, rate in (("idle", 0.72), ("user", 0.18), ("system", 0.08), ("iowait", 0.02)):
            labels = {**common, "cpu": str(cpu), "mode": mode}
            label_text = ",".join(f'{key}="{value}"' for key, value in labels.items())
            if not chunks:
                chunks.append("# HELP node_cpu_seconds_total Logical demo CPU seconds.\n# TYPE node_cpu_seconds_total counter\n")
            chunks.append(f"node_cpu_seconds_total{{{label_text}}} {STARTED_AT * rate + elapsed * rate}\n")

    mem_total = 4 * 1024**3
    mem_available = int(mem_total * (0.52 + 0.05 * math.sin(phase)))
    chunks.append(metric("node_memory_MemTotal_bytes", common, mem_total, "gauge", "Logical demo memory total."))
    chunks.append(metric("node_memory_MemAvailable_bytes", common, mem_available, "gauge", "Logical demo memory available."))

    fs_total = 20 * 1024**3
    fs_available = int(fs_total * (0.65 + 0.03 * math.sin(phase / 2)))
    fs_labels = {**common, "device": "/dev/vda", "fstype": "ext4", "mountpoint": "/var/lib/postgresql"}
    chunks.append(metric("node_filesystem_size_bytes", fs_labels, fs_total, "gauge", "Logical demo filesystem size."))
    chunks.append(metric("node_filesystem_avail_bytes", fs_labels, fs_available, "gauge", "Logical demo filesystem available bytes."))

    net_rx_base = STARTED_AT * 125_000
    net_tx_base = STARTED_AT * 42_000
    chunks.append("# HELP node_network_receive_bytes_total Logical demo network receive bytes.\n# TYPE node_network_receive_bytes_total counter\n")
    chunks.append("# HELP node_network_transmit_bytes_total Logical demo network transmit bytes.\n# TYPE node_network_transmit_bytes_total counter\n")
    for device, rx_rate, tx_rate in (("eth0", 125_000, 42_000), ("eth1", 18_000, 9_000)):
        labels = {**common, "device": device}
        label_text = ",".join(f'{key}="{value}"' for key, value in labels.items())
        chunks.append(f"node_network_receive_bytes_total{{{label_text}}} {net_rx_base + elapsed * rx_rate}\n")
        chunks.append(f"node_network_transmit_bytes_total{{{label_text}}} {net_tx_base + elapsed * tx_rate}\n")

    chunks.append("# HELP namedprocess_namegroup_cpu_seconds_total Logical demo process CPU seconds.\n# TYPE namedprocess_namegroup_cpu_seconds_total counter\n")
    chunks.append("# HELP namedprocess_namegroup_memory_bytes Logical demo process resident memory.\n# TYPE namedprocess_namegroup_memory_bytes gauge\n")
    for group, cpu_rate, resident in (
        ("postgres", 0.36, 512 * 1024**2),
        ("opskeeper", 0.18, 256 * 1024**2),
        ("agentteams", 0.10, 192 * 1024**2),
        ("nginx", 0.03, 64 * 1024**2),
    ):
        labels = {**common, "groupname": group}
        label_text = ",".join(f'{key}="{value}"' for key, value in labels.items())
        chunks.append(f"namedprocess_namegroup_cpu_seconds_total{{{label_text}}} {STARTED_AT * cpu_rate + elapsed * cpu_rate}\n")
        chunks.append(f'namedprocess_namegroup_memory_bytes{{{label_text},memtype="resident"}} {resident}\n')

    load1 = 0.55 + 0.18 * math.sin(phase)
    chunks.append(metric("node_load1", common, round(load1, 3), "gauge", "Logical demo one-minute load."))
    disk_labels = {**common, "device": "vda"}
    disk_label_text = ",".join(f'{key}="{value}"' for key, value in disk_labels.items())
    chunks.append("# HELP node_disk_read_bytes_total Logical demo disk read bytes.\n# TYPE node_disk_read_bytes_total counter\n")
    chunks.append(f"node_disk_read_bytes_total{{{disk_label_text}}} {STARTED_AT * 280_000 + elapsed * 280_000}\n")
    chunks.append("# HELP node_disk_written_bytes_total Logical demo disk written bytes.\n# TYPE node_disk_written_bytes_total counter\n")
    chunks.append(f"node_disk_written_bytes_total{{{disk_label_text}}} {STARTED_AT * 165_000 + elapsed * 165_000}\n")

    conntrack = int(18_000 + 2_500 * math.sin(phase))
    chunks.append(metric("node_nf_conntrack_entries", common, conntrack, "gauge", "Logical demo connection tracking entries."))
    chunks.append(metric("node_nf_conntrack_entries_limit", common, 65536, "gauge", "Logical demo connection tracking limit."))
    chunks.append(metric("node_netstat_Tcp_CurrEstab", common, int(90 + 25 * math.sin(phase / 3)), "gauge", "Logical demo established TCP connections."))
    return "".join(chunks).encode()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            body = b"ok\n"
            self.send_response(200)
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        body = render_metrics()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        return


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
