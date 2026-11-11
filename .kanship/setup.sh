#!/bin/sh
# Toolchain for building and testing recarto: Go (go.mod pins go 1.26.4), a C
# compiler (internal/parse links tree-sitter through CGO), make (Makefile builds),
# gitleaks (secret-scan check) and golangci-lint (code-quality check). Kanship
# keeps system changes but discards $HOME and /tmp, so tools live in /usr/local
# and are linked into /usr/local/bin, which is on every bare PATH (checks, bug
# proofs, sessions).
set -eu

GO_VERSION=1.26.4
GO_SHA256=1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f
GITLEAKS_VERSION=8.30.1
GITLEAKS_SHA256=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb
GOLANGCI_VERSION=2.15.0
GOLANGCI_SHA256=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq gcc libc6-dev make curl ca-certificates >/dev/null

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

if [ "$(/usr/local/bin/gitleaks version 2>/dev/null || true)" != "$GITLEAKS_VERSION" ]; then
	tgz=$(mktemp)
	curl -fsSL -o "$tgz" "https://github.com/gitleaks/gitleaks/releases/download/v$GITLEAKS_VERSION/gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz"
	echo "$GITLEAKS_SHA256  $tgz" | sha256sum -c -
	tar -C /usr/local/bin -xzf "$tgz" gitleaks
	rm -f "$tgz"
fi

if [ "$(/usr/local/bin/golangci-lint --version 2>/dev/null | awk '{print $4}')" != "$GOLANGCI_VERSION" ]; then
	tgz=$(mktemp)
	dir=$(mktemp -d)
	curl -fsSL -o "$tgz" "https://github.com/golangci/golangci-lint/releases/download/v$GOLANGCI_VERSION/golangci-lint-$GOLANGCI_VERSION-linux-amd64.tar.gz"
	echo "$GOLANGCI_SHA256  $tgz" | sha256sum -c -
	tar -C "$dir" -xzf "$tgz"
	install -m755 "$dir/golangci-lint-$GOLANGCI_VERSION-linux-amd64/golangci-lint" /usr/local/bin/golangci-lint
	rm -rf "$dir" "$tgz"
fi

