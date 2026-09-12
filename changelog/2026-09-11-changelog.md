# Release Notes (2026-09-11)

New Meta Muse Code harness and a security tightening on agent messaging permissions.

## 🚀 Features
* **Muse Code harness (#1541):** Complete harness-config bundle for Meta's Muse Code terminal coding agent — `config.yaml` with API key auth and declarative MCP mapping, `provision.py` with auth selection and instruction projection, `dialect.yaml` mapping all 13 hook events, Dockerfile, seed files, and 14 unit tests.

## 🔒 Security
* **Remove `agent.message` from project-member role (#1543):** Members could previously message any project-mode agent regardless of ownership. Messaging now requires owner/admin role or ancestry (agent creator), aligning with the terminal attach permission gate.
