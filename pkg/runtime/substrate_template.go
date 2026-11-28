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

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/substratecaps"
	"github.com/GoogleCloudPlatform/scion/pkg/substrateenv"
	ateapipb "github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// substrateServeEntrypointVersion is folded into the template's
// content-address (substrate-runtime.md §3) so a change to the
// `sciontool substrate-serve` protocol forces a new template rather than
// silently reusing a golden snapshot built against the old one. Bumped to
// v2 for the absolute-Command hardening below: Command itself is not part
// of the hash input list (it's a Go literal, not a value derived from
// config that could otherwise vary), so this version bump is the only
// thing that forces existing golden snapshots — built with the old, bare
// "sciontool" entrypoint baked in — to be rebuilt rather than reused as-is.
const substrateServeEntrypointVersion = "substrate-serve/v2"

// substrateServePort is the TCP port substrate-serve listens on inside the
// actor: the --addr flag's default (":80") in
// cmd/sciontool/commands/substrate_serve.go. buildActorTemplate's Command
// passes no --addr, so the control server always binds this port, which is
// also the port the inbound router targets by default.
const substrateServePort = 80

// substrateWakeupProbeTimeoutSeconds is how long Substrate keeps polling the
// wakeup probe before it fails the wakeup and moves the actor to
// ACTOR_STATE_CRASHED. Substrate's own default is 30s. 120s is deliberately
// conservative:
//
//   - On a cold boot (the golden actor, or any actor started without a
//     snapshot), substrate-serve runs the rootfs fixup and the self-binary
//     integrity check synchronously before it starts listening (see
//     runSubstrateServe). On a slow or busy worker that, plus process start,
//     can approach 30s, and a false failure there fails the whole template.
//   - On a restore, the listener is part of the checkpointed memory, so the
//     first poll normally answers and this bound is never approached.
//   - A broken image (for example a sciontool without substrate-serve, which
//     exits at once) still fails within two minutes: inside
//     defaultTemplateReadyTimeout (10m) for the golden actor, and inside
//     defaultHealthzTimeout (5m), the client-side wait that runs after
//     ResumeActor returns.
const substrateWakeupProbeTimeoutSeconds = 120

// substrateWakeupProbe returns the wakeup probe for the agent container: GET
// substrateHealthzPath on substrateServePort. healthz needs no auth and
// always answers 200 once the control server is up, whatever lifecycle state
// it reports (pkg/sciontool/substrate's HealthzResponse), so the probe
// passes on both a fresh golden boot ("awaiting-bootstrap") and a resumed,
// bootstrapped actor ("running" or "init-failed"). It gates each wakeup on
// substrate-serve actually serving: the probe is polled on every wakeup,
// including a cold boot, and a miss within the timeout crashes the actor
// (ateapi.proto, WakeupProbe). The golden actor's cold boot is
// gated the same way, so an image whose entrypoint exits fails template
// creation rather than producing a golden that cannot be restored.
// Substrate's API guide further states that when every container declares
// a probe, the golden snapshot is taken once the probe has answered,
// instead of after a fixed warm-up delay.
//
// A fresh value on every call, so no caller can mutate a shared probe and
// change what substrateTemplateName hashes without changing its input.
func substrateWakeupProbe() *ateapipb.WakeupProbe {
	return &ateapipb.WakeupProbe{
		HttpGet: &ateapipb.HttpGetAction{
			Path: substrateHealthzPath,
			Port: substrateServePort,
		},
		TimeoutSeconds: substrateWakeupProbeTimeoutSeconds,
	}
}

// substrateWakeupProbeCacheKey renders the wakeup probe into a stable string
// for substrateTemplateName's hash input, from the same value
// buildActorTemplate sets, so a change to the probe's path, port or timeout
// always produces a new template name.
func substrateWakeupProbeCacheKey() string {
	p := substrateWakeupProbe()
	return fmt.Sprintf("wakeup(path=%s,port=%d,timeout=%d)",
		p.GetHttpGet().GetPath(), p.GetHttpGet().GetPort(), p.GetTimeoutSeconds())
}

