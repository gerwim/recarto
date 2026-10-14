#!/bin/sh
# Go toolchain for building and testing the graph engine (go.mod: go 1.26.4).
# Kanship provisions a VM per task; run this from the repository's .kanship/setup.sh
# hook so bare `go` commands (checks, bug proofs, sessions) find the toolchain.
set -eu

if [ ! -x /root/goroot/go/bin/go ]; then
	mkdir -p /root/goroot
	curl -fsSL https://go.dev/dl/go1.26.4.linux-amd64.tar.gz | tar -C /root/goroot -xz
fi
ln -sf /root/goroot/go/bin/go /usr/local/bin/go
ln -sf /root/goroot/go/bin/gofmt /usr/local/bin/gofmt
if [ -n "${KANSHIP_PATH:-}" ]; then
	echo /root/goroot/go/bin >>"$KANSHIP_PATH"
fi
