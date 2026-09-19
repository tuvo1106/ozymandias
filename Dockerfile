# ozymandias — one image, two entry points (ozyd, agent).
#
#   docker build --build-arg VERSION=$(git describe --tags --always) -t ozymandias .
#
# Stages:
#   web    builds the UI (Vite) straight into the Go embed directory
#   build  compiles both static binaries with the UI embedded
#   final  distroless/static, non-root: no shell, no package manager, no curl.
#          Health is checked by the binaries' own `healthcheck` subcommand.

# --- web ----------------------------------------------------------------------
FROM node:24-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# vite.config.ts writes to ../internal/api/ui/dist
RUN mkdir -p ../internal/api/ui/dist && npm run build

# --- build --------------------------------------------------------------------
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
COPY --from=web /src/internal/api/ui/dist/ internal/api/ui/dist/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/tuvo1106/ozymandias/internal/buildinfo.Version=${VERSION}" \
      -o /out/ ./cmd/...
# Data dir owned by the distroless nonroot user, so a fresh named volume
# mounted here inherits writable ownership (distroless has no shell to chown).
RUN mkdir -p /out/data

# --- final --------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ozyd /out/agent /usr/local/bin/
COPY --from=build --chown=65532:65532 /out/data /data
COPY deploy/ozyd.yaml deploy/agent.yaml /etc/ozy/
COPY deploy/agent.d/ /etc/ozy/agent.d/
ENV OZY_DATA_DIR=/data \
    OZY_AGENT_CONFD_PATH=/etc/ozy/agent.d
USER nonroot:nonroot
EXPOSE 9400 8126 8125/udp
ENTRYPOINT ["/usr/local/bin/ozyd"]
CMD ["-config", "/etc/ozy/ozyd.yaml"]
