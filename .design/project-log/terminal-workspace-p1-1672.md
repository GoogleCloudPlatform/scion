# Terminal workspace P1: agent SSE authorization (#1672)

Frozen parent: `1c58e70dfa79a634b270ac1939023cbe70229920` (#1671).
The manager authorized this separate leaf on the frozen ordering candidate while
independent review is pending. No integration is performed by this developer.

The web-session SSE endpoint previously authorized project and user subjects but
passed through agent subjects. An unrelated authenticated user could subscribe
to an agent's status, creation/deletion, ports, and message events without the
agent's metadata read permission. The isolated regression reproduced HTTP 200,
flushed headers, and one publisher subscription for an unauthorized agent.

Agent subjects now require a concrete canonical stored UUID plus an event
selector. Each unique ID is resolved with the configured store's `GetAgent` and
checked with `CheckAccess(ctx, identity, agentResource(agent), ActionRead)`.
This matches `Server.getAgent` through `Server.authorize`, including the stored
owner, project parent, labels and ancestry, and the existing authorization
kernel's role bindings, relationship grants and restrictions. Session role
strings and PTY attach permission are not alternate read grants. Agent
visibility does not add a bypass absent from the metadata resource policy.

Bare, wildcard, alias, and noncanonical resource selectors are denied, as are
missing agents, nil/mismatched lookup results, unavailable stores and failed
lookup/policy decisions. A permitted concrete agent still supports wildcard
event suffixes. Administrators use the same established policy and must also
select concrete existing agents. The caller remains the web-session user.

The existing whole-batch 403 response and denied-subject list are preserved;
there is no partial subscription. Invalid/denied requests subscribe to nothing.
The client must remove definitively inaccessible agents from a batch before
resubscribing its accessible neighbors. Authorization remains a connection-time
check; live revocation and other subject-family policies are outside this leaf.
#1671's subscription-before-flush order remains unchanged.

Tests use isolated stores and the real in-memory publisher, with no live Hub,
active agents, timing sleeps, or polling. They cover metadata-policy parity,
owner/ancestor access, project inheritance and cross-project denial, system
read and super-admin grants, unprivileged role strings, exact/wildcard event
suffixes, invalid/missing selectors, lookup/policy errors, and real handler
403/no-subscription behavior for unauthorized and mixed batches.

Verification: the executable unauthorized-handler regression failed before the
repair (HTTP 200 and one subscription). The repaired focused SSE/authorization
suite and its race run passed. `make ci` passed formatting, vet, custom checks,
repository tests with `no_sqlite`, and the build. Scoped `golangci-lint` against
the frozen parent passed with zero issues; focused gofmt and diff checks passed.
All test subprocesses removed inherited `SCION_*` and used
`GOFLAGS=-buildvcs=false`. No baseline failures remain. Cumulative `make ci-full`
is reserved for manager/writer and was not run for this backend leaf.

Verification and durable handoff details are recorded in
`/scion-volumes/scratchpad/projects/terminal-workspace/reports/p1-1672-developer.md`.
The separate shared bundle requires the frozen #1671 commit. Both leaves still
require independent exact-head review and sole-writer integration.
