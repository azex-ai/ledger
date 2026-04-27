# syntax=docker/dockerfile:1.7
# ---- Builder ----
# NOTE: tag-only ref. Pin via @sha256 digest in a follow-up for full supply-chain
# integrity once the digest is captured from the registry.
FROM golang:1.26.1-alpine AS builder

# git: needed for some module fetches that resolve via VCS
# ca-certificates: TLS roots used during `go mod download`
# tzdata: copied into the runtime stage (distroless/static lacks it)
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache module downloads on dependency-only layer
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
        -ldflags "-s -w" \
        -trimpath \
        -o /out/ledgerd \
        ./cmd/ledgerd

# ---- Runtime ----
# distroless/static: no shell, no package manager, runs as nonroot (uid 65532)
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="ledgerd" \
      org.opencontainers.image.description="azex-ai/ledger — production-grade double-entry ledger engine" \
      org.opencontainers.image.source="https://github.com/azex-ai/ledger" \
      org.opencontainers.image.licenses="MIT"

# CA roots + tzdata for TLS-bound dependencies and time-zone aware code
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=builder /out/ledgerd /ledgerd

USER nonroot:nonroot
EXPOSE 8080

# Healthcheck delegated to docker-compose (distroless has no shell or wget/curl).
ENTRYPOINT ["/ledgerd"]
