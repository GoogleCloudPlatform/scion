# Release Notes (2026-06-11)

The skill bank feature landed in a single massive PR — a complete registry, provisioning, and caching system for reusable agent skills. This is the largest single change in the project's history at 18,000+ lines spanning milestones M1 through M6.

## 🚀 Features
* **[Skill Bank — M1: Agent-Side Provisioning]:** Added `SkillReference` type with URI parser supporting full, shorthand, alias, and bare name forms. Integrated skill resolution into the agent provisioning pipeline with fail-closed safety (S1): required skills without a resolver cause provisioning failure. Skills are installed via staging with per-file SHA-256 hash verification (S2), path safety validation (S3), and atomic rename (#399).
* **[Skill Bank — M2: Hub API & Storage]:** Full CRUD API for skills and versions — create, list, get, update, delete, publish with signed URL upload workflow. Batch resolve endpoint with scope search order (user > project > global > core) and semver constraint matching (exact, `^`, `~`, `>=`, `latest`, `sha256:` content-addressed). Ent schemas for `Skill` and `SkillVersion` entities with comprehensive store tests (#399).
* **[Skill Bank — M2: CLI]:** `scion skills` command group with `list`, `show`, `create` (scaffold), `publish` (upload + finalize), `delete`, `versions`, and `resolve` subcommands. Includes `scion skill` singular alias and `--format json` support (#399).
* **[Skill Bank — M3: Hub Resolver Wiring]:** `HubSkillResolver` adapter bridging `hubclient.SkillService` to the agent `SkillResolver` interface, injected at both CLI and broker call sites with identity propagation (#399).
* **[Skill Bank — M4: Broker-Side Caching]:** Content-hash-keyed caching for resolved skills using the existing `templatecache.Cache` infrastructure (500MB default, `~/.scion/cache/skills/`). Cache hits verified with SHA-256 on read; mismatches evict and re-download. Latest/range constraints always re-resolve against Hub but skip download on cache hit (#399).
* **[Skill Bank — M6: Discovery & Lifecycle]:** Tag filtering with AND semantics and case-insensitive matching. Deprecation workflow with version-level deprecation messages, optional replacement URIs, and warnings surfaced during resolution. Download counter with atomic per-version increment. Published versions preferred over deprecated in latest/constraint resolution (#399).

## 🔒 Security
* **[Skill Bank]:** ActionRead authorization checks added to all read/download/resolve endpoints. User scope authorization gap fixed in `createSkill`. Batch resolve item cap (max 50) to prevent abuse. Cache hit hash verification to detect corruption. HTTPS-only downloads (S5) with cross-host redirect rejection (#399).
