#!/bin/bash
# fake qwenpaw — Docker 演示专用,真实生产 worker 应含真实 qwenpaw binary
# 模拟 `qwenpaw plugin install <dir> --force` 命令成功
echo "[fake-qwenpaw] argv=$*"
case "$1 $2" in
    "plugin install"|"plugin install "*)
        echo "[fake-qwenpaw] installed package: $3"
        exit 0
        ;;
    *)
        echo "[fake-qwenpaw] unknown subcommand: $1 $2"
        exit 1
        ;;
esac
