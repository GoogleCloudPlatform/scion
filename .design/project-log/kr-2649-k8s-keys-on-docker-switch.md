# Clear and hide Kubernetes-only runtime keys on a switch to docker or podman

**Date:** 2026-11-06
**Issue:** ptone/scion#2649 (web only)

## Problem

In the admin server-config editor, switching a runtime entry from kubernetes to docker or podman
kept every Kubernetes-only key (`runtimeTypeSwitchKeys` cleared only the Cloud Run blocks for
non-Cloud-Run types), and the save sent those keys back to the hub.

## Change

`web/src/components/pages/admin-server-config.ts`:

- A switch to a non-Kubernetes container type clears the 12 Kubernetes-only keys and both Cloud Run
  blocks, keeping host, context, sync and env. Switches to Cloud Run, to a Kubernetes type or to no
  type keep their current behaviour.
- The Kubernetes-type rule is the shared `isKubernetesRuntimeType` from
  `web/src/shared/runtime-kind.ts` (kubernetes, k8s, remote), used by both the switch and
  `isKubernetesRuntime`. The page-local `isNonKubernetesContainerType` drives both the clear and the
  hide.
- A type switch drops the cached map rows (node_selector, KSA mappings) of a cleared field.
- Docker and podman cards leave out Namespace, GKE cluster, List all namespaces and Safe to Evict.
- Known residual: Kubernetes-only keys already stored on a docker/podman entry are hidden and
  round-trip as stored (no runtime effect); switching the entry to kubernetes and back clears them.

## Follow-ups

Four follow-ups were made: card-level coverage of the safe-to-evict "Ignored" hint
(including an untyped runtime whose name is not a Kubernetes name), `toHaveProperty(k, null)` for
the cleared-key payload checks, the page-local helpers un-exported (the Kubernetes-type rule now
reuses the shared helper instead of a page copy), and the PR text names the existing test whose
expectation changed on purpose.

## Lessons

- Look for an existing shared helper before adding a page-local predicate: `runtime-kind.ts`
  already had the Kubernetes-type rule.
- npm refuses `--userconfig /dev/null` together with `--globalconfig /dev/null`; point the global
  config at an empty file.
