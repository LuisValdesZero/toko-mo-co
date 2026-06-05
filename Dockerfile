# ══════════════════════════════════════════════════════════════════════
# Toko-Mo-Co — multi-stage Docker build.
# The app is 100% pure Go (incl. modernc.org/sqlite, jackc/pgx, pgvector-go),
# so it builds fully static with CGO_ENABLED=0 — no C toolchain at build time,
# no libc dependency at runtime. Final image: binary + dashboard assets only.
# ══════════════════════════════════════════════════════════════════════

# ── Stage 1: Build ────────────────────────────────────────────────────
# go 1.25: go.mod requires it (jackc/pgx v5.10 + pgvector-go v0.4 declare go 1.25).
FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache dependency downloads (re-runs only when go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a static binary. CGO off -> single self-contained
# binary; modernc.org/sqlite is pure Go so SQLite works without cgo, and the
# Postgres path (pgx + pgvector-go) is pure Go too.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tokomoco .

# ── Stage 2: Runtime ──────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates: the proxy makes HTTPS calls to upstream LLM providers (and to
# a TLS Postgres). tzdata: correct local time in logs/sessions. wget: HEALTHCHECK.
RUN apk add --no-cache ca-certificates tzdata wget

# Non-root user for security
RUN adduser -D -h /app proxy
WORKDIR /app

# Binary
COPY --from=builder /out/tokomoco .

# Dashboard assets — served from disk at runtime relative to the workdir
# (dashboard/websocket.go http.ServeFile + main.go http.Dir("./dashboard/static")).
COPY dashboard/index.html      dashboard/index.html
COPY dashboard/settings.html   dashboard/settings.html
COPY dashboard/static/         dashboard/static/

# SQLite data directory — mount a volume here for persistence. (Ignored when
# CONFIG_DATABASE_URL points the proxy at Postgres instead.)
RUN mkdir -p /app/data && chown proxy:proxy /app/data
VOLUME ["/app/data"]

USER proxy

# Default environment. Set CONFIG_DATABASE_URL=postgres://... to use Postgres
# instead of the SQLite file below.
ENV CONFIG_PORT=8080 \
    CONFIG_DB_PATH=/app/data/proxy.db \
    CONFIG_ALLOWED_ORIGINS=http://localhost:8080

EXPOSE 8080

LABEL org.opencontainers.image.title="Toko-Mo-Co" \
      org.opencontainers.image.description="AI agent proxy with cost tracking, caching, and rules engine" \
      org.opencontainers.image.source="https://github.com/AntibodyEngineers/tokomoco"

# Health check — hit the dedicated health endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:${CONFIG_PORT}/health || exit 1

ENTRYPOINT ["./tokomoco"]
