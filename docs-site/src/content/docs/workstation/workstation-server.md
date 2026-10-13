---
title: Workstation Server Mode
---

The **Workstation Server Mode** allows you to run a standalone, single-tenant Scion Hub directly on your local machine. This is a "bonus" feature primarily intended for advanced users who want the benefits of the hosted architecture—such as the web dashboard and remote agent dispatch—without deploying a full, multi-user infrastructure.

## What is Workstation Server Mode?

In a standard local workflow, you run agents directly via the CLI (`scion start ...`). In a standard hosted workflow, a central Hub manages state for an entire team.

The Workstation Server mode bridges these two paradigms. It starts a local instance of the Scion Hub API and Web Dashboard, while also automatically registering your local machine as the default Runtime Broker. 

This enables you to:
1.  Manage your local agents through a visual interface (the Web Dashboard).
2.  Dispatch agents to your local machine from other devices on your network.
3.  Simulate a hosted environment for testing or custom template development.

## Starting the Server

To start the workstation server, use the `server start` command. By default, the server runs as a persistent background **daemon**, freeing up your terminal:

```bash
scion server start
```

This command performs several actions simultaneously:
1.  **Starts the Hub**: Binds a local API and Web server (defaulting to port 8080).
2.  **Starts a Broker**: Binds a local broker interface to execute containers.
3.  **Links Them**: Automatically registers the local broker with the local hub.

`scion server start` also adds the local Hub to your CLI's hub list as `local` if it is not there yet. It becomes the current Hub only when no Hub is current and your global settings name no other Hub in `hub.endpoint`. To use the local Hub together with remote ones, see [Working with Several Hubs](/scion/hosted/user/multiple-hubs/).

`scion server start` returns only after the started components answer their `/healthz` endpoints (the web port, which also serves the Hub API, and the broker port when the broker starts; without an image registry the server starts without the broker, and start waits only for the web port), so a command run right after it, such as `scion hub link`, can reach the server. If they do not answer within about 30 seconds, the command exits non-zero and says which component did not answer. The server is left running in that case; check the log and `scion server status`. If the server process exits during startup, the command says so at once. `scion server restart` waits the same way.

When the server is up, `scion server start` asks the Hub whether onboarding is complete. If it is not, it prints and opens the [onboarding wizard](/scion/getting-started/onboarding/) (`/onboarding`); otherwise it opens the Web Dashboard. If the Hub's onboarding status cannot be read, Scion opens the wizard only when no global settings file existed before this start.

**No image registry yet.** The Runtime Broker needs `image_registry` to pull agent images. If it is not set, the Hub and Web Dashboard still start, so the wizard (which sets the registry) is reachable, and the Runtime Broker is skipped with a warning. Set the registry in the wizard or with `scion config set --global image_registry <registry>`, then run `scion server restart` to start the Runtime Broker. (A hosted server still refuses to start without it.)

**Harness logins from your home directory.** Agents run by the co-located Runtime Broker can use the harness login files you already have in your home directory (for example a Claude or Codex login). Only files declared by the harness configs built into Scion are copied, and only when they resolve to a regular file inside your home directory. A Hub secret with the same name or target always wins.

Because it runs as a background daemon, you can manage its lifecycle using:
- `scion server status`: View running status, PID, and log file location. A degraded server (up, but a non-critical check such as the co-located broker is failing) is shown as running with the failing checks named; an unhealthy one (database or workspace storage failing) is shown as unhealthy. A degraded runtime broker is shown on its own line with its failing checks; the Hub API line reflects only the hub's own checks, and the Web Frontend line reflects the combined status. With `--json`, `hubRunning`/`webRunning`/`brokerRunning` are true when the component is up (healthy or degraded), `hubStatus`/`webStatus`/`brokerStatus` hold the status when it is not healthy (`degraded` or `unhealthy`), and `hubHealthReason`/`webHealthReason`/`brokerHealthReason` name the failing checks (for example `colocated_broker: unhealthy: registration failed`). The `*Status` and `*HealthReason` keys are omitted for a component that is healthy or not detected.
- `scion server restart`: Restart the daemon.
- `scion server stop`: Stop the background process. It waits up to 30 seconds for the process to exit. If it is still running after that, the command fails and keeps the PID file so the process can still be found; `scion server stop --force` then stops it.

If you prefer to run the server interactively, use the `--foreground` flag:

```bash
scion server start --foreground
```

You can now navigate to `http://localhost:8080` in your browser to access the Web Dashboard.

## Network Configuration and Bridges

Because the Workstation Server mode involves both a central Hub (running on your host network) and agents (running in isolated containers), network configuration can occasionally require attention, especially depending on your chosen runtime.

### Automatic Bridge (Podman, Docker)

If you are using **Podman** as your container runtime, Scion automatically creates and configures the necessary network bridge. The agents can communicate with the Hub (for API access, status updates, and SSE events) seamlessly via this internal network. No manual intervention is required.

### Manual Configuration

If you are using other runtimes, you may need to ensure that the containers can route traffic back to the host machine's IP address on the port where the Hub is listening (e.g., `8080`).

To configure the agents to find the Hub, you might need to adjust the `SCION_HUB_API_URL` setting passed to the agents in your template or via the CLI:

```bash
scion start my-agent --env SCION_HUB_API_URL=http://host.bridge.internal:8080
```

## Next Steps

While the Workstation Server is a powerful local tool, the concepts it uses—such as API tokens, Hub connections, and the Web Dashboard—are identical to those used in a full team deployment.

To learn more about how to navigate the UI, manage secrets via the Hub, or dispatch remote agents, refer to the [Hub User Guide](/scion/hosted/user/hosted-user/).
