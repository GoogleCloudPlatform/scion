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

# Multi-stage build for scion-plugin-telegram standalone mode.
# Produces a minimal distroless image suitable for Cloud Run or k8s.

FROM golang:1.26-bookworm AS builder

WORKDIR /src

# Copy the root module (required by the replace directive).
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY cmd/ cmd/

# Copy the telegram module.
COPY extras/scion-telegram/ extras/scion-telegram/

WORKDIR /src/extras/scion-telegram

RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /scion-plugin-telegram ./cmd/scion-plugin-telegram

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /scion-plugin-telegram /scion-plugin-telegram

ENV SCION_TELEGRAM_STANDALONE=1

EXPOSE 9094

ENTRYPOINT ["/scion-plugin-telegram", "--standalone"]
