---
title: Metadata Host Hardening
description: Operator-managed host firewall and Kubernetes NetworkPolicy recipes for defense-in-depth cloud metadata isolation.
---

On Docker and rootful Podman in `block` or `assign` mode (outside `host` networking), Scion isolates agent containers from cloud instance metadata services (`169.254.169.254`, plus `fd20:ce::254` and `fd00:ec2::254` when the container network namespace has IPv6 enabled) using a short-lived setup container (`sciontool netns-setup`) while agent containers run without `CAP_NET_ADMIN` (and Docker/Podman agent containers drop `CAP_NET_RAW` in every metadata and network mode).

This page provides **optional, operator-managed defense-in-depth recipes** that enforce the same HTTP (`TCP/80`) block at the host kernel or Kubernetes cluster network layer, independently of container-side controls. Scion does not program host firewalls; these rules are operator-managed.

The files under [`deploy/host-hardening/`](https://github.com/GoogleCloudPlatform/scion/tree/main/deploy/host-hardening) are canonical; the inline snippets below mirror them.

:::caution[Scope: applies to every bridged container on the host]
The `nftables` and `iptables` host rules below match traffic forwarded from container bridge interfaces. They affect **every** bridged container on the host, including:

- Scion agents running in `passthrough` metadata mode, and
- non-Scion containers that rely on the host VM's metadata identity.

Enable these host rules only on hosts dedicated to `block` and `assign` agents.
:::

## Host firewall with nftables (recommended)

Use an `inet` table hooked at `forward` with `priority filter - 1` (running ahead of container-engine filter chains) to drop HTTP (`TCP/80`) to IPv4 and IPv6 cloud metadata endpoints from Docker, Podman, and CNI bridges (`docker0`, `br-*`, `podman*`, `cni-*`). One rule per interface pattern is used (rather than wildcards inside an anonymous set) so the ruleset loads across older `nftables` releases.

The drop rules match only `TCP/80`; the `accept` rules for `UDP/53` and `TCP/53` to `169.254.169.254` make the GCE DNS exemption explicit.

Canonical file: `deploy/host-hardening/scion-metadata-block.nft`

```nft
table inet scion_metadata_block
delete table inet scion_metadata_block

table inet scion_metadata_block {
    chain forward {
        type filter hook forward priority filter - 1; policy accept;

        # Make the GCE DNS (UDP/TCP 53 on 169.254.169.254) exemption explicit
        # (the drop rules below match only TCP/80).
        iifname "docker0" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
        iifname "br-*" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
        iifname "podman*" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
        iifname "cni-*" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept

        # Drop HTTP (TCP/80) to IPv4 cloud metadata endpoint.
        iifname "docker0" ip daddr 169.254.169.254 tcp dport 80 drop
        iifname "br-*" ip daddr 169.254.169.254 tcp dport 80 drop
        iifname "podman*" ip daddr 169.254.169.254 tcp dport 80 drop
        iifname "cni-*" ip daddr 169.254.169.254 tcp dport 80 drop

        # Drop HTTP (TCP/80) to IPv6 cloud metadata endpoints (GCE, AWS).
        iifname "docker0" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
        iifname "br-*" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
        iifname "podman*" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
        iifname "cni-*" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
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

On iptables-based Docker hosts, insert interface-scoped rules (`-i docker0` and `-i br-+`) into Docker's `DOCKER-USER` chain (which Docker preserves across daemon restarts and evaluates before its own `DOCKER` forwarding rules). The `DROP` rules match only `TCP/80`; inserting the `RETURN` rules ahead of them makes the GCE DNS (`UDP/53` and `TCP/53` to `169.254.169.254`) exemption explicit:

```bash
# IPv4: drop HTTP (TCP/80) to the cloud metadata endpoint from Docker bridges;
# RETURN rules make the DNS (UDP/TCP 53) exemption explicit.
for iface in docker0 br-+; do
  sudo iptables -I DOCKER-USER -i "$iface" -d 169.254.169.254/32 -p tcp --dport 80 -j DROP
  sudo iptables -I DOCKER-USER -i "$iface" -d 169.254.169.254/32 -p tcp --dport 53 -j RETURN
  sudo iptables -I DOCKER-USER -i "$iface" -d 169.254.169.254/32 -p udp --dport 53 -j RETURN
done

# IPv6 (when ip6tables and the IPv6 DOCKER-USER chain are active):
for iface in docker0 br-+; do
  sudo ip6tables -I DOCKER-USER -i "$iface" -d fd20:ce::254/128 -p tcp --dport 80 -j DROP
  sudo ip6tables -I DOCKER-USER -i "$iface" -d fd00:ec2::254/128 -p tcp --dport 80 -j DROP
done
```

An idempotent script version is provided at `deploy/host-hardening/scion-metadata-block-iptables.sh`. For hosts that also run Podman or CNI bridges, prefer the `nftables` recipe above.

## Persisting across reboots

The `systemd` unit and `cloud-init` snippet below persist the `nftables` ruleset. They do **not** persist the `iptables` / `DOCKER-USER` script above; to persist `DOCKER-USER` rules across reboots, either switch to the `nftables` unit below or run `scion-metadata-block-iptables.sh` from a `systemd` oneshot unit with `After=docker.service` and `Requires=docker.service` (or a `docker.service` drop-in `ExecStartPost=`).

### systemd oneshot unit

Install the unit at `/etc/systemd/system/scion-metadata-block.service` (canonical file: `deploy/host-hardening/scion-metadata-block.service`) so the ruleset loads before container daemons start:

```ini
[Unit]
Description=Scion host-level cloud metadata block for bridged containers
Wants=network-pre.target
Before=network-pre.target docker.service podman.service
After=nftables.service

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

Note that running `systemctl restart nftables` on a host whose `/etc/nftables.conf` starts with `flush ruleset` removes the `scion_metadata_block` table until `scion-metadata-block.service` is restarted (or `/etc/nftables.d/scion-metadata-block.nft` is included from `/etc/nftables.conf`).

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

              # Make the GCE DNS (UDP/TCP 53 on 169.254.169.254) exemption explicit
              # (the drop rules below match only TCP/80).
              iifname "docker0" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
              iifname "br-*" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
              iifname "podman*" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept
              iifname "cni-*" ip daddr 169.254.169.254 meta l4proto { udp, tcp } th dport 53 accept

              # Drop HTTP (TCP/80) to IPv4 cloud metadata endpoint.
              iifname "docker0" ip daddr 169.254.169.254 tcp dport 80 drop
              iifname "br-*" ip daddr 169.254.169.254 tcp dport 80 drop
              iifname "podman*" ip daddr 169.254.169.254 tcp dport 80 drop
              iifname "cni-*" ip daddr 169.254.169.254 tcp dport 80 drop

              # Drop HTTP (TCP/80) to IPv6 cloud metadata endpoints (GCE, AWS).
              iifname "docker0" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
              iifname "br-*" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
              iifname "podman*" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
              iifname "cni-*" ip6 daddr { fd20:ce::254, fd00:ec2::254 } tcp dport 80 drop
          }
      }

  - path: /etc/systemd/system/scion-metadata-block.service
    permissions: "0644"
    owner: root:root
    content: |
      [Unit]
      Description=Scion host-level cloud metadata block for bridged containers
      Wants=network-pre.target
      Before=network-pre.target docker.service podman.service
      After=nftables.service

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

