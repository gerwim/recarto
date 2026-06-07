# Release builder for recarto: cross-compiles linux/amd64 + windows/amd64 with zig
# and darwin/arm64 with osxcross (macOS SDK), and carries the publish tools.
#
# The macOS SDK is NOT redistributable, so it is not baked in here by URL. Before
# building this image, place a macOS SDK tarball (e.g. MacOSX14.sdk.tar.xz, made
# with osxcross's tools/gen_sdk_package*.sh on a machine with Xcode) into the build
# context at ci/osxcross-sdk/. See the plan's Task 12 Step 4.
FROM golang:1.26-bookworm

# osxcross build deps + publish tools (minisign, awscli) + xz/curl for zig.
RUN apt-get update && apt-get install -y --no-install-recommends \
      clang llvm lld cmake make patch xz-utils libxml2-dev libssl-dev zlib1g-dev \
      curl ca-certificates git python3 minisign awscli \
    && rm -rf /var/lib/apt/lists/*

# zig for linux/amd64 (musl) and windows/amd64.
ARG ZIG_VERSION=0.13.0
RUN curl -sSL https://ziglang.org/download/${ZIG_VERSION}/zig-linux-x86_64-${ZIG_VERSION}.tar.xz \
      | tar -xJ -C /opt \
    && ln -s /opt/zig-linux-x86_64-${ZIG_VERSION}/zig /usr/local/bin/zig

# osxcross for darwin/arm64. Provides oa64-clang carrying the macOS SDK.
RUN git clone --depth 1 https://github.com/tpoechtrager/osxcross /opt/osxcross
COPY ci/osxcross-sdk/ /opt/osxcross/tarballs/
RUN cd /opt/osxcross && UNATTENDED=1 ./build.sh
ENV PATH="/opt/osxcross/target/bin:${PATH}"
