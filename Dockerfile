# Stage 1: Build the web frontend assets
FROM node:22-alpine AS frontend
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ .
# npm run build already runs copy:shoelace-icons, vite build, and copy:client
RUN npm run build

# Stage 2: Build the Scion Hub binary (with embedded web assets)
FROM golang:1.26.1-alpine AS builder
WORKDIR /app
ENV GOWORK=off

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Copy built frontend assets into the embed location
COPY --from=frontend /web/dist/client web/dist/client

# Build a static binary (CGO_ENABLED=0) so it runs on the debian runtime image
# without musl/glibc mismatch from the Alpine builder.
RUN CGO_ENABLED=0 go build -o /scion ./cmd/scion/

# Stage 3: Create a minimal runtime image
#
# The "AS runtime" name is used by both stages below; do not rename it without
# updating them. Naming a stage is builder metadata only: no instruction in this
# stage changes, so the image it produces is unchanged.
FROM debian:bookworm-slim AS runtime
WORKDIR /app

# Install runtime dependencies used by the Hub broker and Cloud Run IAP exec path.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client && rm -rf /var/lib/apt/lists/*

# Copy the binary from the builder stage
COPY --from=builder /scion /usr/local/bin/scion

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/scion"]

# Stage 4: Non-root variant for GKE, built with `--target hub-gke`.
#
# The same image as the runtime stage, except that it runs as uid/gid 1000 with
# a writable HOME. The scion-hub Helm chart (deploy/helm/scion-hub) sets
# runAsNonRoot and runAsUser 1000, and Kubernetes refuses to start a
# runAsNonRoot container whose image user is root. ENTRYPOINT is inherited.
#
# Deliberately absent: ENV KUBECONFIG. pkg/k8s/client.go tries kubeconfig
# loading first (client-go default loading rules, which honour $KUBECONFIG) and
# falls back to in-cluster ServiceAccount credentials only when that fails, so a
# baked KUBECONFIG pointing at a loadable file would take precedence over
# in-cluster auth.
#
# Deliberately absent: CMD. No arguments are baked into the image; the chart
# supplies them.
#
# $HOME/.scion is pre-created so it is writable by uid 1000. Note that
# cmd/server_foreground.go calls config.InitGlobal only when that directory
# does not exist, so a server started from this image without a volume there
# takes the "directory exists" branch. The chart mounts an emptyDir at that
# path, so under the chart the directory exists at startup either way.
#
# The chown is not redundant with `useradd -m`: -m sets the owner, but the
# primary gid is whichever one useradd allocates, while USER below names gid
# 1000 explicitly.
FROM runtime AS hub-gke
RUN useradd -u 1000 -m -d /home/scion scion \
 && mkdir -p /home/scion/.scion \
 && chown -R 1000:1000 /home/scion
ENV HOME=/home/scion
USER 1000:1000

# Stage 5: the default build target. Keep this stage last and empty.
#
# `docker build` with no `--target` builds the last stage in the file, and
# consumers such as `gcloud run deploy --source` and `gcloud builds submit` pass
# no `--target`. With this stage last, the default target is the stage-3
# runtime image, as it was before hub-gke existed; hub-gke is reachable only via
# `--target hub-gke`. Removing this stage, or adding a stage after it, changes
# the default image without any build failure. cmd/dockerfile_contract_test.go
# checks this.
FROM runtime
