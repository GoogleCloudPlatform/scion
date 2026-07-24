# Release Notes (2026-07-23)

The agent lineage graph view shipped for the web UI — a zoomable, pannable tree visualization of agent parent/child relationships — then was refactored into an inline component shared across agents and project-detail pages. GKE/IAP connectivity fixes landed for transport auth, GFE proxy health checks, and `whoami` gained typed output with Hub enrichment.

## 🚀 Features
* **[Web]:** Agent lineage graph view — renders agents as a parent/child forest with HTML card nodes, SVG cubic-curve edges, spawn-direction arrowheads, pan/zoom/fit-to-view, and collapse pruning. Third mode in the grid/list view toggle (#849).
* **[Web]:** Inline agent tree/graph view — refactored graph into a shared `<scion-agent-tree-view>` component rendered in the same content slot as grid/list, so status and label filters apply automatically. Used on both agents and project-detail pages (#854, #855).
* **[CLI]:** Enhanced `whoami` with typed `WhoamiResult` struct, Tier 1 env-var fields (project, template, harness, model, creator, etc.), and `--full` flag for Hub-enriched Tier 2 output (phase, ancestry, labels, taskSummary) with graceful degradation (#850).

## 🐛 Fixes
* **[CLI]:** Resolve transport auth before app-token gate in `attach` to unblock IAP mode — auth was checked after the token gate, preventing IAP-authenticated connections (#853).
* **[Hub/Broker]:** Handle GFE proxy interception of `/healthz` — detect non-JSON 2xx responses from reverse proxies and fall back to `/health` (hub) or return a descriptive error naming the likely cause (broker) (#852).