// substrateContainerCapabilitiesAdd lists the Linux capabilities
// buildActorTemplate grants on top of Substrate's default set. It is
// substratecaps.Names() — the single source of truth shared with
// checkPrivilegeDropFeasible (cmd/sciontool/commands), so the template and
// the /bootstrap precondition that verifies it can never drift apart — see
// package substratecaps's doc comment for why a leaf package, and
// substratecaps.Required for what each capability is for and the evidence
// behind it. This is also the hash input for substrateTemplateName
// (below), so any future change here also changes the template's
// content-address, forcing a new golden template instead of silently
// reusing one built without the capabilities a code change just added or
// removed.
var substrateContainerCapabilitiesAdd = substratecaps.Names()

// defaultTemplateReadyTimeout bounds how long Run waits for a newly created
// ActorTemplate's golden snapshot to become ready, when
// V1SubstrateConfig.TemplateReadyTimeout is unset.
const defaultTemplateReadyTimeout = 10 * time.Minute

// templateReadyPollInterval is how often Run re-polls GetActorTemplate
// while waiting for the golden snapshot.
const templateReadyPollInterval = 5 * time.Second

// substrateDefaultMemoryLimit is the actor memory limit used when neither
// resources.limits.memory nor resources.requests.memory is set. Substrate v1
// requires both a CPU and a memory limit on every ActorTemplate.
const substrateDefaultMemoryLimit = "4Gi"

// substrateDefaultCPULimit is the actor CPU limit used when neither
// resources.limits.cpu nor resources.requests.cpu is set (matching
// config.BuiltinDefaultResources).
const substrateDefaultCPULimit = "2"

// substrateTemplateName computes the content-addressed ActorTemplate name
// (substrate-runtime.md §3). Same inputs always produce the same name,
// so concurrent Runs for the same effective template converge on one
// CreateActorTemplate instead of racing to create distinct ones.
//
// The hash covers every input that changes what buildActorTemplate
// produces: image digest, worker selector, snapshot storage location, the
// *effective* resource limits (resolved the same way buildActorTemplate
// resolves them, so a change to a default never silently reuses an old
// golden template), the snapshot fidelity and golden-snapshot mode, the
// container's added capabilities, the entrypoint version, the container's
// wakeup probe, and the conditional egress_trust_bundle input (appended
// only when non-empty) — every template-content input listed in
// substrate-runtime.md §3. All of these are template content, so changing
// any of them in settings — or in this runtime's own code — must not
// silently reuse a stale golden template.
func substrateTemplateName(imageDigest string, sc config.V1SubstrateConfig, resources *api.ResourceSpec) string {
	h := sha256.New()
	// hash.Hash.Write never returns an error (see the hash.Hash doc
	// comment), so the error from Fprintf is deliberately discarded rather
	// than checked.
	_, _ = fmt.Fprintf(h, "v1|%s|%s|%s|%s|%s|%s|%s|%s|%s",
		imageDigest,
		workerSelectorCacheKey(sc.WorkerSelector),
		sc.SnapshotStorage,
		substrateLimitsCacheKey(resources),
		ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED,
		strings.Join(substrateContainerCapabilitiesAdd, ","),
		substrateServeEntrypointVersion,
		substrateWakeupProbeCacheKey(),
	)
	// EgressTrustBundle is appended as its own hash-input segment ONLY when
	// set: buildActorTemplate's output genuinely differs (a system-info
	// volume, a mount, and five Env vars that were not there before).
	if sc.EgressTrustBundle != "" {
		_, _ = fmt.Fprintf(h, "|%s", sc.EgressTrustBundle)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return "scion-" + sum[:12]
}

// workerSelectorCacheKey renders a worker-selector match-labels map into a
// stable string for substrateTemplateName's hash input: sorted by key, so
// map iteration order never changes the hash.
func workerSelectorCacheKey(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
	}
	return b.String()
}

// substrateLimitsCacheKey renders the effective template limits for
// substrateTemplateName's hash input. An unparsable quantity hashes as its
// raw text; buildActorTemplate rejects it before any template is created.
func substrateLimitsCacheKey(resources *api.ResourceSpec) string {
	limits, err := substrateTemplateLimits(resources)
	if err != nil {
		cpu, mem := substrateLimitQuantities(resources)
		return fmt.Sprintf("limits(cpu=%s,mem=%s)", cpu, mem)
	}
	return fmt.Sprintf("limits(cpu_m=%d,mem_b=%d)", limits.GetCpuMillicores(), limits.GetMemoryBytes())
}

