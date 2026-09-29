// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package substratecaps is the single source of truth for the Linux
// capabilities a Substrate actor's container must be granted, beyond
// Substrate's own minimal default set (AUDIT_WRITE, KILL,
// NET_BIND_SERVICE), for substrate-serve's privilege drop to actually
// succeed.
//
// Two independent consumers must never drift apart:
//   - pkg/runtime's buildActorTemplate grants exactly this set on the
//     actor's container SecurityContext;
//   - cmd/sciontool/commands's checkPrivilegeDropFeasible (substrate-
//     serve's synchronous /bootstrap precondition) verifies every one of
//     them is effective before /bootstrap ever responds 200.
//
// A capability present in one list but not the other is exactly the
// defect class this package exists to prevent: a template missing a
// capability the checker doesn't know to ask for would pass the
// precondition and then fail deep inside RunInit instead (observed live —
// see Required's CHOWN entry). This package has no
// dependencies beyond the standard library, so both pkg/runtime (broker-
// side, heavy k8s/grpc dependencies) and cmd/sciontool/commands (the
// agent-side sciontool binary, which must stay light and must never
// import pkg/runtime) can import it without pulling in the other's
// dependencies or creating an import cycle.
package substratecaps

// Capability names a single required Linux capability.
type Capability struct {
	// Name is the capability name the way Substrate's
	// ContainerSpec.Capabilities.Add expects it: no CAP_ prefix (see the
	// vendored third_party/ateapipb types), e.g. "SETUID".
	Name string

	// EffBit is Name's bit position in /proc/self/status's CapEff field —
	// the standard Linux capability number (e.g. CAP_SETUID = 7) — used to
	// verify the capability is actually held.
	EffBit uint

	// Why documents, with a citation, the specific evidence that this
	// capability is required. Kept on the struct (rather than only in a
	// comment) so it can be surfaced in logs/documentation generated from
	// this list, and so adding a capability without a reason is visibly
	// awkward.
	Why string
}

// Required lists every capability substrate-serve's privilege drop needs.
// Order matters only for readability; nothing depends on it.
var Required = []Capability{
	{
		Name:   "SETUID",
		EffBit: 7,
		Why:    "su (via execAsUserCmd, used by `sciontool substrate-serve exec`) and the supervisor's own syscall.Credential drop (pkg/sciontool/supervisor's Run, which execs the harness with a Uid/Gid-bearing Credential) both need CAP_SETUID to leave root.",
	},
	{
		Name:   "SETGID",
		EffBit: 6,
		Why:    "su and the supervisor's own syscall.Credential drop also both need CAP_SETGID (setgroups(2)/setgid(2)) to leave root, for the same two consumers as SETUID.",
	},
	{
		Name:   "CHOWN",
		EffBit: 0,
		Why:    `Observed live: "Failed to chown log file: chown /home/scion/agent.log: operation not permitted" and "Failed to chown workspace to UID=1000 GID=1000: chown /workspace: operation not permitted", followed by "Git clone failed: git init failed" and init exiting 1. RunInit unconditionally chowns the log file (log.Chown) and the workspace/home tree (chownTreeRootOwned/ensureWorkspaceOwnership) from root to the scion user once a drop is expected, and chown(2) to an arbitrary uid/gid needs CAP_CHOWN once a process's capability set is restricted — the same "still UID 0 isn't still all-powerful" property that made SETUID/SETGID necessary for su in the first place.`,
	},
	{
		Name:   "DAC_OVERRIDE",
		EffBit: 1,
		Why:    `Observed live: healthz reports init-failed and the init log stops right after "setupHostUser result", with no further lines. Root cause: the rootfs fixup chowns $HOME to the scion user (mode 0700) before RunInit's own root-phase writes into it (stagedsecrets.Write, then agent-info.json, hooks, and reportInitFailure's own local write) — without CAP_DAC_OVERRIDE, root is subject to the same permission check as any other non-owning uid against a 0700 directory it doesn't own, so every one of those writes fails. A second, independent defect hid the first: log.Chown hands the log file (mode 0644) to the scion user immediately before this, so root can no longer append to it either once the drop is underway, silently dropping every later log line rather than reporting the real failure. DAC_OVERRIDE fixes both, without changing either chown.`,
	},
}

// Names returns the plain capability names, in Required's order — what
// buildActorTemplate passes as ContainerSpec.Capabilities.Add.
func Names() []string {
	names := make([]string, len(Required))
	for i, c := range Required {
		names[i] = c.Name
	}
	return names
}
