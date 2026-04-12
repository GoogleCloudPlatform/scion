# Release Notes (2026-04-05)

This update focuses on improving authentication flexibility for Vertex AI, enhancing the reliability of template imports in restricted environments, and resolving workspace mounting inconsistencies.

## 🚀 Features
* **Vertex AI Authentication:** Added support for the `GOOGLE_APPLICATION_CREDENTIALS` environment variable as an alternative to `gcloud-adc` for Vertex AI authentication. The system now automatically detects these credentials if they are present in the environment, simplifying configuration for automated workflows and containerized deployments.

## 🐛 Fixes
* **Template Importing:** Migrated the template import process to use GitHub's tarball API via HTTPS. This removes the dependency on `svn` or `git` binaries and credentials for importing public templates, ensuring higher reliability in server-side and restricted environments.
* **Shared Workspace Mounting:** Fixed an issue where git-groves using shared workspaces were incorrectly mounted at `/repo-root`. They are now consistently mounted at `/workspace`, matching standard workspace behavior and resolving trust issues with tools like Claude Code.
* **Secret Management:** Resolved "Duplicate mount point" errors by deduplicating secrets that target the same container path. Precedence is given to higher-scoped secrets (e.g., grove-level over user-level), ensuring consistent configuration resolution.
* **CLI UX:** Added the missing `--project` flag to the `gcloud` verification command suggested in the failure dialog, providing users with a working command to resolve authentication issues.