// substrateLimitQuantities picks the CPU and memory quantity strings a
// template is sized by: the limit when set, else the request, else the
// substrate default.
func substrateLimitQuantities(resources *api.ResourceSpec) (cpu, mem string) {
	if resources != nil {
		cpu, mem = resources.Limits.CPU, resources.Limits.Memory
		if cpu == "" {
			cpu = resources.Requests.CPU
		}
		if mem == "" {
			mem = resources.Requests.Memory
		}
	}
	if cpu == "" {
		cpu = substrateDefaultCPULimit
	}
	if mem == "" {
		mem = substrateDefaultMemoryLimit
	}
	return cpu, mem
}

// substrateTemplateLimits converts resources into Substrate v1's required
// template limits (cpu_millicores and memory_bytes, both > 0).
func substrateTemplateLimits(resources *api.ResourceSpec) (*ateapipb.Resources, error) {
	cpuText, memText := substrateLimitQuantities(resources)
	cpu, err := resource.ParseQuantity(cpuText)
	if err != nil {
		return nil, fmt.Errorf("substrate: invalid CPU quantity %q: %w", cpuText, err)
	}
	mem, err := resource.ParseQuantity(memText)
	if err != nil {
		return nil, fmt.Errorf("substrate: invalid memory quantity %q: %w", memText, err)
	}
	cpuMilli, memBytes := cpu.MilliValue(), mem.Value()
	if cpuMilli <= 0 || memBytes <= 0 {
		return nil, fmt.Errorf("substrate: actor CPU (%q) and memory (%q) limits must both be greater than zero", cpuText, memText)
	}
	if cpuMilli > 999999 {
		return nil, fmt.Errorf("substrate: actor CPU limit %q exceeds Substrate's maximum of 999999 millicores", cpuText)
	}
	return &ateapipb.Resources{CpuMillicores: cpuMilli, MemoryBytes: memBytes}, nil
}

// substrateTrustBundleMountPath is the fixed, non-secret mount path for the
// projected trust-bundle system-info volume. Kept as a constant, not derived
// from sc, because the bundle's consumers (buildActorTemplate's own Env
// values below, and every doc/support reference) all need the same literal
// path.
const substrateTrustBundleMountPath = "/run/ate"

// substrateTrustBundleFileName is the projected file's name within the
// system-info volume (SystemInfoDataSource.TrustBundle.Path is relative to
// the volume root), so its absolute path is
// substrateTrustBundleMountPath + "/" + substrateTrustBundleFileName.
const substrateTrustBundleFileName = "trust-bundle.pem"

// substrateTrustBundleFile is the bundle's full absolute path once mounted,
// used for every CA-related env var below.
const substrateTrustBundleFile = substrateTrustBundleMountPath + "/" + substrateTrustBundleFileName

