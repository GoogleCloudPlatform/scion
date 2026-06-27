# Scion Hub — Cloud Run container image
# Multi-stage build: web frontend → Go binary → slim runtime

# ---------------------------------------------------------------------------
# Stage 1: Build web frontend
# ---------------------------------------------------------------------------
FROM node:20-slim AS web-builder

WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2: Build Go binary (with embedded web assets)
# ---------------------------------------------------------------------------
FROM golang:1.25 AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web-builder /src/web/dist/client ./web/dist/client

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -buildvcs=false \
      -ldflags "-X github.com/GoogleCloudPlatform/scion/pkg/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /scion ./cmd/scion

# ---------------------------------------------------------------------------
# Stage 3: Runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      git \
      openssh-client \
      curl \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

RUN useradd -m -d /home/scion -s /bin/bash -u 1000 scion \
    && mkdir -p /home/scion/.kube /run/secrets \
    && chown -R scion:scion /home/scion /run/secrets

COPY --from=go-builder /scion /usr/local/bin/scion
COPY scripts/cloudrun/entrypoint.sh /usr/local/bin/entrypoint.sh

ENV HOME=/home/scion
ENV KUBECONFIG=/home/scion/.kube/config

USER scion
WORKDIR /home/scion

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
