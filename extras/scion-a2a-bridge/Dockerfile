# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build stage
FROM golang:1.25 AS builder

WORKDIR /src

# Copy the main module (needed for replace directive)
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY cmd/ cmd/
COPY web/ web/
COPY internal/ internal/

# Copy the extras module
COPY extras/scion-a2a-bridge/ extras/scion-a2a-bridge/

WORKDIR /src/extras/scion-a2a-bridge

RUN go mod download
RUN CGO_ENABLED=1 go build -o /scion-a2a-bridge ./cmd/scion-a2a-bridge/

# Runtime stage
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /scion-a2a-bridge /usr/local/bin/scion-a2a-bridge

# Create data directory for SQLite
RUN mkdir -p /var/lib/scion-a2a-bridge

EXPOSE 8443 9090

ENTRYPOINT ["scion-a2a-bridge"]
CMD ["--config", "/etc/scion-a2a-bridge/config.yaml"]
