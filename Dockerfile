# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
# CGO off produces a static binary that runs on scratch.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/mail-mcp ./cmd/mail-mcp

# ---- runtime --------------------------------------------------------------
FROM scratch

# TLS roots, so IMAP and SMTP certificate verification works.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Timezone data, so message dates render correctly.
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/mail-mcp /mail-mcp

# Writable location for get_attachment.
COPY --from=build --chown=65534:65534 /tmp /tmp

# nobody: nothing here needs root.
USER 65534:65534

ENV CONFIG_PATH=/config.yml \
    PORT=3000 \
    TRANSPORT=http

EXPOSE 3000

ENTRYPOINT ["/mail-mcp"]
