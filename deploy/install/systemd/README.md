# opskeeper pure-systemd mode

Alternative to docker-compose for operators who can't (or won't) run
docker. Manager + frontier + dep stack all run as native systemd units.

## When to pick this mode

- Air-gapped / regulated env that disallows the docker daemon.
- Existing host already runs MariaDB / Prometheus / Grafana under
  systemd; we don't want to dual-stack.
- Want tighter resource/security control via systemd's primitives
  (cgroups, capabilities, namespaces) directly.

For everything else, prefer the default `--mode=compose` (top-level
`install.sh`).

## Layout

```
/usr/local/bin/opskeeper             # manager binary
/usr/local/bin/opskeeper-frontier    # tunnel multiplexer binary
/etc/systemd/system/
  opskeeper.service                  # manager unit (this dir → installed here)
  opskeeper-frontier.service
  prometheus.service              # local Prom (we own the config)
  loki.service                    # local Loki
  tempo.service                   # local Tempo
  qdrant.service                  # local qdrant
/etc/opskeeper/
  opskeeper.env                      # manager env (DSNs, LLM key, listen addrs)
  frontier.yaml
  prometheus/prometheus.yml
  prometheus/rules.yml            # ADR-026 self-obs alerts
  loki-config.yaml
  tempo-config.yaml
/var/lib/opskeeper/                  # manager state
/var/lib/opskeeper-prometheus/       # TSDB
/var/lib/opskeeper-loki/             # log store
/var/lib/opskeeper-tempo/            # trace store
/var/lib/opskeeper-qdrant/           # vector store
/var/log/opskeeper/                  # journald is primary; this is for app logs
```

OS-package deps (you install via apt/dnf):

- `mariadb-server` (or compatible MySQL)
- `nginx`
- `grafana`

## Install

```bash
sudo bash install.sh --mode=systemd
```

This script:

1. Creates the `opskeeper` system user + per-dep users
   (`opskeeper-prometheus`, `opskeeper-loki`, `opskeeper-tempo`, `opskeeper-qdrant`).
2. Lays down `/etc/opskeeper/` configs (preserves existing on re-run).
3. Installs the manager + frontier binaries to `/usr/local/bin/`.
4. Writes the 6 systemd unit files.
5. Runs `systemctl daemon-reload` + `enable` (not `start` — operator
   reviews `/etc/opskeeper/opskeeper.env` first).
6. Prints the bring-up sequence.

Phase 2 (next iteration) automates the OS-package dep install and the
Prom/Loki/Tempo/qdrant binary download. Phase 1 expects those binaries
to be present at `/usr/local/bin/` already; the installer warns
loudly when they aren't.

## Uninstall

```bash
sudo bash uninstall.sh                  # stop + remove units; preserve data
sudo bash uninstall.sh --purge          # also nuke data dirs + service users
sudo bash uninstall.sh --purge --yes    # skip the confirmation
```

The top-level `uninstall.sh` auto-detects install mode by looking at
`/etc/systemd/system/opskeeper.service` vs `/opt/opskeeper/docker-compose.yml`
and dispatches into here when the systemd path exists.
