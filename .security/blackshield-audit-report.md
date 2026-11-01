# BlackShield Security Audit Report — `GoogleCloudPlatform/scion`

- **Scanned Commit:** `c91e8ecb61d862b37b8a103795948fbbcb20dfdf`
- **Actionable Findings:** `42` (`3 Critical`, `39 High`, `1 Medium`, `6 Low`)
- **BlackShield Platform & Continuous Zero-Cost PR Protection:** [https://blackshield.chaplau.com](https://blackshield.chaplau.com)

## Detailed Findings & Remediation Guide

### 1. [CRITICAL] `private-key` — Private Cryptographic Key Detected

- **Category:** `secret`
- **Location:** `.design/github-app.md:211`
- **Details:** Potential hardcoded credential or secret detected in `.design/github-app.md:211` (rule `private-key`). Raw value redacted (`***REDACTED***`) for safe disclosure.
- **Recommended Fix:** Revoke leaked credential immediately and rotate in secret manager.

### 2. [CRITICAL] `db-conn-postgres` — PostgreSQL Connection String with Credentials

- **Category:** `secret`
- **Location:** `.design/hosted/server-implementation-design.md:215`
- **Details:** Potential hardcoded credential or secret detected in `.design/hosted/server-implementation-design.md:215` (rule `db-conn-postgres`). Raw value redacted (`***REDACTED***`) for safe disclosure.
- **Recommended Fix:** Revoke leaked credential immediately and rotate in secret manager.

### 3. [CRITICAL] `db-conn-postgres` — PostgreSQL Connection String with Credentials

- **Category:** `secret`
- **Location:** `.design/hosted/server-implementation-design.md:589`
- **Details:** Potential hardcoded credential or secret detected in `.design/hosted/server-implementation-design.md:589` (rule `db-conn-postgres`). Raw value redacted (`***REDACTED***`) for safe disclosure.
- **Recommended Fix:** Revoke leaked credential immediately and rotate in secret manager.

### 4. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 5. [HIGH] `CKV_DOCKER_11` — CKV_DOCKER_11: Unverified Script Piped Directly to Shell

- **Category:** `misconfiguration`
- **Location:** `cr-ci/runner/Dockerfile:38`
- **Details:** Piping remote HTTP downloads directly into sh or bash executes unverified code without checksum verification.
- **Recommended Fix:** Download scripts first, verify their SHA-256 checksum against an expected digest, then execute.

### 6. [HIGH] `CKV_DOCKER_11` — CKV_DOCKER_11: Unverified Script Piped Directly to Shell

- **Category:** `misconfiguration`
- **Location:** `cr-ci/runner/Dockerfile:72`
- **Details:** Piping remote HTTP downloads directly into sh or bash executes unverified code without checksum verification.
- **Recommended Fix:** Download scripts first, verify their SHA-256 checksum against an expected digest, then execute.

### 7. [HIGH] `CKV_DOCKER_11` — CKV_DOCKER_11: Unverified Script Piped Directly to Shell

- **Category:** `misconfiguration`
- **Location:** `cr-ci/runner/Dockerfile:165`
- **Details:** Piping remote HTTP downloads directly into sh or bash executes unverified code without checksum verification.
- **Recommended Fix:** Download scripts first, verify their SHA-256 checksum against an expected digest, then execute.

### 8. [HIGH] `generic-secret-assignment` — Hardcoded Secret or API Key Assignment Detected

- **Category:** `secret`
- **Location:** `deploy/terraform/modules/hub-gke/main.tf:32`
- **Details:** Potential hardcoded credential or secret detected in `deploy/terraform/modules/hub-gke/main.tf:32` (rule `generic-secret-assignment`). Raw value redacted (`***REDACTED***`) for safe disclosure.
- **Recommended Fix:** Revoke leaked credential immediately and rotate in secret manager.

### 9. [HIGH] `CKV_DOCKER_11` — CKV_DOCKER_11: Unverified Script Piped Directly to Shell

- **Category:** `misconfiguration`
- **Location:** `docs-site/Dockerfile:19`
- **Details:** Piping remote HTTP downloads directly into sh or bash executes unverified code without checksum verification.
- **Recommended Fix:** Download scripts first, verify their SHA-256 checksum against an expected digest, then execute.

### 10. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `docs-site/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 11. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `extras/docs-agent/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 12. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `extras/scion-chat-app/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 13. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `extras/scion-discord/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 14. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `extras/scion-telegram/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 15. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/antigravity/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 16. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `harnesses/antigravity/Dockerfile:21`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 17. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/claude/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 18. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `harnesses/claude/Dockerfile:22`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 19. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `harnesses/claude/Dockerfile:63`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 20. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/codex/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 21. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/copilot/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 22. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/gemini-cli/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 23. [HIGH] `CKV_DOCKER_11` — CKV_DOCKER_11: Unverified Script Piped Directly to Shell

- **Category:** `misconfiguration`
- **Location:** `harnesses/grok-build/Dockerfile:37`
- **Details:** Piping remote HTTP downloads directly into sh or bash executes unverified code without checksum verification.
- **Recommended Fix:** Download scripts first, verify their SHA-256 checksum against an expected digest, then execute.

### 24. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/grok-build/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 25. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `harnesses/grok-build/Dockerfile:36`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 26. [HIGH] `CKV_DOCKER_11` — CKV_DOCKER_11: Unverified Script Piped Directly to Shell

- **Category:** `misconfiguration`
- **Location:** `harnesses/hermes/Dockerfile:25`
- **Details:** Piping remote HTTP downloads directly into sh or bash executes unverified code without checksum verification.
- **Recommended Fix:** Download scripts first, verify their SHA-256 checksum against an expected digest, then execute.

### 27. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/hermes/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 28. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/muse-code/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 29. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/opencode/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 30. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `harnesses/pi/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 31. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `image-build/core-base/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 32. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/core-base/Dockerfile:92`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 33. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `image-build/hub/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 34. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/hub/Dockerfile:24`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 35. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `image-build/omni/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 36. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/omni/Dockerfile:34`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 37. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/omni/Dockerfile:76`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 38. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `image-build/scion-base/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 39. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/scion-base/Dockerfile:22`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 40. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/scion-base/Dockerfile:70`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

### 41. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `image-build/thick-prep/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 42. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Missing Non-Root USER Instruction

- **Category:** `misconfiguration`
- **Location:** `image-build/thick-prep/Dockerfile:94`
- **Details:** Container executes processes as root user by default because no non-root USER instruction was specified.
- **Recommended Fix:** Add 'USER 10001' or a dedicated non-root application user.

