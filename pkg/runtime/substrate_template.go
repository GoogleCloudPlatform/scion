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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/substratecaps"
	"github.com/GoogleCloudPlatform/scion/pkg/substrateenv"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
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

// substrateTemplateName computes the content-addressed ActorTemplate name
// (substrate-runtime.md §3). Same inputs always produce the same name,
// so concurrent Runs for the same effective template converge on one
// CreateActorTemplate instead of racing to create distinct ones.
//
// The hash covers every input that changes what buildActorTemplate
// produces: image digest, sandbox class, sandbox config name, worker
// selector, snapshot storage location, the *effective* resources (resolved
// against config.BuiltinDefaultResources when resources is nil — hashing
// the nil pointer as "" while buildActorTemplate substitutes a real default
// would let a change to that default silently reuse the old golden
// template), the hardcoded snapshot scope, the container's added
// capabilities, the entrypoint version, and the conditional
// egress_trust_bundle input (appended only when non-empty) — every
// template-content input listed in substrate-runtime.md §3. All of these
// are template content, so changing any of them in settings — or in this
// runtime's own code, for the capability set — must not silently reuse a
// stale golden template.
func substrateTemplateName(imageDigest string, sc config.V1SubstrateConfig, resources *api.ResourceSpec) string {
	effectiveResources := resources
	if effectiveResources == nil {
		effectiveResources = config.BuiltinDefaultResources()
	}

	h := sha256.New()
	// hash.Hash.Write never returns an error (see the hash.Hash doc
	// comment), so the error from Fprintf is deliberately discarded rather
	// than checked.
	_, _ = fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s|%s|%s",
		imageDigest,
		sc.SandboxClass,
		sc.SandboxConfigName,
		workerSelectorCacheKey(sc.WorkerSelector),
		sc.SnapshotStorage,
		resourcesCacheKey(effectiveResources),
		"DATA", // on_pause/on_commit scope, hardcoded
		strings.Join(substrateContainerCapabilitiesAdd, ","),
		substrateServeEntrypointVersion,
	)
	// EgressTrustBundle is appended as its own hash-input segment ONLY when
	// set, rather than always written as a 10th "%s" field (which would be
	// "" on every existing plain install and still change the hash from
	// what it was before this field existed). This is deliberate: an unset
	// EgressTrustBundle must keep producing the exact template name it
	// always has, so existing templates and golden snapshots on plain
	// installs are reused unchanged; setting it must produce a new name,
	// since buildActorTemplate's output genuinely differs (a system-info
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

// resourcesCacheKey renders a *api.ResourceSpec into a stable string for
// substrateTemplateName's hash input.
func resourcesCacheKey(r *api.ResourceSpec) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("req(cpu=%s,mem=%s)limit(cpu=%s,mem=%s)disk=%s",
		r.Requests.CPU, r.Requests.Memory, r.Limits.CPU, r.Limits.Memory, r.Disk)
}

// substrateSandboxClass maps the config string ("gvisor"/"microvm", default
// gvisor) to the proto enum.
func substrateSandboxClass(name string) ateapipb.SandboxClass {
	switch name {
	case "microvm":
		return ateapipb.SandboxClass_SANDBOX_CLASS_MICROVM
	default:
		return ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR
	}
}

