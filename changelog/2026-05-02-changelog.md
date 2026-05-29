# Release Notes (2026-05-02)

This update focuses on critical infrastructure stability and reliability, particularly addressing networking conflicts on starter-hub VMs and ensuring message delivery persistence across server restarts.

## 🚀 Features
* **No new features were introduced in this release.**

## 🐛 Fixes
* **Networking (Metadata Sidecar):** Resolved a critical circular dependency issue where iptables REDIRECT rules leaked to the host namespace in host-network mode, resulting in 502 Bad Gateway errors. This fix includes the introduction of a `CachedGCPTokenGenerator` with singleflight deduplication to optimize IAM token fetching.
* **Hub Message Broker:** Improved message delivery reliability by bootstrapping broker subscriptions for all active projects on startup. Previously, messages sent immediately after a restart could be lost if the subscriptions hadn't been re-established by a lifecycle event.
* **Chat Application:** Fixed signing key auto-discovery by correcting the GCP project resolution order, ensuring the application correctly identifies keys stored within the Hub's project.
* **API Client Stability:** Prevented a panic in the API client's pagination logic when no options are provided, which was affecting chat application registration flows.
* **Message Validation:** Added validation to the message broker to explicitly reject outbound messages addressed to unknown recipients.
* **Documentation:** Published a comprehensive post-mortem detailing the root cause and resolution of recent metadata server failures on starter-hub VMs.
