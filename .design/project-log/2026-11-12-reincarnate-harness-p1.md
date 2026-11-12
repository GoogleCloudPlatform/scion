# Reincarnate --harness, Phase 1 (local-disk runtimes)

**Date:** 2026-11-12
**Branch:** scion/reincarnate-harness-p1
**Issues:** ptone/scion#7758 (phase), ptone/scion#7757 (tracking)

## What landed

`scion reincarnate <agent> --harness <harness-config>` moves an agent to
another harness config in place and resets its home. It is gated by the
`hub.reincarnate_harness_switch` experiment (alpha, off).

- **Broker / agent.** `CreateAgentRequest.ResetHome` + `ResetHomeID`
  (provision-only reprovision only; ID `[A-Za-z0-9-]{1,64}`).
  `Manager.Reprovision` moves the home aside after every precondition, as
  `.removing-home-<id>` in the home's parent, writes
  `<agentDir>/home-reset.json` through `privatefs`, then the normal
  `ProvisionAgent` composes a fresh home. Purge: background after the
  reset, a broker startup sweep over `<global>/projects/*` and
  `<global>/project-configs/*`, and agent delete (the moved-aside home sits
  inside an agent directory that delete removes). A failure after the move
  wraps `agent.ErrHomeResetApplied`; a precondition refusal before it
  answers 409 `reprovision_refused_home_intact`. Capability
  `reprovisionResetHomeRuntimes = [docker, podman, container]` (heartbeat,
  info, and registration names `reprovisionResetHome:<runtime>`).
- **Hub.** Check order per design §3.3.2; new codes
  `harness_config_not_found` (404) and `harness_credentials_missing` (422,
  the no-auth-fallback guard). Drop rules recorded into `CreateInputs`.
  Plan: `harness`, `homeReset`, home-reset warning first.
  `AgentReincarnation.HomeReset` (additive ent column).
- **Worker.** `DispatchAgentReprovision(ctx, agent, ReprovisionOptions)`.
  D-b fail forward once the home may be reset (`reprovisionLeftHomeIntact`
  decides); the sweep does the same for a home-resetting record in
  `provisioning`. Preamble sentence on a reset.
- **CLI.** `--harness`, warning block from `plan.warnings[0]`, no prompt.

## Notes for later phases

- Phase 2 (credential preflight, §3.3.5) is not here: a target that needs
  credentials the hub cannot see is refused only by the broker's gather
  pass, which runs before the reset, so the home stays intact.
- Phase 3/4 add runtimes to `ReprovisionResetHomeRuntimes()` in
  `pkg/runtimebroker/static_capabilities.go` once their NFS leaf is
  cleared; the hub needs no change for that.
- The startup sweep does not reach linked projects outside the global
  directory; their leftovers go with the agent's next reset or delete.
- The template config-dir warning (§3.3.3) only compares the template's
  `home/` manifest against the old harness's built-in config dir.
