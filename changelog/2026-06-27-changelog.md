# Release Notes (2026-06-27)

A single targeted fix: GitHub URL parsing now correctly handles branch names containing slashes when resolving remote template references.

## 🐛 Fixes
* **[Config]:** Handle branch names with slashes in GitHub URL parsing — `resolveGitHubRef` now uses `git ls-remote --heads` to disambiguate branch vs path segments, picking the longest matching ref. Previously, branches like `feature/foo` were incorrectly split at the first slash, misidentifying part of the branch name as a file path (#503).
