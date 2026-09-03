# dist/ — opskeeper release pipeline

This directory owns the **release/packaging** pipeline for opskeeper. One command
produces one artefact, ready to scp to any Linux box with docker + docker
compose installed.

## What `make package` produces

A single tarball:

```
dist/out/opskeeper-v<VERSION>-linux-amd64.tar.xz
dist/out/opskeeper-v<VERSION>-linux-amd64.tar.xz.sha256
dist/out/opskeeper-v<VERSION>-linux-arm64.tar.xz
dist/out/opskeeper-v<VERSION>-linux-arm64.tar.xz.sha256
```

Unpacked layout:

```
opskeeper-v<VERSION>-linux-<arch>/
  VERSION
  README.md              (from deploy/install/README.md)
  install.sh             (from deploy/install/install.sh)
  uninstall.sh
  upgrade.sh
  docker-compose.yml     (prod compose, from deploy/install/)
  .env.example
  prometheus/
    prometheus.yml       (from deploy/prometheus/)
  images/
    opskeeper.tar           (docker save opskeeper:<VERSION>)
  edge/
    opskeeper-edge-linux-amd64
    opskeeper-edge-linux-arm64
    opskeeper-edge-darwin-amd64
    opskeeper-edge-darwin-arm64
    install-edge.sh
    opskeeper-edge.yaml.example
    opskeeper-edge.service
```

The cloud service ships as a docker image tarball (`images/opskeeper.tar`);
`install.sh` runs `docker load -i images/opskeeper.tar` and then
`docker compose up -d`. Edge agents ship as static binaries for four OS/arch
combos so users can run them directly on heterogeneous hosts.

## Release flow

1. Bump the version: edit `VERSION` at the repo root (e.g. `v0.1.1`), commit
   the change, then tag that commit with the same value:
   `git tag v0.1.1 && git push origin v0.1.1`.
2. The `Release` GitHub Actions workflow runs on `v*.*.*` tag pushes and
   executes `make package-all` to build both server packages. Use
   `make package TARGET_ARCH=arm64` locally only when you need a single ARM64
   package. The release build will:
   - `build-edge-all`    — cross-compile opskeeper-edge for 4 targets
   - `docker-build`      — build `opskeeper:<VERSION>` image for `linux/<arch>`
   - stage everything under `dist/stage/opskeeper-<VERSION>-linux-<arch>/`
   - emit the amd64/arm64 tarballs + sha256 files under `dist/out/`
3. Ship the matching package, for example:
   `scp dist/out/opskeeper-v<VERSION>-linux-<arch>.tar.xz user@host:~/`.
4. On the target: untar, `sudo ./install.sh`.

## Checksum

`dist/out/opskeeper-v<VERSION>-linux-<arch>.tar.xz.sha256` sits next to the
tarball. The install script can verify integrity with `sha256sum -c` on
Linux or `shasum -a 256 -c` on macOS.

## Local dry-run

Test the tarball without shipping:

```
make package
mkdir -p /tmp/opskeeper-test && tar -xf dist/out/opskeeper-v*.tar.xz -C /tmp/opskeeper-test
cd /tmp/opskeeper-test/opskeeper-v*
ls -R
# Inside a disposable Ubuntu container with docker socket mounted:
#   docker run --rm -it -v $PWD:/pkg -v /var/run/docker.sock:/var/run/docker.sock \
#     ubuntu:22.04 bash -c 'cd /pkg && ./install.sh'
```

## Files in this directory

- `package.sh` — assembly script invoked by `make package`. Tolerates missing
  `deploy/install/*` files (warns, continues) so the pipeline is testable
  before the on-target scripts land.
- `README.md` — this file.

## What this directory does NOT own

- `deploy/install/**` — on-target install/uninstall/upgrade scripts and prod
  `docker-compose.yml`. Owned by the install-agent.
- `deploy/Dockerfile.*`, `deploy/docker-compose.yml` — build contexts and dev
  compose file.
- Images are **never** pushed to a registry from this pipeline. The tarball
  is the distribution channel.
