# Release Notes (2026-04-29)

This release focuses on enhancing the project's documentation with a new marketing landing page and improving the reliability of broker-to-hub communication in local environments.

## 🚀 Features
* **[Docs Site]: New Project Landing Page.** A standalone marketing page has been added at `/scion/landing/`, featuring a project overview, hero graphic, quickstart guide, and embedded video tour.

## 🐛 Fixes
* **[Core]: Reliable Local Broker Connections.** Resolved an issue where brokers would fail to connect to the hub in environments without hairpin NAT support. By defaulting to `localhost` for co-located communication, terminal and heartbeat reliability is significantly improved.
* **[Docs Site]: Astro 6 & Zod 4 Compatibility.** Upgraded site dependencies to support the latest versions of Astro and Zod, ensuring long-term maintenance and build stability.
* **[Infrastructure]: Cloud Build & CI Updates.**
    * Fixed a bug where Docker authentication settings were being overwritten during the Cloud Build registry verification step.
    * Upgraded GitHub and Pages actions to Node.js 24 native versions.
