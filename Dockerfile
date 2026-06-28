# Stage 1: Build the web frontend assets
FROM node:22-alpine AS frontend
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ .
RUN npm run copy:shoelace-icons && npm run build

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

# Build the application (without no_embed_web tag so assets are embedded)
RUN go build -o /scion ./cmd/scion/

# Stage 3: Create a minimal runtime image
FROM debian:bookworm-slim
WORKDIR /app

# Install runtime dependencies used by the Hub broker and Cloud Run IAP exec path.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client && rm -rf /var/lib/apt/lists/*

# Copy the binary from the builder stage
COPY --from=builder /scion /usr/local/bin/scion

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/scion"]
