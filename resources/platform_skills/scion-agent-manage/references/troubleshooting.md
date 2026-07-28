# Troubleshooting and Recovery

Most stuck agents are recoverable without recreation. Always start with `scion look <agent>` to inspect current state before acting.

## Triage Table

| Symptom | Cause | Recovery |
|---|---|---|
| Transient API error in log | LLM provider rate-limit or timeout | `scion message <agent> "continue"` — do NOT recreate |
| `LIMITS_EXCEEDED` state | Context/rate limit hit | `scion message <agent> "continue"` |
| `failed to verify token: error in cryptographic primitive` | Hub regenerated signing keys on restart, or multi-replica key mismatch | `scion message <agent> "continue"` — the message is delivered via the broker's HMAC control channel (bypasses the agent's JWT). If looping across `login` / `session_expired`, the signing key itself is gone — escalate to an operator to pin `SharedSigningSecret` (`SESSION_SECRET`) in deployment config so keys survive restarts. A pinned `HubID` is a stopgap that avoids a rebuild but requires matching across replicas |
| `Token refresh failed … 401` repeating every 30s | Token expired; refresh deadlocked | `scion message <agent> "continue"` — if no effect, recreate |
| Container exit 255 / status `Exited` | Container crash | Recreate the agent |
| Phase `created`, lastSeen zero, persists 5+ min | Broker dispatch exceeded CLI timeout | Wait a few minutes; if stuck, delete and recreate. **If recurring:** broker is under pressure — reduce concurrent agent count rather than retrying |
| `scion start` fails with 422 `no_runtime_broker` (hub log shows `onlineProviders: 0` after broker reconnects) | Broker heartbeat/provider status split-brain during reconnect — two distinct races: (1) heartbeat overwrites `broker.Status` to online but `ProjectProvider.Status` stays offline; (2) TOCTOU between release and heartbeat (no session guard) | Wait 30–60s for heartbeat cycle to reconcile, then retry. If persistent, the hub process may need a restart. Reduce concurrent starts if it recurs under load. **Note:** this symptom applies to `scion start` — if the agent is already running, it is unaffected by the provider-status race |
| Phase `starting` + activity `completed` | Duplicate sciontool process reset phase via hub API. Common trigger: `go test` (or any child process) inheriting hub env vars and spawning a second `sciontool init` | Kill duplicate process, recreate. **Diagnostic:** look for `sciontool init starting as PID <different-pid>` and `Failed to start telemetry: ... address already in use` in agent logs — the second PID and the port conflict confirm a duplicate |
| Context at 100% | Memory/context limit reached | Send raw clear sequence (see below) |
| `gh` CLI returns 401 | Stale `GH_TOKEN` env var, not an agent state issue | Fall back to `curl` with known-good token |
| Rebase fails "unrelated histories" | Shallow clone hides common ancestor | `git fetch --unshallow` then retry `git rebase origin/main` — **not** force-push or branch recreation |
| Interactive prompt blocking agent | Harness waiting for user input | `scion message <agent> --raw "ENTER"` or appropriate dismissal |

## Context Clear

When an agent's context approaches 100%, clear it manually with raw terminal input:

```bash
scion message <agent> --raw "/"
scion message <agent> --raw "clear"
scion message <agent> --raw "ENTER"
```

Always `scion look <agent>` first to verify screen state before sending raw input.

## Anti-Patterns

- **Recreating on first sign of trouble.** Most stuck states recover with `scion message <agent> "continue"`. Recreation destroys uncommitted work and in-memory state.
- **Sending raw input blind.** Always `scion look` first — raw keystrokes go to whatever is on screen.
- **Treating all 401s the same.** Hub token 401 (agent state) and GitHub token 401 (API auth) have different recovery paths.
