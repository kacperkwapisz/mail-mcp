# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
FROM golang:1.25-alpine AS build

# ca-certificates for TLS verification at runtime. Timezone data is embedded
# in the binary instead (see the time/tzdata import), since Alpine ships no
# /usr/share/zoneinfo to copy.
RUN apk add --no-cache ca-certificates

# An empty directory to become a writable /tmp on scratch, where
# get_attachment writes files.
RUN mkdir -p /emptytmp && chmod 1777 /emptytmp

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
COPY --from=build /out/mail-mcp /mail-mcp

# Writable location for get_attachment.
COPY --from=build --chown=65534:65534 /emptytmp /tmp

# nobody: nothing here needs root.
USER 65534:65534

ENV CONFIG_PATH=/config.yml \
    PORT=3000 \
    TRANSPORT=http

EXPOSE 3000

ENTRYPOINT ["/mail-mcp"]
