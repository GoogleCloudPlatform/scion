# Host-Level Metadata Hardening Examples

This directory contains operator-managed, defense-in-depth examples for blocking
cloud instance metadata HTTP access (TCP/80) from container workloads at the
host or cluster network layer. Scion does not install or manage these rules
automatically.

See the full operator guide at
`docs-site/src/content/docs/hosted/single-node/metadata-host-hardening.md`.

## Files

| File | Purpose |
| :--- | :--- |
| `scion-metadata-block.nft` | `nftables` (`inet`, `forward` hook at `priority filter - 1`) ruleset dropping TCP/80 to `169.254.169.254`, `fd20:ce::254`, and `fd00:ec2::254` from container bridges (`docker0`, `br-*`, `podman*`, `cni-*`) while keeping DNS (UDP/TCP 53) to `169.254.169.254` allowed. |
| `scion-metadata-block-iptables.sh` | Idempotent `iptables` / `ip6tables` (`DOCKER-USER` chain) equivalent for iptables-based Docker hosts. |
| `scion-metadata-block.service` | `systemd` oneshot unit that loads `scion-metadata-block.nft` before `docker.service` and `podman.service`. |
| `cloud-init.yaml` | `cloud-init` snippet installing `scion-metadata-block.nft` and `scion-metadata-block.service` at VM boot. |
| `k8s-block-metadata-networkpolicy.yaml` | Kubernetes `NetworkPolicy` for `scion.metadata-mode: block` pods allowing IPv4 egress except `169.254.169.254/32` and `169.254.169.252/32` (GKE metadata server). Requires an enforcing CNI and must not be applied to `assign` or `passthrough` pods. |

## Scope Caveat

The host firewall examples (`scion-metadata-block.nft` and
`scion-metadata-block-iptables.sh`) affect **every** bridged container on the
host, including Scion agents in `passthrough` mode and non-Scion containers that
rely on the host VM's metadata identity. Enable them only on hosts dedicated to
`block` and `assign` agents.