// substrateTrustBundleMountPath is the fixed, non-secret mount path for the
// projected trust-bundle system-info volume. Kept as a constant, not derived
// from sc, because the bundle's consumers (buildActorTemplate's own Env
// values below, and every doc/support reference) all need the same literal
// path — see docs/egress-trust-bundle.md (agent-substrate/substrate
// d277088b) and demos/egress/egress-mitm-template.yaml.tmpl, which this
// mirrors exactly.
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
// (substrate-runtime.md §3). Env carries no secrets and no per-agent
// config, ever (substrate-runtime.md §3; that is pushed after the actor starts, via
// POST /scion/v1/bootstrap) — the only Env this function ever sets is the
// fixed set of CA-bundle paths below, and only when
// sc.EgressTrustBundle is non-empty.
func buildActorTemplate(atespace, templateName, imageDigest string, sc config.V1SubstrateConfig, resources *api.ResourceSpec) *ateapipb.ActorTemplate {
	if resources == nil {
		resources = config.BuiltinDefaultResources()
	}

	var limits []*ateapipb.Limits
	if resources.Limits.CPU != "" {
		limits = append(limits, &ateapipb.Limits{Name: "cpu", Quantity: resources.Limits.CPU})
	}
	if resources.Limits.Memory != "" {
		limits = append(limits, &ateapipb.Limits{Name: "memory", Quantity: resources.Limits.Memory})
	}

	// env, volumes, and volumeMounts for the projected egress-gateway trust
	// bundle — added only when sc.EgressTrustBundle is set (opt-in, default
	// off). Off must stay byte-identical to before this field existed: no
	// system-info volume, no /run/ate mount, Env nil.
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
								Name: sc.EgressTrustBundle,
								Path: substrateTrustBundleFileName,
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
		// projected bundle — never per-agent config or a secret, so setting
		// them here does not weaken the "Env carries no secrets" invariant
		// above. The names come from substrateenv.TrustBundleVarNames, the
		// single source of truth shared with pkg/sciontool/substrate's
		// runExec (which passes exactly the subset of these names that are
		// actually set into its exec-invoked commands' own environment
		// explicitly, since that environment is built from scratch rather
		// than inherited) — see that package's doc comment.
		//
		// SSL_CERT_DIR=/run/ate is set deliberately, not left at its
		// default. SSL_CERT_DIR makes the gateway CA exclusive for Go and Python
		// `ssl` (crypto/x509's SystemCertPool and OpenSSL's default-path
		// lookup both replace their default directory list with it); it is
		// additive or ignored for curl, git, and Node — see
		// deploy/substrate/README.md's egress_trust_bundle section for why
		// (Debian's curl/git are built with a compiled-in CApath and never
		// consult SSL_CERT_DIR; Node doesn't read it at all). What it buys:
		// sciontool's own Go client trusts only the gateway CA, so a hub
		// status report succeeding is positive proof for Go, and a path
		// that bypasses the gateway fails closed rather than silently
		// trusting public roots. The cost: if the hub is ever reached
		// without the gateway re-originating the connection, status
		// reports fail TLS.
		trustBundleEnv = make([]*ateapipb.EnvVar, 0, len(substrateenv.TrustBundleVarNames))
		for _, name := range substrateenv.TrustBundleVarNames {
			value := substrateTrustBundleFile
			if name == "SSL_CERT_DIR" {
				value = substrateTrustBundleMountPath
			}
			trustBundleEnv = append(trustBundleEnv, &ateapipb.EnvVar{Name: name, Value: value})
		}
	}

	tmpl := &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: templateName},
		Containers: []*ateapipb.Container{
			{
				Name:  "scion-agent",
				Image: imageDigest,
				// Absolute, never a bare name: this is PID 1's own launch
				// argv, resolved by whatever mechanism starts the
				// container's entrypoint — not something this codebase's
				// own rootexec.Resolve (fixed system directories, verified
				// by fd-walk) has any chance to guard, since it runs before
				// this binary's own code executes at all. A bare
				// "sciontool" would be looked up against the container's
				// PATH, which includes the workload-owned npm-global
				// directory ahead of any trusted system directory — see
				// the rootexec package doc comment for the general shape
				// of that hazard. /usr/local/bin/sciontool is where the
				// image installs it (image-build/scion-base/Dockerfile).
				Command: []string{"/usr/local/bin/sciontool", "substrate-serve"},
				Env:     trustBundleEnv, // no secrets, ever (substrate-runtime.md §3); only fixed CA-bundle paths when egress_trust_bundle is set
				VolumeMounts: append([]*ateapipb.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
				}, trustBundleMounts...),
				// Substrate always starts the actor process as UID 0 / GID 0
				// (ContainerSpec has no user field — agent-substrate/substrate
				// internal/ocispec/ocispec.go) with a minimal default
				// capability set (AUDIT_WRITE, KILL, NET_BIND_SERVICE —
				// cmd/atelet/oci.go) that does not include any of
				// substratecaps.Required. scion never runs the harness or
				// exec as root, and dropping from root to the scion user —
				// via the supervisor's own syscall.Credential drop
				// (pkg/sciontool/supervisor/supervisor.go's Run, ~lines
				// 113-150) and the exec control-plane endpoint's own
				// syscall.Credential drop (pkg/sciontool/substrate's
				// runExec, used for `sciontool substrate-serve exec`) — as
				// well as RunInit's own chowns of the log file and
				// workspace immediately after that drop, all need
				// capabilities this default set doesn't grant. See
				// substratecaps.Required for exactly which ones and the
				// evidence behind each — the same capabilities Docker's
				// default set already grants, which is why this only
				// surfaces on Substrate. These are added here, not assumed
				// from a container default, so they apply inside the gVisor
				// sentry the actor runs in; a direct credential drop grants
				// the dropped-to process none of them, so nothing
				// scion-owned ever runs privileged.
				SecurityContext: &ateapipb.SecurityContext{
					Capabilities: &ateapipb.Capabilities{
						// Clone, not the package-level slice itself: any
						// caller that mutated tmpl...Capabilities.Add in
						// place would otherwise corrupt
						// substrateContainerCapabilitiesAdd for every future
						// template built in this process, silently changing
						// what substrateTemplateName hashes without changing
						// the hash input's own value.
						Add: slices.Clone(substrateContainerCapabilitiesAdd),
					},
				},
				Resources: &ateapipb.Resources{Limits: limits},
			},
		},
		Volumes: append([]*ateapipb.Volume{
			{Name: "workspace", DurableDir: &ateapipb.DurableDirVolumeSource{}},
		}, trustBundleVolumes...),
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			OnCommit:        ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			StorageLocation: sc.SnapshotStorage,
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: substrateSandboxClass(sc.SandboxClass),
			ConfigName:   sc.SandboxConfigName,
		},
	}

	if len(sc.WorkerSelector) > 0 {
		tmpl.WorkerSelector = &ateapipb.Selector{MatchLabels: sc.WorkerSelector}
	}

	return tmpl
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
				runtimeLog.Warn("substrate: failed to clean up a failed golden-snapshot template",
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
