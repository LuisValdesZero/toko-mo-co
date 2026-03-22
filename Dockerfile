# ══════════════════════════════════════════════════════════════════════
# Toko-Mo-Co — Multi-stage Docker build
# Final image: ~30MB, no Go toolchain, just the binary + dashboard assets
# ══════════════════════════════════════════════════════════════════════

# ── Stage 1: Build ────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /src

# Cache dependency downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /out/tokomoco .

# ── Stage 2: Runtime ──────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget

# Non-root user for security
RUN adduser -D -h /app proxy
WORKDIR /app

# Binary
COPY --from=builder /out/tokomoco .

# Dashboard assets (served from ./dashboard/ relative to workdir)
COPY dashboard/index.html      dashboard/index.html
COPY dashboard/settings.html   dashboard/settings.html
COPY dashboard/static/         dashboard/static/

# SQLite data directory — mount a volume here for persistence
RUN mkdir -p /app/data && chown proxy:proxy /app/data

USER proxy

# Default environment
ENV CONFIG_PORT=8080
ENV CONFIG_DB_PATH=/app/data/proxy.db
ENV CONFIG_ALLOWED_ORIGINS=http://localhost:8080

EXPOSE 8080

LABEL org.opencontainers.image.title="Toko-Mo-Co" \
      org.opencontainers.image.description="AI agent proxy with cost tracking, caching, and rules engine" \
      org.opencontainers.image.source="https://github.com/AntibodyEngineers/tokomoco"

# Health check — hit the dedicated health endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:${CONFIG_PORT}/health || exit 1

ENTRYPOINT ["./tokomoco"]
