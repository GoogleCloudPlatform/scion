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
FROM golang:1.26.1@sha256:cd78d88e00afadbedd272f977d375a6247455f3a4b1178f8ae8bbcb201743a8a AS builder

WORKDIR /src

# Copy the main module (needed for replace directive)
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY cmd/ cmd/
COPY web/ web/
COPY harnesses/ harnesses/
COPY proto/ proto/
COPY resources/ resources/

# Copy the extras module
COPY extras/scion-a2a-bridge/ extras/scion-a2a-bridge/

WORKDIR /src/extras/scion-a2a-bridge

RUN go mod download
RUN CGO_ENABLED=1 go build -tags no_embed_web -o /scion-a2a-bridge ./cmd/scion-a2a-bridge/

# Runtime stage
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -r -g 1000 bridge && useradd -r -u 1000 -g bridge -s /sbin/nologin bridge

COPY --from=builder /scion-a2a-bridge /usr/local/bin/scion-a2a-bridge
RUN chmod 0755 /usr/local/bin/scion-a2a-bridge

RUN mkdir -p /var/lib/scion-a2a-bridge && chown bridge:bridge /var/lib/scion-a2a-bridge && chmod 0700 /var/lib/scion-a2a-bridge

ENV SCION_A2A_STATE_DB=/var/lib/scion-a2a-bridge/state.db

EXPOSE 8080 8443 50051

USER bridge

ENTRYPOINT ["scion-a2a-bridge"]
CMD ["--config", "/etc/scion-a2a-bridge/config.yaml"]
