# ---- build stage ----
FROM golang:1.26-bookworm AS build
# golang:*-bookworm bundles gcc/make/git (buildpack-deps) — enough for CGO tree-sitter.
WORKDIR /src
# Cache modules first for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
# `make native` => CGO_ENABLED=1 go build -tags netgo,osusergo -ldflags "-s -w -X ...Version" -o releases/recarto ./cmd/recarto
RUN make native VERSION="${VERSION}"

# ---- runtime stage ----
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates wget \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data --no-create-home recarto \
 && mkdir -p /data && chown recarto:recarto /data
COPY --from=build /src/releases/recarto /usr/local/bin/recarto
ENV RECARTO_SERVER_DATA=/data
USER recarto
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
# exec form => recarto is PID 1 and receives SIGTERM for graceful shutdown.
ENTRYPOINT ["recarto"]
CMD ["server", "--addr", ":8080", "--data-dir", "/data"]
