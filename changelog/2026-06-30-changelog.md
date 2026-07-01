# Release Notes (2026-06-30)

A landmark feature landed: the managed agent backend, enabling Scion to orchestrate cloud-hosted agents (starting with Google's Gemini API) alongside its existing container-based runtime. The glossary was also ported to the repo root.

## 🚀 Features
* **[Managed Agent]:** Added the `ManagedAgentBackend` interface and Google API client — introduces a new execution path where `ManagedAgentManager` implements the existing `Manager` interface but delegates to a cloud API instead of a local Runtime+Harness pair. The first backend targets `generativelanguage.googleapis.com` (Gemini API). Includes SSE stream parser, hub handlers for managed agent CRUD, design document, and ~2800 lines of new code across 21 files (#541).

## 📖 Docs
* **[Glossary]:** Ported runtime broker taxonomy to root `GLOSSARY.md` — updated Runtime Broker definition and added Node-Bound Broker, Proxy Broker, Embedded Broker, Hosted Broker, and Managed Agent entries from the docs-site glossary (#546).