// buildActorTemplate constructs the ActorTemplate for CreateActorTemplate
// (substrate-runtime.md §3), shaped for Substrate v1: template-level
// resource limits (required), MEMORY snapshot fidelity with golden
// snapshots enabled, and no sandbox config (the WorkerPool selected by
// worker_selector supplies the sandbox class).
//
// Env carries no secrets and no per-agent config, ever (substrate-runtime.md
// §3; that is pushed after the actor starts, via POST /scion/v1/bootstrap) —
// the only Env this function ever sets is the fixed set of CA-bundle paths
// below, and only when sc.EgressTrustBundle is non-empty.
func buildActorTemplate(atespace, templateName, imageDigest string, sc config.V1SubstrateConfig, resources *api.ResourceSpec) (*ateapipb.ActorTemplate, error) {
	limits, err := substrateTemplateLimits(resources)
	if err != nil {
		return nil, err
	}

	// env, volumes, and volumeMounts for the projected egress-gateway trust
	// bundle — added only when sc.EgressTrustBundle is set (opt-in, default
	// off). Off adds no system-info volume, no /run/ate mount and no Env.
	var trustBundleEnv []*ateapipb.EnvVar
	var trustBundleVolumes []*ateapipb.Volume
	var trustBundleMounts []*ateapipb.VolumeMount
	if sc.EgressTrustBundle != "" {
		trustBundleVolumes = []*ateapipb.Volume{
			{
				Name: "system-info",
				SystemInfo: &ateapipb.SystemInfoVolumeSource{
					DataSources: []*ateapipb.SystemInfoDataSource{
						{
							TrustBundle: &ateapipb.TrustBundleDataSource{
								Names: []string{sc.EgressTrustBundle},
								Path:  substrateTrustBundleFileName,
							},
						},
					},
				},
			},
		}
		trustBundleMounts = []*ateapipb.VolumeMount{
			{Name: "system-info", MountPath: substrateTrustBundleMountPath},
		}
		// NODE_EXTRA_CA_CERTS, GIT_SSL_CAINFO, SSL_CERT_FILE, CURL_CA_BUNDLE,
		// and SSL_CERT_DIR are all fixed, non-secret paths into the
		// projected bundle — never per-agent config or a secret. The names
		// come from substrateenv.TrustBundleVarNames, the single source of
		// truth shared with pkg/sciontool/substrate's runExec.
		//
		// SSL_CERT_DIR=/run/ate is set deliberately: it makes the gateway CA
		// exclusive for Go and Python `ssl`, so a path that bypasses the
		// gateway fails closed rather than silently trusting public roots
		// (see deploy/substrate/README.md's egress_trust_bundle section).
		trustBundleEnv = make([]*ateapipb.EnvVar, 0, len(substrateenv.TrustBundleVarNames))
		for _, name := range substrateenv.TrustBundleVarNames {
			value := substrateTrustBundleFile
			if name == "SSL_CERT_DIR" {
				value = substrateTrustBundleMountPath
			}
			trustBundleEnv = append(trustBundleEnv, &ateapipb.EnvVar{Name: name, Value: value})
		}
	}

	snapshotConfig := &ateapipb.SnapshotConfig{
		PreferredFidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		GoldenSnapshotConfig: &ateapipb.GoldenSnapshotConfig{
			Mode: ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED,
		},
	}
	if sc.SnapshotStorage != "" {
		// Accepted by Substrate only with its PerTemplateSnapshotLocation
		// preview gate; unset uses the ate-api server's --snapshot-location.
		snapshotConfig.StorageLocation = sc.SnapshotStorage
	}

	tmpl := &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: templateName},
		Containers: []*ateapipb.Container{
			{
				Name:  "scion-agent",
				Image: imageDigest,
				// Absolute, never a bare name: this is PID 1's own launch
				// argv, resolved before this binary's own code executes at
				// all. A bare "sciontool" would be looked up against the
				// container's PATH, which includes the workload-owned
				// npm-global directory ahead of any trusted system directory
				// — see the rootexec package doc comment.
				// /usr/local/bin/sciontool is where the image installs it
				// (image-build/scion-base/Dockerfile).
				Command: []string{"/usr/local/bin/sciontool", "substrate-serve"},
				Env:     trustBundleEnv, // no secrets, ever (substrate-runtime.md §3); only fixed CA-bundle paths when egress_trust_bundle is set
				VolumeMounts: append([]*ateapipb.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
				}, trustBundleMounts...),
				// Substrate always starts the actor process as UID 0 / GID 0
				// with a minimal default capability set (AUDIT_WRITE, KILL,
				// NET_BIND_SERVICE) that does not include any of
				// substratecaps.Required. scion never runs the harness or
				// exec as root, and dropping from root to the scion user —
				// via the supervisor's and the exec endpoint's own
				// syscall.Credential drops — as well as RunInit's chowns
				// right after that drop, all need capabilities this default
				// set doesn't grant. See substratecaps.Required for exactly
				// which ones and why; all four are inside Substrate v1's
				// capability allowlist. A direct credential drop grants the
				// dropped-to process none of them, so nothing scion-owned
				// ever runs privileged.
				SecurityContext: &ateapipb.SecurityContext{
					Capabilities: &ateapipb.Capabilities{
						// Clone, not the package-level slice itself: any
						// caller that mutated tmpl...Capabilities.Add in
						// place would otherwise corrupt
						// substrateContainerCapabilitiesAdd for every future
						// template built in this process.
						Add: slices.Clone(substrateContainerCapabilitiesAdd),
					},
				},
				// Gates every wakeup, including the golden actor's first
				// boot, on substrate-serve answering healthz. See
				// substrateWakeupProbe.
				WakeupProbe: substrateWakeupProbe(),
			},
		},
		Volumes: append([]*ateapipb.Volume{
			{Name: "workspace", DurableDir: &ateapipb.DurableDirVolumeSource{}},
		}, trustBundleVolumes...),
		SnapshotConfig: snapshotConfig,
		Resources:      &ateapipb.ResourceRequirements{Limits: limits},
	}

	if len(sc.WorkerSelector) > 0 {
		tmpl.WorkerSelector = &ateapipb.Selector{MatchLabels: sc.WorkerSelector}
	}

	return tmpl, nil
}

