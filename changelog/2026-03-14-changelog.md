# Release Notes (Mar 14, 2026)

This release introduces the foundational infrastructure for the Scion plugin system, adds comprehensive support for syncing grove-level templates, and unifies all Grove IDs to a standard UUID format.

## ⚠️ BREAKING CHANGES
* **Grove ID Format Unification:** All Grove IDs have been standardized to a unified UUID format. Git-backed groves now use a deterministic UUID v5 (based on the namespace and normalized URL) instead of a 16-character hex hash, while non-git and hub-managed groves continue using UUID v4. Existing git-backed groves may need to be re-linked, and any integrations relying on the old hex format must be updated (commit e896693).

## 🚀 Features
* **Plugin System Infrastructure:** Introduced the core architecture for a new Scion plugin system using `hashicorp/go-plugin`, complete with reference implementations for message broker and agent harness plugins (consolidated from commits 6c543d0, b1a5ae1, 22991ec).
* **Grove Template Sync & Management:** Implemented capabilities for syncing grove-level templates with the Hub. This includes new API endpoints (`POST /api/v1/groves/{groveId}/sync-templates`), CLI commands (`scion templates sync --all`, `scion templates status`), and a dedicated Web UI for managing synced templates. Additionally, machine-specific settings for git-backed groves are now externalized, while templates remain in-repo to support version control (consolidated from commits d0507b1, 3c9cb4b, 0cf62d7, ef4f208, 56df5b4).
* **CLI Navigation Commands:** Added `config dir`, `cd-config`, and `cd-grove` commands to simplify locating and navigating to configuration and workspace directories (commit 596295d).

## 🐛 Fixes
* **Agent Git Cloning:** Resolved an issue where git clones would hang indefinitely when authentication was required but no token was present. Added proper error state reporting upon clone failures, and corrected the `agent-info.json` path to correctly use the `scion` user's home directory (consolidated from commits 93dfdcd, 7ec5eb2).
* **Image Builds:** Fixed the Google Cloud SDK installation in the build environment by explicitly using `apt-get` (commit d76197c).
