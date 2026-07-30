FROM golang:1.26 AS builder

ARG GIT_COMMIT=unknown
ARG BUILD_VERSION=dev

WORKDIR /src

# Copy root module (needed by replace directive in extras/scion-discord/go.mod)
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY proto/ proto/
COPY cmd/ cmd/
COPY internal/ internal/
COPY resources/ resources/
COPY harnesses/ harnesses/
COPY web/*.go web/

# Copy discord module
COPY extras/scion-discord/ extras/scion-discord/

# Build from the discord module directory
WORKDIR /src/extras/scion-discord
RUN CGO_ENABLED=0 go build -tags no_embed_web \
    -ldflags "-X 'github.com/GoogleCloudPlatform/scion/pkg/version.Commit=${GIT_COMMIT}' -X 'github.com/GoogleCloudPlatform/scion/pkg/version.Version=${BUILD_VERSION}'" \
    -o /discord-bot ./cmd/scion-plugin-discord

FROM gcr.io/distroless/static-debian12
COPY --from=builder /discord-bot /discord-bot
ENTRYPOINT ["/discord-bot", "--standalone"]
