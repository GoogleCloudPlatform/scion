# Substrate: exec hardening, IP-literal hub refusal, redaction order, async-create fallback

**Date:** 2026-10-03
**Branch:** scion/substrate-pr3-restack-wip

## What changed

- **Broker startup refusal is scoped.** `runtime.GetRuntime` tags substrate construction failures
  with `runtime.ErrSubstrateProfileInvalid`. `resolveBrokerDefaultRuntime` refuses startup only
  for that sentinel; any other `ErrorRuntime` (for example a missing kubeconfig) starts degraded,
  as before.
- **Substrate exec (sciontool control server):**
  - `execUserCredential` refuses a user resolving to uid 0.
  - `runExec` runs the child in the user's passwd home, and refuses a missing or non-directory
    home.
  - Tests use a host-independent "scion" passwd fixture (`exec_fixture_test.go`), so they run
    on CI hosts without a real scion account.
  - A source guard pins that `runExec` waits only through `procreap.RunManaged`.
  - The timeout test proves the process-group kill reaches a grandchild.
- **Trusted hub host.** `substrate.IsIPLiteralHost` applies the WHATWG URL "ends in a number"
  rule, so inet_aton spellings (`127.1`, `0x7f000001`, `0177.0.0.1`, `2130706433`, ...) are
  refused like dotted quads. Hostnames such as `api2.example.com` or `1.example.com` still pass.
  Tenant egress entries already refused these forms (`looksLikeIPAttempt` plus the public-suffix
  check); rows now prove it.
- **Redact before truncate.** Both `doExec` and `postBootstrap` read up to 64 KiB, redact the
  whole string, then truncate to 4 KiB on a rune boundary. A secret straddling the cut can no
  longer leak its prefix.
- **Async create fallback.** Main's async create depends on `RunConfig.Checkpoint` /
  `OnResourceCreated`, which the substrate runtime does not call.
  - A new optional capability, `runtime.AsyncLaunchUnsupportedRuntime`, is implemented by
    SubstrateRuntime. Runtimes that don't implement it are unaffected.
  - createAgent's existing fallback switch serves such a request synchronously, with a warning.
  - The check uses the runtime resolved for the request, not the broker default.
  - A synchronous 201 is what an async-capable hub already expects from an older broker.

## Gotchas

- `pkg/sciontool/rootexec/guard_test.go` keys its exec-site allowlist by `file:line`. Any edit
  above an allowlisted call site (here `pkg/sciontool/substrate/exec.go`) breaks
  `./pkg/sciontool/rootexec`. Run the whole `./pkg/sciontool/...` suite, not just the package
  you edited.
- The broker advertises `async_launch` broker-wide, but a broker can serve several runtimes via
  per-project profiles. A per-runtime capability has to be checked per request, not at
  advertisement time.

## Possible follow-ups (not done)

- Advertise async-launch support per runtime target (heartbeat inventory) instead of
  broker-wide. The hub would then avoid asking substrate brokers for async at all.
- Implement the launch hooks in SubstrateRuntime (checkpoints around CreateActor/bootstrap,
  `OnResourceCreated` for the actor) so substrate can drop the fallback.
