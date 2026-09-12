---
name: docker-topology
description: >-
  Documents the sibling-container topology when an agent has Docker socket
  access. Covers bind-mount path resolution, volume strategies, and common
  pitfalls.
inject_when: docker_socket
---

# Docker Sibling-Container Topology

Your container has access to a Docker daemon socket. This means you can create
and manage Docker containers — but the topology is **sibling**, not
parent-child.

## Key Concept: You Are a Sibling, Not a Parent

```
┌──────────────────────────────┐
│       Docker daemon host     │
│                              │
│  ┌──────────┐ ┌──────────┐  │
│  │  You     │ │ Container│  │
│  │ (agent)  │ │ you start│  │
│  └──────────┘ └──────────┘  │
│       ▲              ▲      │
│       └──── peers ───┘      │
└──────────────────────────────┘
```

Containers you start via the Docker socket are **siblings** to your own
container, not children running inside it. Both are managed by the same Docker
daemon running on the host.

## Bind-Mount Paths Resolve on the Host

This is the most common source of silent failures. When you run:

```bash
docker run -v /workspace/data:/data my-image
```

The path `/workspace/data` is resolved **on the Docker daemon's host
filesystem**, not inside your container. If `/workspace/data` does not exist on
the host at that path, Docker silently creates an empty directory — your files
are not mounted and nothing errors.

### Why This Fails Silently

1. You see `/workspace/data` with files in your container.
2. You tell Docker to bind-mount `/workspace/data`.
3. Docker looks for `/workspace/data` **on the host** — not in your container.
4. The host path is different (or absent), so Docker creates it empty.
5. The new container starts with an empty `/data` directory — no error.

## Safe Volume Strategies

### Prefer Named Volumes for Sharing Data

Named volumes are managed by Docker and work regardless of container topology:

```bash
# Create a named volume
docker volume create my-data

# Use it from a container you start
docker run -v my-data:/data my-image
```

### Use Docker-Managed Paths When Bind-Mounting

If you must use bind mounts, the source path must exist **on the host**. Paths
that are valid inside your container are generally not valid as host paths.

### Copy Data via Docker Commands

Instead of bind-mounting, copy files in and out of containers:

```bash
# Copy a file into a container
docker cp /workspace/myfile.txt container_name:/app/myfile.txt

# Copy a file out of a container
docker cp container_name:/app/output.txt /workspace/output.txt
```

## Socket Bind-Mounts

If you need to pass the Docker socket into a container you create, use the same
path on both sides:

```bash
docker run -v /var/run/docker.sock:/var/run/docker.sock my-image
```

This works because the socket path is the same on the host and in your
container (it was bind-mounted into yours the same way).

## Quick Reference

| Scenario | Works? | Why |
|---|---|---|
| `docker run -v /workspace/foo:/foo img` | **No** | `/workspace/foo` resolves on host, not in your container |
| `docker run -v my-volume:/foo img` | **Yes** | Named volumes are host-managed |
| `docker cp local-file ctr:/path` | **Yes** | Copies bytes, no path resolution |
| `docker run -v /var/run/docker.sock:/var/run/docker.sock img` | **Yes** | Socket path is identical on host |
