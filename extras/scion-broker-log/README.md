# scion-broker-log

A minimal Scion message broker plugin that logs all messages flowing through the broker. Serves as both a **reference implementation** of the broker plugin interface and a **debugging/observability tool** for inspecting message traffic.

## Build

```bash
cd extras/scion-broker-log
go build -o scion-broker-log .
```

## Usage

```bash
# Start with defaults (listen on localhost:9091, subscribe to all topics)
./scion-broker-log

# JSON output for piping to jq
./scion-broker-log --json

# Only watch user-targeted messages
./scion-broker-log --topic "scion.grove.*.user.>"

# Show full message bodies (default truncates to 120 chars)
./scion-broker-log --full-msg

# Only show topic, type, and message content
./scion-broker-log --fields topic,type,msg

# Custom listen address
./scion-broker-log --addr localhost:9999
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `localhost:9091` | RPC listen address |
| `--topic` | `scion.>` | Subscription pattern (NATS-style: `*` = one token, `>` = remainder) |
| `--json` | `false` | Output JSON Lines instead of human-readable format |
| `--full-msg` | `false` | Show full message body (default truncates to 120 chars) |
| `--fields` | *(all)* | Comma-separated fields to include: `topic`, `sender`, `recipient`, `type`, `status`, `msg`, `attachments` |
| `--forward` | *(none)* | Forward messages to another broker plugin at this address (e.g. `localhost:9090`) |

## Hub Configuration

Add to your server settings (`~/.scion/server.yaml` or versioned settings):

```yaml
server:
  message_broker:
    enabled: true
    type: "broker-log"
  plugins:
    broker:
      broker-log:
        self_managed: true
        address: "localhost:9091"
```

Start `scion-broker-log` before the hub. The hub connects to it as a self-managed plugin via go-plugin RPC.

## Output

**Message traffic** goes to stdout, **lifecycle events** (configure, subscribe, health checks) go to stderr. This makes it easy to separate them:

```bash
# Messages only
./scion-broker-log 2>/dev/null

# Lifecycle only
./scion-broker-log >/dev/null
```

### Human-readable (default)

```
10:23:01.123 PUB scion.grove.abc.user.def.messages
  sender=agent:code-reviewer → recipient=user:alice
  type=assistant-reply  [urgent]
  msg="I'll analyze this carefully... Here is my resp..." [2048 bytes]
```

### JSON Lines (`--json`)

```json
{"ts":"2026-05-07T10:23:01.123Z","topic":"scion.grove.abc.user.def.messages","sender":"agent:code-reviewer","recipient":"user:alice","type":"assistant-reply","urgent":true,"msg_len":2048,"msg":"I'll analyze this carefully..."}
```

## How It Works

This is a self-managed [go-plugin](https://github.com/hashicorp/go-plugin) broker plugin. It:

1. Starts a net/rpc server on `--addr`
2. Waits for the hub to connect and call `Configure()`
3. On receiving host callbacks, requests a subscription for the `--topic` pattern
4. On each `Publish()` call from the hub, formats and writes the message to stdout

It implements `MessageBrokerPluginInterface` and `HostCallbacksAware` from `pkg/plugin`. See `main.go` for the complete implementation — it's a single file designed to be read as a reference.

## Forwarding Mode (tee/proxy)

The hub supports one broker plugin at a time. To use `broker-log` alongside another plugin (e.g. `scion-chat-app`), use `--forward` to tee messages to the downstream plugin:

```bash
# Start the chat app on its normal port
./scion-chat-app ...

# Start broker-log as a proxy — logs everything AND forwards to the chat app
./scion-broker-log --forward localhost:9090
```

Then configure the hub to point at `broker-log` instead of the chat app:

```yaml
server:
  message_broker:
    enabled: true
    type: "broker-log"
  plugins:
    broker:
      broker-log:
        self_managed: true
        address: "localhost:9091"
```

In this mode, broker-log connects to the downstream plugin via go-plugin RPC and forwards all `Publish`, `Configure`, `Subscribe`, and `Unsubscribe` calls after logging. The downstream plugin continues to function normally — it just receives traffic through broker-log instead of directly from the hub.
