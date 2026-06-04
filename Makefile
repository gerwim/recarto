BINARY_NAME := recarto
PKG         := ./cmd/recarto
MODULE      := git.gerwim.nl/gerwim/recarto
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -ldflags "-s -w -X $(MODULE)/internal/version.Version=$(VERSION)"

# Recarto links tree-sitter via CGO, so cross-compilation needs a C cross-compiler.
# We use `zig cc` (a build-time dependency only — never shipped). Targets match CI:
# linux/amd64 (static musl), darwin/arm64, windows/amd64.

.PHONY: all linux darwin windows native test vet fmt clean

all: linux darwin windows

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-musl" \
		go build $(LDFLAGS) -o releases/linux-amd64/$(BINARY_NAME) $(PKG)

darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC="zig cc -target aarch64-macos" \
		go build $(LDFLAGS) -o releases/darwin-arm64/$(BINARY_NAME) $(PKG)

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-windows-gnu" \
		go build $(LDFLAGS) -o releases/windows-amd64/$(BINARY_NAME).exe $(PKG)

# Build for the host platform using the system C compiler (no zig required).
native:
	CGO_ENABLED=1 go build $(LDFLAGS) -o releases/$(BINARY_NAME) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -rf releases
