# Release builder for recarto: cross-compiles linux/amd64 + windows/amd64 with zig
# and carries the publish tools.
#
# Darwin/arm64 (osxcross + macOS SDK) is disabled for now; re-add the osxcross
# layers here when bringing the darwin build back. See ci/README.md.
FROM golang:1.26-bookworm

# xz/curl for zig + publish tools (minisign, awscli).
RUN apt-get update && apt-get install -y --no-install-recommends \
      make xz-utils curl ca-certificates git minisign awscli \
    && rm -rf /var/lib/apt/lists/*

# zig for linux/amd64 (musl) and windows/amd64.
ARG ZIG_VERSION=0.13.0
RUN curl -sSL https://ziglang.org/download/${ZIG_VERSION}/zig-linux-x86_64-${ZIG_VERSION}.tar.xz \
      | tar -xJ -C /opt \
    && ln -s /opt/zig-linux-x86_64-${ZIG_VERSION}/zig /usr/local/bin/zig
