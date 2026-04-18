# Release Notes (2026-04-17)

This release introduces native Discord notification support and significantly improves the real-time experience in the Messages tab by switching to a store-backed SSE stream. It also addresses several access control and dashboard consistency issues.

## ⚠️ BREAKING CHANGES
* **Discord Webhook Validation:** The new native Discord notification channel now explicitly rejects Discord webhook URLs that end with the `/slack` compatibility suffix. If you were previously using a Discord webhook via the Slack channel type, you must remove the `/slack` suffix and switch the channel type to `discord`. Note that `@here` and `@everyone` mentions are no longer supported; use specific role or user IDs instead (e.g., `"<@&ROLE_ID>"`).

## 🚀 Features
* **Native Discord Notification Channel:** Added support for Discord webhooks as a native notification channel. This includes colour-coded severity levels, support for urgent mentions (roles/users), and robust payload validation (PR #151).
* **Messages Tab Real-time Streaming:** Implemented a new Hub-store backed SSE stream for per-agent Messages tabs. This enables real-time message updates on any Hub deployment, regardless of whether Cloud Logging is enabled (PR #144).

## 🐛 Fixes
* **Administrative Access Control:** Resolved an issue where grove owners and admins who were not the original creator were sometimes denied full access. They can now edit grove settings, manage members, and control agents created by other members.
* **Dashboard Accuracy:** Resolved an issue where dashboard resource counts would not consistently update upon page load (PR #155).
* **Web Performance and Stability:** Optimized the home page to use hydrated SSR data when available and added safeguards against race conditions and stale UI mutations during navigation.
* **SSE Reliability:** Improved the reliability of the Messages SSE stream by implementing auto-refetching upon reconnection to ensure no messages are missed during transient network interruptions.
