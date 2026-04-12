# Release Notes (2026-04-08)

This update significantly improves system stability by addressing critical race conditions in service management and resolving resource leaks and panics within the messaging broker. It also introduces a more robust agent authentication mechanism to prevent stale credential issues.

## 🐛 Fixes
* **[Agent Auth] Robust Token Management:** Transitioned agent authentication from the `SCION_AUTH_TOKEN` environment variable to a canonical file (`~/.scion/scion-token`). This ensures all processes—including the CLI, lifecycle hooks, and child agents—always use the latest refreshed token, eliminating failures caused by stale credentials.
* **[Service Manager] Thread-Safe Service Supervision:** Introduced mutex guarding for internal service state in `sciontool`. This resolves race conditions that caused inconsistent restart tracking and occasional crashes during rapid process exit/restart cycles.
* **[Broker] Messaging Stability & Performance:** 
    * **Resource Leak Fix:** Corrected a file descriptor leak in `runtimebroker` by ensuring WebSocket handshake responses are properly closed on dial failures.
    * **Panic Prevention:** Resolved a Hub-side panic by safely managing `BrokerConnection` closures using context cancellation instead of unsafe channel operations.
    * **Context Propagation:** Updated `InProcessBroker` to propagate publisher contexts to subscribers, allowing handlers to correctly respect timeouts and cancellations.
* **[Docs]:** Corrected minor typos in the README documentation.
