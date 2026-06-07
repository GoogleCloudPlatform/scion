# Agent Visualizer

A standalone tool that replays agent activity from Google Cloud Logging exports as a 2D force-directed graph visualization. A Go binary parses log files and serves a web-based visualizer over WebSocket, enabling playback at variable speeds.

## What it shows

- **File graph** -- force-directed graph of the project's file/directory tree (center)
- **Agent ring** -- agents distributed radially around the file graph, with color-coded state icons
- **Messages** -- transient directional pulse lines between agents, fading after ~0.5s
- **Agent Communications panel** -- right-side scrolling transcript of inter-agent messages, kept in sync with playback (and rebuilt on seek); broadcasts are highlighted and de-duplicated. Collapse it with the −/+ button. It reads the same `message` events that drive the on-graph pulses, so no extra data source is needed.
- **File edits** -- particles traveling from agent to file node; new files materialize with an expand effect
- **Playback controls** -- play/pause, speed (1x--100x), time scrubber, agent and event type filters

## Prerequisites

- Go 1.25+
- Node.js 20+ and npm

## Building

```bash
make build
```

This installs npm dependencies, builds the web frontend with Vite, embeds the assets into the Go binary, and produces the `agent-viz` executable.

To build components individually:

```bash
make web    # build web frontend only
make test   # run Go tests
```

## Usage

```bash
./agent-viz --log-file /path/to/gcp-logs.json [--port 8080]
```

The browser opens automatically to `http://localhost:8080`.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--log-file` | (required) | Path to GCP log JSON export file |
| `--port` | `8080` | Port to serve on |
| `--dev` | `false` | Serve web assets from disk instead of embedded (for frontend development) |
| `--no-browser` | `false` | Don't open browser automatically |

### Log file format

The input is a JSON array of log entries exported from Google Cloud Logging. The tool consumes three log streams:

| Log stream | Used for |
|------------|----------|
| `scion-agents` | Agent state changes, tool calls, file edit detection |
| `scion-messages` | Message flow between agents |
| `scion-server` | Context (grove setup, broker registration) |

Export logs from GCP with:

```bash
gcloud logging read 'logName=~"scion-"' --format=json --order=asc > logs.json
```

## Development

For frontend development, run the Go backend and Vite dev server separately:

```bash
# Terminal 1: Go backend
go run ./cmd/agent-viz/ --log-file /path/to/logs.json --port 8080

# Terminal 2: Vite dev server (proxies /ws to Go backend)
cd web && npm run dev
```

The Vite dev server runs on port 3000 with hot reload and proxies WebSocket connections to the Go backend on port 8080.

## Architecture

```
extras/agent-viz/
├── cmd/agent-viz/          CLI entry point
├── internal/
│   ├── logparser/          GCP log JSON -> playback events
│   ├── playback/           Timing, speed, seek, filtering
│   └── server/             HTTP + WebSocket server, embedded assets
├── web/
│   ├── src/
│   │   ├── main.ts         Bootstrap, WebSocket connect, animation loop
│   │   ├── graph.ts        Force-graph file tree layout
│   │   ├── agents.ts       Radial agent ring rendering
│   │   ├── messages.ts     Message pulse line animations
│   │   ├── files.ts        File edit particle effects
│   │   ├── playback.ts     Transport bar and filter panel
│   │   ├── ws.ts           WebSocket client
│   │   ├── types.ts        TypeScript interfaces
│   │   └── icons.ts        Bootstrap Icon SVG paths for agent state
│   ├── index.html
│   └── vite.config.ts
├── go.mod
└── Makefile
```

## Playback controls

- **Play/Pause** -- toggle playback
- **Rewind/Forward** -- jump to start/end of timeline
- **Speed** -- dropdown: 1x, 2x, 5x, 10x, 20x, 50x, 100x
- **Scrubber** -- drag to seek to any point in the log timeline
- **Agent filter** -- checkboxes to show/hide specific agents
- **Event type filter** -- toggle state changes, messages, file edits, lifecycle events

Filters are applied server-side to reduce WebSocket traffic.
