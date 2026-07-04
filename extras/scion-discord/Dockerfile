FROM golang:1.26 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /discord-bot ./extras/scion-discord/cmd/scion-plugin-discord

FROM gcr.io/distroless/static-debian12
COPY --from=builder /discord-bot /discord-bot
ENTRYPOINT ["/discord-bot", "--standalone"]
