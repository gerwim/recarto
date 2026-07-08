BINARY_NAME := recarto
PKG         := ./cmd/recarto
MODULE      := git.gerwim.nl/gerwim/recarto
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -ldflags "-s -w -X $(MODULE)/internal/version.Version=$(VERSION)"

# Recarto links tree-sitter via CGO, so cross-compilation needs a C cross-compiler.
# We use `zig cc` (a build-time dependency only — never shipped). Targets:
# linux/amd64 (static musl), windows/amd64, darwin/arm64.
#
# CGO stays enabled for tree-sitter, but the netgo/osusergo tags force Go's
# pure-Go DNS and os/user resolvers, keeping the binaries fully static.
#
# NOTE on `darwin`: cross-compiling to macOS additionally requires the macOS SDK
# frameworks (CoreFoundation, Security) that crypto/x509 links — zig bundles libc
# but not these frameworks. Build the darwin target either on a macOS host
# (`make native`) or with the SDK available (e.g. via osxcross) on `-F`/SDKROOT.
# linux and windows cross-build from any host with zig.
TAGS := -tags netgo,osusergo

.PHONY: all linux windows darwin native test vet fmt clean image compose-up

all: linux windows darwin

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-musl" \
		go build $(TAGS) $(LDFLAGS) -o releases/linux-amd64/$(BINARY_NAME) $(PKG)

# darwin needs the macOS SDK to link (CoreFoundation/Security via crypto/x509).
# Locally this defaults to zig (works only on a macOS host); CI overrides
# DARWIN_CC with the osxcross compiler (oa64-clang) which carries the SDK.
DARWIN_CC ?= zig cc -target aarch64-macos
darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC="$(DARWIN_CC)" \
		go build $(TAGS) $(LDFLAGS) -o releases/darwin-arm64/$(BINARY_NAME) $(PKG)

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-windows-gnu" \
		go build $(TAGS) $(LDFLAGS) -o releases/windows-amd64/$(BINARY_NAME).exe $(PKG)

# Build for the host platform using the system C compiler (no zig required).
native:
	CGO_ENABLED=1 go build $(TAGS) $(LDFLAGS) -o releases/$(BINARY_NAME) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -rf releases

image: ## build the sync-server container image
	docker build --build-arg VERSION="$(VERSION)" -t recarto-sync:$(VERSION) .

compose-up:
	docker compose up -d --build
