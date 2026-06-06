# Release Notes (2026-06-04)

This release marks a major architectural milestone for Scion, introducing the foundational components for **Multi-node and Distributed Operations**. The system now supports horizontal scaling of Hub replicas, distributed message brokering, and shared agent workspaces.

## 🚀 Features

* **Postgres Storage Backend:** Migrated the core persistence layer to a Postgres backend using `ent` and `pgx/v5`. This shift enables multiple Hub replicas to share state, leveraging Postgres-native advisory locks for distributed coordination and `LISTEN/NOTIFY` for efficient, real-time cross-node event propagation.
* **Multi-node Broker Dispatch:** Introduced a distributed dispatching system for message brokers. This includes support for broker affinity, durable intent tracking, and intelligent message routing across a cluster, ensuring reliable communication in multi-node deployments.
* **NFS-Coordinated Workspace Sharing:** Implemented shared workspace support via NFS, allowing agents running on different nodes to access and coordinate on the same project data. This feature provides a unified storage model across Docker (Model A) and GKE/Kubernetes (Model B) environments.

## 🧹 Chores & Internal

* **Engineering Glossary:** Added a comprehensive `GLOSSARY.md` to the repository root to establish a canonical "ubiquitous language" for Scion terminology.
* **Developer Tooling Reorganization:** Consolidated developer convenience scripts and Go tools into the `hack/` directory and added Kubernetes manifests for testing NFS workspace scenarios.
* **Cleanup:** Removed legacy scratchpad markdown files and optimized the internal build configuration for developer tools.
