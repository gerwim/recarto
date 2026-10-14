#!/bin/sh
# Toolchain for building and testing recarto: Go (go.mod pins go 1.26.4) and a C
# compiler (internal/parse links tree-sitter through CGO). Kanship keeps system
# changes but discards $HOME and /tmp, so Go lives in /usr/local/go and is linked
# into /usr/local/bin, which is on every bare PATH (checks, bug proofs, sessions).
set -eu

GO_VERSION=1.26.4
GO_SHA256=1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq gcc libc6-dev curl ca-certificates >/dev/null

if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "go$GO_VERSION" ]; then
	tgz=$(mktemp)
	curl -fsSL -o "$tgz" "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
	echo "$GO_SHA256  $tgz" | sha256sum -c -
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tgz"
	rm -f "$tgz"
fi
ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/
if [ -n "${KANSHIP_PATH:-}" ]; then
	echo /usr/local/go/bin >>"$KANSHIP_PATH"
fi