// templateReadyTimeout resolves V1SubstrateConfig.TemplateReadyTimeout,
// falling back to defaultTemplateReadyTimeout when unset or unparsable.
func templateReadyTimeout(sc config.V1SubstrateConfig) time.Duration {
	if sc.TemplateReadyTimeout == "" {
		return defaultTemplateReadyTimeout
	}
	d, err := time.ParseDuration(sc.TemplateReadyTimeout)
	if err != nil || d <= 0 {
		return defaultTemplateReadyTimeout
	}
	return d
}

// ensureActorTemplate gets the ActorTemplate named templateName in atespace,
// creating it if missing, then waits for its golden snapshot to become
// ready — creating a template boots a golden actor and snapshots it
// (substrate-runtime.md §3), so Run must not proceed to CreateActor until
// that snapshot is ready. clock lets tests replace time.Sleep with an
// instant no-op.
func ensureActorTemplate(ctx context.Context, client ateapipb.ControlClient, atespace, templateName string, tmpl *ateapipb.ActorTemplate, timeout time.Duration, sleep func(time.Duration)) error {
	existing, err := client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: templateName},
	})
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return fmt.Errorf("substrate: get actor template %s/%s: %w", atespace, templateName, err)
		}
		created, err := client.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{ActorTemplate: tmpl})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("substrate: create actor template %s/%s: %w", atespace, templateName, err)
		}
		existing = created
	}

	if templateIsReady(existing) {
		return nil
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("substrate: actor template %s/%s did not become ready within %s", atespace, templateName, timeout)
		}
		sleep(templateReadyPollInterval)

		existing, err = client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: templateName},
		})
		if err != nil {
			return fmt.Errorf("substrate: poll actor template %s/%s: %w", atespace, templateName, err)
		}
		if templateIsReady(existing) {
			return nil
		}
		if msg := templateErrorMessage(existing); msg != "" {
			// templateName is deterministic (substrateTemplateName hashes
			// the image/config/resources shape), so leaving the failed
			// template in place would make every future Run for this same
			// shape find the SAME broken template via GetActorTemplate
			// above and never attempt CreateActorTemplate again — sticky
			// until an operator manually intervenes. Best-effort cleanup:
			// a failed delete here doesn't change the outcome (Run already
			// fails either way), and the next Run's own GetActorTemplate
			// will simply find the same broken template again and retry
			// this same cleanup.
			if _, delErr := client.DeleteActorTemplate(ctx, &ateapipb.DeleteActorTemplateRequest{
				ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: templateName},
			}); delErr != nil && status.Code(delErr) != codes.NotFound {
				runtimeLog().Warn("substrate: failed to clean up a failed golden-snapshot template",
					"atespace", atespace, "template", templateName, "error", delErr)
			}
			return fmt.Errorf("substrate: actor template %s/%s golden snapshot failed: %s", atespace, templateName, msg)
		}
	}
}

// templateIsReady reports whether tmpl's golden snapshot has been taken
// (GoldenSnapshotStatus.golden_tag is set).
func templateIsReady(tmpl *ateapipb.ActorTemplate) bool {
	return tmpl != nil &&
		tmpl.GetStatus() != nil &&
		tmpl.GetStatus().GetGoldenSnapshotStatus() != nil &&
		tmpl.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag() != nil &&
		tmpl.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag().GetName() != ""
}

// templateErrorMessage returns the golden-snapshot error message, if any.
func templateErrorMessage(tmpl *ateapipb.ActorTemplate) string {
	if tmpl == nil || tmpl.GetStatus() == nil || tmpl.GetStatus().GetGoldenSnapshotStatus() == nil {
		return ""
	}
	return tmpl.GetStatus().GetGoldenSnapshotStatus().GetErrorMessage()
}
