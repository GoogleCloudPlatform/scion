# Release Notes (2026-05-05)

This update focuses on enhancing core images, improving the robustness of workspace management, and refining message routing in the chat application.

## 🚀 Features
* **[image]:** Added `openssh-client` to the `core-base` image to support SSH-based operations within the workspace.
* **[harness]:** Defaulted the Codex provisioner to `container-script` for more consistent environment setup.

## 🐛 Fixes
* **[chat-app]:** Improved message routing and privacy by ensuring only explicit instruction messages are relayed to chat and preventing harness output leaks. (Consolidated from 3 commits)
* **[workspace]:** Added a fallback to the default branch when cloning shared workspaces if the specified branch does not exist.
* **[hub]:** Refined grove slug derivation from the `--name` flag and improved visibility of names in confirmation prompts.
* **[scripts]:** Enhanced robustness of DNS updates and Caddy reloads in the `gce-certs.sh` script.
* **[util]:** Added normalization for trailing slashes in git remote URLs to prevent cloning errors.
