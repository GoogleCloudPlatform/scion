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

Because it runs as a background daemon, you can manage its lifecycle using:
- `scion server status`: View running status, PID, and log file location.
- `scion server restart`: Restart the daemon.
- `scion server stop`: Stop the background process.

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

To learn more about how to navigate the UI, manage secrets via the Hub, or dispatch remote agents, refer to the [Hub User Guide](/scion/hub-user/hosted-user/).
