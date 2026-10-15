#!/usr/bin/env bash
# 构建并执行一条验收链，原样传出退出码。
#
#   bash scripts/acceptance/run.sh                                  # 阶段 0
#   bash scripts/acceptance/run.sh --chain=federation \
#        --root-env=/etc/opskeeper/root.env \
#        --child-env=/etc/opskeeper/child.env
#
# 为什么要有这个包装：
#   - `go run` 会把子进程的 3 压成 1；
#   - make 会把任何非零压成 2。
# 而 1 与 3 正是这条命令要区分的两件事（检查失败 / 缺输入）。bash 的 exit 会原样传出，所以 CI
# 要拿得到契约，就从这里跑，不要从 `go run` 或 `make` 跑。
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="$(mktemp -t opskeeper-acceptance)"
trap 'rm -f "$BIN"' EXIT
go build -o "$BIN" "$ROOT/scripts/acceptance" || exit 1
if [[ $# -eq 0 ]]; then
	"$BIN" "$ROOT"
else
	"$BIN" "$ROOT" "$@"
fi
exit $?
