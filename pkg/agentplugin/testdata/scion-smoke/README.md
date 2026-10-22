# scion-smoke Agent Plugins fixture

A minimal Agent Plugins 1.0.0 bundle for the live smoke test of native plugin
install (hub.agent_plugins; see the design's §5.3). Text files only, no
dependencies beyond python3.

- `skills/smoke-hello`: asked for the smoke word, the agent answers `PLUM-7`.
- `bin/stdio-mcp`: stdio MCP server with one tool, `whereami`, returning the
  server's cwd, `PLUGIN_ROOT`, `PLUGIN_DATA`, whether `PLUGIN_DATA` is
  writable, and its argv. `mcp.json` omits `cwd` on purpose, to test the
  spec's default (the plugin root).
- `bin/http-mcp`: minimal streamable-http MCP server on 127.0.0.1:8765 with
  one tool, `ping`, returning `pong`. Start it by hand from the native plugin
  root during the smoke test.

To use it, copy this directory to `plugins/scion-smoke/` in a test template
whose `scion-agent.yaml` declares:

```yaml
agent_plugins:
  - path: plugins/scion-smoke
```
