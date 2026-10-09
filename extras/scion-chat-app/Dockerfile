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
COPY resources/ resources/
COPY harnesses/ harnesses/
COPY third_party/ third_party/

# Copy the extras module
COPY extras/scion-chat-app/ extras/scion-chat-app/

WORKDIR /src/extras/scion-chat-app

RUN go mod download
RUN CGO_ENABLED=1 go build -o /scion-chat-app ./cmd/scion-chat-app/

# Runtime stage
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /scion-chat-app /usr/local/bin/scion-chat-app

# Create data directory for SQLite
RUN mkdir -p /var/lib/scion-chat-app

EXPOSE 8443 9090

ENTRYPOINT ["scion-chat-app"]
CMD ["--config", "/etc/scion-chat-app/config.yaml"]
