# Release Notes (2026-08-18)

A light day with targeted web chat polish: an expand popover for truncated agent-agent messages and an SSE attachment rendering fix.

## 🚀 Features
* **[Chat]:** Add expand popover for truncated agent-agent messages — zoom/expand icon opens a full-screen overlay with the complete message rendered as markdown, with close via X button or click-outside (#1221).

## 🐛 Fixes
* **[Chat]:** Render attachment previews immediately on SSE messages — move `v2AttachmentMap.set()` before `mergeMessages()` so the Lit re-render sees attachment refs immediately instead of waiting for the next user-triggered render (#1219).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 17 — new Helm deploy guide (158 lines), web chat feature gap Phases 0–5, string-slice CLI flags, IAP audience warning, boolean env var parsing, GitHub App config persistence, HA preflight split (#1218).