On Kubernetes (including GKE with Workload Identity), the Runtime Broker stamps `scion.metadata-mode=<mode>` on each agent pod, where `<mode>` is `block` or `passthrough` (`passthrough` includes Workload Identity `assign` pods because the Runtime Broker rewrites `assign` to `passthrough` before emitting `SCION_METADATA_MODE`). You can enforce per-pod metadata blocking for `block`-mode agents with an egress `NetworkPolicy` that allows cluster DNS (`kube-dns`) and external IPv4 egress to `0.0.0.0/0` except `169.254.169.254/32` (cloud metadata server) and `169.254.169.252/32` (GKE metadata server).

Canonical file: `deploy/host-hardening/k8s-block-metadata-networkpolicy.yaml`

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
    # Allow cluster DNS (required on Cilium / GKE Dataplane V2 where ipBlock
    # does not match in-cluster pod IPs).
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # Allow external IPv4 egress except cloud and GKE metadata servers.
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 169.254.169.254/32
              - 169.254.169.252/32
```

:::caution[Enforcing CNI required; do not apply to `passthrough` / `assign` pods; allow in-cluster peers]
- **Enforcing CNI required:** Kubernetes `NetworkPolicy` objects are enforced only when the cluster runs an enforcing CNI such as **GKE Dataplane V2** or **Calico**. Without an enforcing CNI, `kubectl apply` succeeds silently without blocking traffic.
- **Keep scoped to `block` pods:** Do not widen the `podSelector` to match `passthrough` pods (which include Workload Identity `assign` pods). Those pods legitimately reach the GKE metadata server for Workload Identity tokens.
- **In-cluster peers on Cilium / GKE Dataplane V2:** `ipBlock: 0.0.0.0/0` does not match in-cluster pod IPs on Cilium / GKE Dataplane V2. The example includes an explicit `kube-dns` egress rule so DNS continues to work; add matching `namespaceSelector` / `podSelector` egress rules for any other in-cluster peers your agents must reach (such as an in-cluster Scion Hub or internal HTTP proxies). If NodeLocal DNSCache is enabled, also allow egress to its address and port (typically `169.254.20.10/32` UDP/TCP 53).
- **Existing pods after upgrade:** Pods created by older Runtime Brokers carry no `scion.metadata-mode` label, so this policy does not select them until they are recreated.
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
