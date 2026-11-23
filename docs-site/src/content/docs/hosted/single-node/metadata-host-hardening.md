---
title: Metadata Host Hardening
description: Operator-managed host firewall and Kubernetes NetworkPolicy recipes for defense-in-depth cloud metadata isolation.
---

Scion isolates agent containers from cloud instance metadata services (`169.254.169.254`, `fd20:ce::254`, and `fd00:ec2::254`) using a short-lived setup container (`sciontool netns-setup`) that installs per-namespace netfilter rules while agent containers run without `CAP_NET_ADMIN` or `CAP_NET_RAW`.

This page provides **optional, operator-managed defense-in-depth recipes** that enforce the same HTTP (`TCP/80`) block at the host kernel or Kubernetes cluster network layer, independently of container-side controls. Scion does not install or manage these rules automatically: Runtime Brokers run unprivileged against container sockets and cannot program host firewalls.

Ready-to-use files for every snippet below live under [`deploy/host-hardening/`](https://github.com/GoogleCloudPlatform/scion/tree/main/deploy/host-hardening) in the repository.

:::caution[Scope: applies to every bridged container on the host]
The `nftables` and `iptables` host rules below match traffic forwarded from container bridge interfaces. They affect **every** bridged container on the host, including:

- Scion agents running in `passthrough` metadata mode, and
- non-Scion containers that rely on the host VM's metadata identity.

Enable these host rules only on hosts dedicated to `block` and `assign` agents.
:::

## Host firewall with nftables (recommended)

Use an `inet` table hooked at `forward` with `priority filter - 1` (running ahead of container-engine filter chains) to drop HTTP (`TCP/80`) to IPv4 and IPv6 cloud metadata endpoints from Docker, Podman, and CNI bridges (`docker0`, `br-*`, `podman*`, `cni-*`).

DNS (`UDP/53` and `TCP/53`) to `169.254.169.254` stays allowed because Google Compute Engine VMs use `169.254.169.254` as their DNS resolver.

File: `deploy/host-hardening/scion-metadata-block.nft`

```nft
table inet scion_metadata_block
delete table inet scion_metadata_block

table inet scion_metadata_block {
    chain forward {
        type filter hook forward priority filter - 1; policy accept;

        # Keep GCE metadata-server DNS (UDP/TCP 53) allowed.
        iifname { "docker0", "br-*", "podman*", "cni-*" } ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept

        # Drop HTTP (TCP/80) to IPv4 cloud metadata endpoint.
        iifname { "docker0", "br-*", "podman*", "cni-*" } ip daddr 169.254.169.254 tcp dport 80 drop

        # Drop HTTP (TCP/80) to IPv6 cloud metadata endpoints (GCE, AWS).
        iifname { "docker0", "br-*", "podman*", "cni-*" } ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
    }
}
```

Validate and apply:

```bash
sudo install -D -m 0644 deploy/host-hardening/scion-metadata-block.nft /etc/nftables.d/scion-metadata-block.nft
sudo nft -c -f /etc/nftables.d/scion-metadata-block.nft
sudo nft -f /etc/nftables.d/scion-metadata-block.nft
```

## Host firewall with iptables (`DOCKER-USER`)

On iptables-based Docker hosts, insert rules into Docker's `DOCKER-USER` chain (which Docker preserves across daemon restarts and evaluates before its own `DOCKER` forwarding rules). Insert the `DROP` rule first, then insert the DNS `RETURN` rules ahead of it so UDP/TCP 53 to `169.254.169.254` stays allowed:

```bash
# IPv4: drop HTTP (TCP/80) to the cloud metadata endpoint, keeping DNS (53) allowed.
sudo iptables -I DOCKER-USER -d 169.254.169.254/32 -p tcp --dport 80 -j DROP
sudo iptables -I DOCKER-USER -d 169.254.169.254/32 -p tcp --dport 53 -j RETURN
sudo iptables -I DOCKER-USER -d 169.254.169.254/32 -p udp --dport 53 -j RETURN

# IPv6 (when ip6tables and the IPv6 DOCKER-USER chain are active):
sudo ip6tables -I DOCKER-USER -d fd20:ce::254/128 -p tcp --dport 80 -j DROP
sudo ip6tables -I DOCKER-USER -d fd00:ec2::254/128 -p tcp --dport 80 -j DROP
```

An idempotent script version is provided at `deploy/host-hardening/scion-metadata-block-iptables.sh`. For hosts that also run Podman or CNI bridges, prefer the `nftables` recipe above.

## Persisting across reboots

### systemd oneshot unit

Install the unit at `/etc/systemd/system/scion-metadata-block.service` (file: `deploy/host-hardening/scion-metadata-block.service`) so the ruleset loads before container daemons start:

```ini
[Unit]
Description=Scion host-level cloud metadata block for bridged containers
After=network-pre.target
Before=docker.service podman.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/scion-metadata-block.nft
ExecReload=/usr/sbin/nft -f /etc/nftables.d/scion-metadata-block.nft
ExecStop=/usr/sbin/nft delete table inet scion_metadata_block

[Install]
WantedBy=multi-user.target
```

Enable it:

```bash
sudo install -D -m 0644 deploy/host-hardening/scion-metadata-block.service /etc/systemd/system/scion-metadata-block.service
sudo systemctl daemon-reload
sudo systemctl enable --now scion-metadata-block.service
```

### cloud-init snippet

For automated VM provisioning, `deploy/host-hardening/cloud-init.yaml` installs both `/etc/nftables.d/scion-metadata-block.nft` and `/etc/systemd/system/scion-metadata-block.service` and enables the unit on first boot:

```yaml
#cloud-config
packages:
  - nftables

write_files:
  - path: /etc/nftables.d/scion-metadata-block.nft
    permissions: "0644"
    owner: root:root
    content: |
      table inet scion_metadata_block
      delete table inet scion_metadata_block

      table inet scion_metadata_block {
          chain forward {
              type filter hook forward priority filter - 1; policy accept;

              iifname { "docker0", "br-*", "podman*", "cni-*" } ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
              iifname { "docker0", "br-*", "podman*", "cni-*" } ip daddr 169.254.169.254 tcp dport 80 drop
              iifname { "docker0", "br-*", "podman*", "cni-*" } ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
          }
      }

  - path: /etc/systemd/system/scion-metadata-block.service
    permissions: "0644"
    owner: root:root
    content: |
      [Unit]
      Description=Scion host-level cloud metadata block for bridged containers
      After=network-pre.target
      Before=docker.service podman.service

      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/usr/sbin/nft -f /etc/nftables.d/scion-metadata-block.nft
      ExecReload=/usr/sbin/nft -f /etc/nftables.d/scion-metadata-block.nft
      ExecStop=/usr/sbin/nft delete table inet scion_metadata_block

      [Install]
      WantedBy=multi-user.target

runcmd:
  - systemctl daemon-reload
  - systemctl enable --now scion-metadata-block.service
```

## Kubernetes egress NetworkPolicy

On Kubernetes (including GKE with Workload Identity), the Runtime Broker stamps `scion.metadata-mode=<mode>` (`block`, `assign`, or `passthrough`) on each agent pod. You can enforce per-pod metadata blocking for `block`-mode agents with an egress `NetworkPolicy` that allows IPv4 egress to `0.0.0.0/0` except `169.254.169.254/32` (cloud metadata server) and `169.254.169.252/32` (GKE metadata server).

File: `deploy/host-hardening/k8s-block-metadata-networkpolicy.yaml`

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: scion-block-agent-metadata-egress
  namespace: scion-agents
spec:
  podSelector:
    matchLabels:
      scion.metadata-mode: block
  policyTypes:
    - Egress
  egress:
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 169.254.169.254/32
              - 169.254.169.252/32
```

:::caution[Enforcing CNI required; do not apply to `assign` or `passthrough` pods]
- **Enforcing CNI required:** Kubernetes `NetworkPolicy` objects are enforced only when the cluster runs an enforcing CNI such as **GKE Dataplane V2** or **Calico**. Without an enforcing CNI, `kubectl apply` succeeds silently without blocking traffic.
- **Keep scoped to `block` pods:** Do not widen the `podSelector` to match `assign` or `passthrough` pods. Those pods legitimately reach the GKE metadata server for Workload Identity tokens.
- **Dual-stack clusters:** On clusters where pods require IPv6 egress, add an IPv6 `ipBlock` rule (`::/0` except `fd20:ce::254/128` and `fd00:ec2::254/128`).
:::

See [Kubernetes Runtime — `block`](/scion/hosted/ha/kubernetes/#block) for how Scion configures `block`-mode pods and Workload Identity ServiceAccounts.

## What this does not cover

The host `forward`-hook recipes above do **not** cover:

- **macOS VM daemons** (Docker Desktop, OrbStack, Podman Machine): these run on local workstations with no cloud instance metadata server to protect.
- **Remote container daemons**: when a Runtime Broker connects to a remote Docker or Podman daemon, apply the host firewall rules on the **daemon host**, not on the Runtime Broker host.
- **Host-network containers (`--network=host`)**: containers sharing the host network namespace do not traverse the kernel `forward` hook. Use Scion's host-network refusal policy (`SCION_HOST_NETWORK_BLOCK_POLICY`; see [Host networking on Docker and Podman](/scion/hosted/single-node/auth/#host-networking-on-docker-and-podman)) instead.
- **Rootless Podman**: `pasta` / `slirp4netns` user-mode networking originates egress from a host user process rather than a container bridge, so `forward`-hook bridge rules do not match it. If rootless Podman runs on a cloud VM and needs host-level enforcement, add a host `output` rule scoped to the rootless user's UID (keeping DNS on port 53 allowed).

## Related documentation

- [Authentication & Identity — GCP Identity & Metadata Emulation](/scion/hosted/single-node/auth/#gcp-identity--metadata-emulation)
- [Security Architecture — Credentials Propagation](/scion/reference/security/#45-credentials-propagation)
- [Kubernetes Runtime — `block`](/scion/hosted/ha/kubernetes/#block)
