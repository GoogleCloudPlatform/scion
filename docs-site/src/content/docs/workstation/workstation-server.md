---
title: Workstation Server Mode
---

The **Workstation Server Mode** runs a standalone, single-tenant Scion Hub directly on your local machine. It is how Scion runs agents on your own machine: every `scion` command there goes through it, and it adds the web dashboard without deploying a full, multi-user infrastructure.

## What is Workstation Server Mode?

Scion always runs agents through a Hub. In a hosted workflow, a central Hub manages state for an entire team. On your own machine, the workstation server is that Hub: it starts a local instance of the Scion Hub API and Web Dashboard, and registers your machine as the default Runtime Broker.

This enables you to:
1.  Run agents from the CLI (`scion start ...`) in any repository on this machine. A repository links itself to this Hub on its first command, without a prompt.
2.  Manage your agents through a visual interface (the Web Dashboard).
3.  Simulate a hosted environment for testing or custom template development.

With no Hub configured, agent commands fail with a pointer to `scion server start`; when this server is configured but not running, they say `Local scion server is not running. Start it with 'scion server start'.` Nothing starts the server for you.

## Starting the Server

To start the workstation server, use the `server start` command. By default, the server runs as a persistent background **daemon**, freeing up your terminal:

```bash
scion server start
```

This command performs several actions simultaneously:
1.  **Starts the Hub**: Binds a local API and Web server (defaulting to port 8080), and saves its endpoint as `hub.endpoint` in your global settings (`Configured hub endpoint: http://127.0.0.1:8080`) when none is set, so CLI commands find it.
2.  **Starts a Broker**: Binds a local broker interface to execute containers.
3.  **Links Them**: Automatically registers the local broker with the local hub.

`scion server start` returns only after the started components answer their `/healthz` endpoints (the web port, which also serves the Hub API, and the broker port when the broker starts; without an image registry the server starts without the broker, and start waits only for the web port), so a command run right after it, such as `scion hub link`, can reach the server. If they do not answer within about 30 seconds, the command exits non-zero and says which component did not answer. The server is left running in that case; check the log and `scion server status`. If the server process exits during startup, the command says so at once. `scion server restart` waits the same way.

When the server is up, `scion server start` asks the Hub whether onboarding is complete. If it is not, it prints and opens the [onboarding wizard](/scion/getting-started/onboarding/) (`/onboarding`); otherwise it opens the Web Dashboard. If the Hub's onboarding status cannot be read, Scion opens the wizard only when no global settings file existed before this start.

**No image registry yet.** The Runtime Broker needs `image_registry` to pull agent images. If it is not set, the Hub and Web Dashboard still start, so the wizard (which sets the registry) is reachable, and the Runtime Broker is skipped with a warning. Set the registry in the wizard or with `scion config set --global image_registry <registry>`, then run `scion server restart` to start the Runtime Broker. (A hosted server still refuses to start without it.)

**No dev-auth banner.** The CLI reaches this server with its development token, by design. The `WARNING: Development authentication enabled` banner is therefore not shown for the local workstation Hub on loopback; every other Hub that uses dev auth still shows it.

**Harness logins from your home directory.** Agents run by the co-located Runtime Broker can use the harness login files you already have in your home directory (for example a Claude or Codex login). Only files declared by the harness configs built into Scion are copied, and only when they resolve to a regular file inside your home directory. A Hub secret with the same name or target always wins. Turn it off with `use_host_credentials: false`; see [Host credential files on a workstation](/scion/local/agent-credentials/#host-credential-files-on-a-workstation).

Because it runs as a background daemon, you can manage its lifecycle using:
- `scion server status`: View running status, PID, and log file location. A degraded server (up, but a non-critical check such as the co-located broker is failing) is shown as running with the failing checks named; an unhealthy one (database or workspace storage failing) is shown as unhealthy. A degraded runtime broker is shown on its own line with its failing checks; the Hub API line reflects only the hub's own checks, and the Web Frontend line reflects the combined status. With `--json`, `hubRunning`/`webRunning`/`brokerRunning` are true when the component is up (healthy or degraded), `hubStatus`/`webStatus`/`brokerStatus` hold the status when it is not healthy (`degraded` or `unhealthy`), and `hubHealthReason`/`webHealthReason`/`brokerHealthReason` name the failing checks (for example `colocated_broker: unhealthy: registration failed`). The `*Status` and `*HealthReason` keys are omitted for a component that is healthy or not detected.
- `scion server restart`: Restart the daemon.
- `scion server stop`: Stop the background process. It waits up to 30 seconds for the process to exit. If it is still running after that, the command fails and keeps the PID file so the process can still be found; `scion server stop --force` then stops it.

### Starting at login

`scion server install` prints a service file that runs the server in the foreground under your service manager: a systemd user unit on Linux, a launchd agent on macOS.

```bash
# Linux (systemd user service)
scion server install > ~/.config/systemd/user/scion-server.service
systemctl --user daemon-reload
systemctl --user enable --now scion-server

# macOS (launchd user agent)
scion server install > ~/Library/LaunchAgents/io.scion.server.plist
launchctl load ~/Library/LaunchAgents/io.scion.server.plist
```

### Foreground

If you prefer to run the server interactively, use the `--foreground` flag:

```bash
scion server start --foreground
```

You can now navigate to `http://localhost:8080` in your browser to access the Web Dashboard.

## Network Configuration and Bridges

Because the Workstation Server mode involves both a central Hub (running on your host network) and agents (running in isolated containers), network configuration can occasionally require attention, especially depending on your chosen runtime.

### Automatic Bridge (Podman, Docker)

If you are using **Podman** or **Docker** as your container runtime, Scion automatically creates and configures the necessary network bridge. The agents can communicate with the Hub (for API access, status updates, and SSE events) seamlessly via this internal network. No manual intervention is required.

### Apple container (macOS)

On macOS, Scion picks the Apple `container` runtime first when it is installed (then Podman, then Docker). Apple container needs a `sudo` DNS rule that must be re-created after each reboot (or installed as a LaunchDaemon) before agents can reach the Hub; the onboarding wizard's runtime step shows the command. See [Apple Container DNS](/scion/local/apple-container/), or choose Podman or Docker instead.

### Manual Configuration

If you are using other runtimes, you may need to ensure that the containers can route traffic back to the host machine's IP address on the port where the Hub is listening (e.g., `8080`).

To configure the agents to find the Hub, you might need to adjust the `SCION_HUB_API_URL` setting passed to the agents in your template or via the CLI:

```bash
scion start my-agent --env SCION_HUB_API_URL=http://host.bridge.internal:8080
```

## Next Steps

While the Workstation Server is a powerful local tool, the concepts it uses—such as API tokens, Hub connections, and the Web Dashboard—are identical to those used in a full team deployment.

To learn more about how to navigate the UI, manage secrets via the Hub, or dispatch remote agents, refer to the [Hub User Guide](/scion/hosted/user/hosted-user/).
