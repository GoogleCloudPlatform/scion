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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	substrateserve "github.com/GoogleCloudPlatform/scion/pkg/sciontool/substrate"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Substrate control server routes (substrate-runtime.md §5.1), reachable through
// the router with the ate-target-actor header.
const (
	substrateHealthzPath   = "/scion/v1/healthz"
	substrateBootstrapPath = "/scion/v1/bootstrap"
	substrateExecPath      = "/scion/v1/exec"
)

// Per-request deadlines for router calls. RouterClient itself carries no
// blanket timeout (see its doc comment), so every call here sets its own —
// a single fixed client timeout can't fit both a quick healthz probe and an
// exec whose timeout_s is caller-chosen and can legitimately run longer
// than that.
const (
	// healthzRequestTimeout bounds one GET /healthz attempt. waitForHealthz
	// retries across its own outer timeout/backoff, so this only bounds a
	// single attempt.
	healthzRequestTimeout = 10 * time.Second
	// bootstrapRequestTimeout bounds the one-shot POST /bootstrap call.
	bootstrapRequestTimeout = 30 * time.Second
	// execTimeoutSlack is added on top of the caller-requested exec
	// timeout_s to get doExec's own context deadline, so the HTTP round
	// trip has room for the server's own timeout_s enforcement plus
	// network/processing overhead, instead of racing it.
	execTimeoutSlack = 10 * time.Second
)

// healthzAwaitingBootstrap and healthzRunning are the two states substrate-serve's
// GET /scion/v1/healthz reports (substrate-runtime.md §5.1).
const (
	healthzAwaitingBootstrap = "awaiting-bootstrap"
	healthzRunning           = "running"
)

// bootstrapFile is one entry of the bootstrap payload's "files" array.
// mode is a plain decimal file mode (e.g. 384 == 0600, substrate-runtime.md
// §5.3) — not octal text — so it round-trips through JSON as a number.
type bootstrapFile struct {
	Path       string `json:"path"`
	Mode       int    `json:"mode"`
	ContentB64 string `json:"content_b64"`

	// decodedSize is the exact number of raw (pre-base64) bytes ContentB64
	// carries. It is set once, when a bootstrapFile is built from a real
	// []byte payload (never recomputed by decoding ContentB64 back), used
	// only by buildBootstrapFiles' size-cap check, and — being unexported —
	// is never part of the JSON wire format encoding/json produces for this
	// type.
	decodedSize int64
}

// bootstrapRequest is the POST /scion/v1/bootstrap body (substrate-runtime.md §5.3).
type bootstrapRequest struct {
	Env          map[string]string `json:"env"`
	Files        []bootstrapFile   `json:"files"`
	StartCmd     string            `json:"start_cmd"`
	ControlToken string            `json:"control_token"`
}

// healthzResponse is the GET /scion/v1/healthz body.
type healthzResponse struct {
	State string `json:"state"`
}

// execRequest is the POST /scion/v1/exec body.
type execRequest struct {
	Argv     []string `json:"argv"`
	User     string   `json:"user"`
	TimeoutS int      `json:"timeout_s"`
	// Stdin, when non-empty, is piped to the command's standard input
	// instead of being embedded in Argv, so a secret (e.g. a reset-auth
	// token) never appears in the exec'd command's argv — readable via
	// /proc/<pid>/cmdline for the process's lifetime — on the control
	// server host. encoding/json marshals a []byte field as base64
	// automatically. See doExec and maxExecStdinBytes for the size cap.
	Stdin []byte `json:"stdin,omitempty"`
}

// execResponse is the POST /scion/v1/exec body. Truncated flags that stdout
// or stderr hit the 4 MiB per-stream cap (substrate-runtime.md §5.1); the field
// name is provisional pending confirmation of the exact wire-format
// contract.
type execResponse struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
	// StdinSupported is true on a response from a control server new enough
	// to understand execRequest.Stdin. doExec treats a response missing
	// this, when Stdin was sent, as a hard error: an older control server
	// would otherwise silently ignore Stdin and still return exit 0 for a
	// command that read nothing, which must never look like success.
	StdinSupported bool `json:"stdin_supported,omitempty"`
}

// defaultFileMode is applied to every auth and file-type-secret bootstrap
// file. Neither api.FileMapping nor api.ResolvedSecret carries a mode, so a
// single conservative, owner-only mode is used for those — matching the
// substrate-runtime.md §5.3 example (384 decimal == 0600 octal). Home files
// instead keep their source permission bits (§5.4).
const defaultFileMode = 0o600

// buildBootstrapEnv assembles the full agent env for the bootstrap payload.
//
// substrate-runtime.md §5.3 documents the full env, harness terms included:
// cfg.Harness.GetEnv() (when a harness is set) and GetTelemetryEnv() (also
// when telemetry is enabled), which every other runtime includes (see
// buildCommonRunArgs, KubernetesRuntime.buildPod) and which the harness
// needs to run at all (model, task and telemetry env are not otherwise
// present in cfg.Env), plus cfg.Env + ResolvedAuth.EnvVars + env-type
// ResolvedSecrets.
func buildBootstrapEnv(cfg RunConfig) map[string]string {
	env := make(map[string]string)

	if cfg.Harness != nil {
		for k, v := range cfg.Harness.GetEnv(cfg.Name, util.GetHomeDir(cfg.UnixUsername), cfg.UnixUsername) {
			if v != "" {
				env[k] = v
			}
		}
		if cfg.TelemetryEnabled {
			for k, v := range cfg.Harness.GetTelemetryEnv() {
				env[k] = v
			}
		}
	}

	for _, e := range cfg.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			env[k] = v
		}
	}

	if cfg.ResolvedAuth != nil {
		for k, v := range cfg.ResolvedAuth.EnvVars {
			env[k] = v
		}
	}

	for _, s := range cfg.ResolvedSecrets {
		if s.Type == "environment" || s.Type == "" {
			env[s.Target] = s.Value
		}
	}

	if credPath := findGCPTelemetryCredentialPath(cfg.ResolvedSecrets, util.GetHomeDir(cfg.UnixUsername)); credPath != "" {
		env[telemetryGCPCredentialsEnvVar] = credPath
	}

	// SCION_RUNTIME=substrate lets sciontool disable autoexpose/port-forward
	// (blocked by Substrate's default-deny, no-WebSocket-egress posture —
	// substrate-runtime.md §1) so it does not spin retrying a tunnel that can
	// never connect.
	env["SCION_RUNTIME"] = "substrate"

	// SCION_HOST_UID/GID: every other runtime sets these (buildCommonRunArgs,
	// KubernetesRuntime.buildPod) so `sciontool init`'s setupHostUser can
	// drop the actor's init/harness/tmux process from root to the "scion"
	// user before the supervisor launches the harness (pkg/sciontool/
	// supervisor: it only attempts the drop when UID/GID are both > 0).
	// Without them, setupHostUser's "SCION_HOST_UID/GID not set" branch
	// leaves the whole process tree at UID 0 — the harness and tmux would
	// run as root even with the container's own SETUID/SETGID capabilities
	// in place, since the drop is never attempted in the first place.
	//
	// Unlike Docker/Podman (where these normally mirror the broker host's
	// own UID/GID for bind-mount permission parity) or the NFS backend
	// (where they're a stable, node-independent identity for a shared
	// filesystem), Substrate's workspace is never bind-mounted from the
	// invoking broker's own filesystem at all — there is no host UID to
	// synchronize with. The stable default (1000:1000, matching the actor
	// image's built-in "scion" user, the same fallback the NFS backend uses
	// elsewhere) is the only value that makes sense here, unconditionally.
	env["SCION_HOST_UID"] = "1000"
	env["SCION_HOST_GID"] = "1000"

	// SCION_KEEPID_UID (the Podman keep-id remap signal, set by
	// PodmanRuntime elsewhere) has no meaning on Substrate and must not
	// survive into the bootstrap env if cfg.Env happens to carry it
	// forward from a template shared with another runtime:
	// `sciontool init`'s keep-id branch treats its presence as "the
	// container runtime already remapped this uid", which would skip the
	// very drop SCION_HOST_UID/GID above exists to trigger.
	delete(env, "SCION_KEEPID_UID")

	return env
}

// substrateSecretCandidates returns every value from cfg that must never
// appear in an error string, keyed by the name/target that identifies it in
// a redaction marker: harness env/telemetry env, cfg.Env, ResolvedAuth's
// env vars, and both env-type and file-type ResolvedSecrets.
//
// This is deliberately its own function, not argv_redact.go's
// externalEnvValues (written for cloudrun-sandbox's argv construction) and
// not buildBootstrapEnv's output either:
//
//   - externalEnvValues covers cfg.Env, Harness.GetEnv() and
//     ResolvedAuth.EnvVars, and cross-checks against a final env map — it
//     reads ResolvedSecrets only once a runtime has folded them into
//     cfg.Env, so it must not be treated as a substitute for this function.
//   - buildBootstrapEnv's output isn't reusable as-is either: it adds
//     SCION_RUNTIME=substrate, a runtime-synthesised constant that is also
//     a substring of every one of this runtime's own error-message
//     prefixes ("substrate: ..."). Feeding that into redactEnvValues (a
//     blunt substring replace over the whole error text) would rewrite
//     "substrate" everywhere it appears, corrupting unrelated error
//     messages. This function must mirror every broker-placed secret source
//     in cfg itself, rather than importing another function's whole output —
//     importing buildBootstrapEnv's output, or any future function that
//     gains its own synthesised constant, reaches only whatever that function
//     happens to cover, and a broker-placed secret from any source it doesn't
//     mirror reaches error text unredacted.
//
// Scope is limited to secret values the broker placed in cfg. Credentials the
// agent mints or obtains after bootstrap — for example a refreshed transport
// credential it keeps in its own token file — are never in cfg and are out of
// scope by design; they are not known to the broker.
func substrateSecretCandidates(cfg RunConfig) map[string]string {
	secrets := make(map[string]string)
	// add keys by "<source>:<name>" rather than bare name. Two different
	// sources can legitimately use the same name (e.g. a file-type
	// ResolvedSecret named "GITHUB_TOKEN" alongside a cfg.Env
	// "GITHUB_TOKEN=..." entry) — with a bare-name map, the second one
	// added would silently overwrite the first map entry, and the
	// overwritten source's value would stay in the request but drop out of
	// the redaction set. The source prefix also makes the redaction marker
	// in an error message more informative ("[value of
	// secret_file:GITHUB_TOKEN redacted]" instead of just the name).
	add := func(source, key, value string) {
		if value == "" {
			return
		}
		secrets[source+":"+key] = value
	}

	if cfg.Harness != nil {
		for k, v := range cfg.Harness.GetEnv(cfg.Name, util.GetHomeDir(cfg.UnixUsername), cfg.UnixUsername) {
			add("harness_env", k, v)
		}
		if cfg.TelemetryEnabled {
			for k, v := range cfg.Harness.GetTelemetryEnv() {
				add("telemetry_env", k, v)
			}
		}
	}

	for _, e := range cfg.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			add("cfg_env", k, v)
		}
	}

	if cfg.ResolvedAuth != nil {
		for k, v := range cfg.ResolvedAuth.EnvVars {
			add("resolved_auth_env", k, v)
		}
		// ResolvedAuth.Files' contents (credential JSON, tokens, etc. read
		// from SourcePath) go into the bootstrap payload the same as any
		// other secret and are just as much a candidate for leaking into
		// an error message. A read failure here is swallowed: it can't
		// leak content it never read, and buildBootstrapFiles independently
		// surfaces the read error (naming only the path, never content).
		for _, f := range cfg.ResolvedAuth.Files {
			if f.SourcePath == "" {
				continue
			}
			if data, err := os.ReadFile(f.SourcePath); err == nil {
				add("resolved_auth_file", f.ContainerPath, string(data))
			}
		}
	}

	for _, s := range cfg.ResolvedSecrets {
		key := s.Name
		if key == "" {
			key = s.Target
		}
		switch s.Type {
		case "environment", "":
			add("secret_env", key, s.Value)
		case "file":
			add("secret_file", key, s.Value)
		}
	}

	return secrets
}

// maxBootstrapFilesTotalBytes caps the sum of decoded (pre-base64) bytes
// across every file buildBootstrapFiles ships — the composed home, auth
// files and file-type secrets combined, after dedup — so a bootstrap
// payload can never grow unbounded.
//
// The number is picked with headroom below the smallest MEASURED limit on
// this path, not an assumed one. The measurements:
//   - The router's ingress listener for this route (atenet-router/envoy) has
//     no request-body cap: no buffer filter and no max_request_bytes are
//     configured anywhere in its filter chain (set_filter_state -> ext_proc
//     -> router), the ext_proc filter is header-only (it never inspects the
//     body), and per_connection_buffer_limit_bytes is unset on both the
//     listener and the upstream cluster — envoy's 1 MiB default there is a
//     flow-control watermark, not a cap. The router streams the body through
//     uninspected. The route's timeout is 300s (idle 330s), well above this
//     package's own 30s bootstrapRequestTimeout for the POST.
//   - substrate-serve's own POST /scion/v1/bootstrap handler is therefore the
//     binding limit: it bounds the raw request body to
//     maxBootstrapBodyBytes = 64 MiB (pkg/sciontool/substrate/server.go) via
//     http.MaxBytesReader — about 48 MiB of raw (pre-base64) file bytes once
//     base64's 4/3 expansion and the JSON envelope are accounted for.
//
// The cap here is sized in DECODED bytes but must survive base64 (4/3
// expansion) plus the surrounding JSON envelope (env map, start_cmd,
// control_token, per-file path/mode/quoting overhead) once encoded onto the
// wire: 16 MiB decoded -> ~21.34 MiB base64 -> plus a JSON envelope that is
// negligible next to file content for any realistic env/start_cmd size. That
// leaves roughly 3x headroom under the measured 64 MiB serve-side limit.
const maxBootstrapFilesTotalBytes = 16 * 1024 * 1024

// homeBootstrapFiles walks homeDir — RunConfig.HomeDir, the broker-composed
// agent home (harness-config home/, the template home, and skills) — and
// returns one bootstrapFile per regular file it finds, with paths rewritten
// under containerHome and the source file's own permission bits preserved
// (mode = perm & 0o777).
//
// It never follows a symlink: filepath.WalkDir already doesn't descend into
// one (each directory entry's type comes from an Lstat-equivalent, the same
// rule pkg/hub/project_workspace_handlers.go's walkDirSearcher relies on for
// the same reason) and this additionally treats a symlink entry the
// same as a socket, a device or a fifo — none of them is "a file to ship" —
// skipping and counting each rather than reading through or blocking on it.
// The only thing ever logged about a skipped entry is its path relative to
// homeDir, capped at the first 20 plus the total count so a home with a huge
// number of skipped entries can't blow up the log line; contents are never
// inspected.
//
// It also keeps a running total of Stat-reported (pre-read) file sizes as it
// walks and returns the same cap error buildBootstrapFiles would eventually
// report — naming only the cap and the total, never a path or content — the
// moment that total exceeds maxBootstrapFilesTotalBytes, before reading the
// file that tipped it over. This is a conservative early exit, not the
// authoritative cap check: buildBootstrapFiles still enforces the real cap
// afterwards, over the deduped home+auth+secret total, since a home file
// that turns out to be overridden by a same-path auth/secret file is never
// counted there. The point here is only to stop a stray oversized file (or
// several) in a template home from being fully buffered into memory before
// any cap is ever consulted.
//
// homeDir == "" is not an error: it means the caller has no composed home to
// ship (e.g. a runtime path that never sets RunConfig.HomeDir), and returns
// (nil, nil) so buildBootstrapFiles' output is byte-for-byte unchanged from
// before homeBootstrapFiles existed. A homeDir that is set but does not
// exist, or resolves to something other than a plain directory, IS an
// error — unlike "no home was composed," that signals something upstream
// (the broker's own home composition) is broken.
func homeBootstrapFiles(homeDir, containerHome string) ([]bootstrapFile, error) {
	if homeDir == "" {
		return nil, nil
	}

	rootInfo, err := os.Lstat(homeDir)
	if err != nil {
		return nil, fmt.Errorf("substrate: home dir %s: %w", homeDir, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("substrate: home dir %s is a symlink, refusing to walk it", homeDir)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("substrate: home dir %s is not a directory", homeDir)
	}

	var files []bootstrapFile
	var skipped []string
	var total int64
	walkErr := filepath.WalkDir(homeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("substrate: walk home dir at %s: %w", path, err)
		}
		if path == homeDir {
			return nil
		}
		if d.IsDir() {
			// Directories are never shipped as entries themselves; recurse
			// into them (the default filepath.WalkDir behavior for a real,
			// non-symlinked directory entry).
			return nil
		}

		rel, relErr := filepath.Rel(homeDir, path)
		if relErr != nil {
			return fmt.Errorf("substrate: relativize home file %s: %w", path, relErr)
		}

		if !d.Type().IsRegular() {
			skipped = append(skipped, rel)
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return fmt.Errorf("substrate: stat home file %s: %w", rel, infoErr)
		}
		// Check the running total against the cap using the size Stat
		// already reported, before reading the file's content into memory.
		// buildBootstrapFiles enforces the real cap later, over the deduped
		// home+auth+secret total, and that check still stands; this one is
		// a conservative early exit against a single oversized (or several
		// large) home files so a stray multi-GB file in a template home
		// isn't fully buffered here first. It is conservative, not exact,
		// because it runs on home's own pre-dedup total: a home file that
		// ends up overridden by an auth/secret file at the same path (and
		// so never counted in the final total) could in principle trip this
		// early check on its own, which is an acceptable, strictly-safer
		// trade against ever buffering an unbounded amount of file content.
		total += info.Size()
		if total > maxBootstrapFilesTotalBytes {
			return fmt.Errorf("substrate: bootstrap files total %d bytes exceeds cap of %d bytes", total, maxBootstrapFilesTotalBytes)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("substrate: read home file %s: %w", rel, readErr)
		}
		files = append(files, bootstrapFile{
			Path:        filepath.Clean(filepath.Join(containerHome, rel)),
			Mode:        int(info.Mode().Perm()),
			ContentB64:  base64.StdEncoding.EncodeToString(data),
			decodedSize: int64(len(data)),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(skipped) > 0 {
		const maxLoggedSkipped = 20
		logged := skipped
		if len(logged) > maxLoggedSkipped {
			logged = logged[:maxLoggedSkipped]
		}
		runtimeLog.Info("substrate: skipped non-regular home entries during bootstrap", "count", len(skipped), "paths", logged)
	}
	return files, nil
}

// dedupeBootstrapFilesByPath concatenates groups in the given order and
// collapses duplicate Paths to one entry: the LAST occurrence's content
// wins, but it keeps the position of the FIRST occurrence, so the result is
// deterministic and stable regardless of how many groups collide on a path.
// This is where the one-entry-per-path wire rule (deploy/substrate/README.md,
// "Bootstrap files: home delivery") is enforced — substrate-serve is not
// expected to reconcile duplicates itself.
func dedupeBootstrapFilesByPath(groups ...[]bootstrapFile) []bootstrapFile {
	index := make(map[string]int)
	var out []bootstrapFile
	for _, group := range groups {
		for _, f := range group {
			if i, ok := index[f.Path]; ok {
				out[i] = f
				continue
			}
			index[f.Path] = len(out)
			out = append(out, f)
		}
	}
	return out
}

// buildBootstrapFiles assembles the "files" array in precedence order: the
// broker-composed home (RunConfig.HomeDir) first, then ResolvedAuth.Files
// (read from SourcePath), then file-type ResolvedSecrets — auth and secret
// entries are expected to override a same-path file the composed home
// shipped, per the home-delivery design. Every entry's Path is
// filepath.Clean-ed before dedup — home's already is (filepath.Join cleans
// internally), but an auth ContainerPath or secret Target that is already
// absolute is used as-is by expandTildeTarget, and two differently-spelled
// but equivalent paths (e.g. "/home/scion/x" and "/home/scion//x") would
// otherwise both survive dedup as distinct entries. Duplicate paths across
// the three sources are then deduped (see dedupeBootstrapFilesByPath) so the
// wire payload never carries two entries for the same Path.
//
// The total decoded size across the deduped result is capped at
// maxBootstrapFilesTotalBytes (see its doc comment for how that number was
// chosen); the error on overflow names only the cap and the total size,
// never any path or content.
func buildBootstrapFiles(cfg RunConfig) ([]bootstrapFile, error) {
	containerHome := util.GetHomeDir(cfg.UnixUsername)

	homeFiles, err := homeBootstrapFiles(cfg.HomeDir, containerHome)
	if err != nil {
		return nil, err
	}

	var authFiles []bootstrapFile
	if cfg.ResolvedAuth != nil {
		for _, f := range cfg.ResolvedAuth.Files {
			if f.SourcePath == "" {
				continue
			}
			data, err := os.ReadFile(f.SourcePath)
			if err != nil {
				return nil, fmt.Errorf("substrate: read auth file %s: %w", f.SourcePath, err)
			}
			authFiles = append(authFiles, bootstrapFile{
				Path:        filepath.Clean(expandTildeTarget(f.ContainerPath, containerHome)),
				Mode:        defaultFileMode,
				ContentB64:  base64.StdEncoding.EncodeToString(data),
				decodedSize: int64(len(data)),
			})
		}
	}

	var secretFiles []bootstrapFile
	for _, s := range cfg.ResolvedSecrets {
		if s.Type != "file" {
			continue
		}
		secretFiles = append(secretFiles, bootstrapFile{
			Path:        filepath.Clean(expandTildeTarget(s.Target, containerHome)),
			Mode:        defaultFileMode,
			ContentB64:  base64.StdEncoding.EncodeToString([]byte(s.Value)),
			decodedSize: int64(len(s.Value)),
		})
	}

	files := dedupeBootstrapFilesByPath(homeFiles, authFiles, secretFiles)

	var total int64
	for _, f := range files {
		total += f.decodedSize
	}
	if total > maxBootstrapFilesTotalBytes {
		return nil, fmt.Errorf("substrate: bootstrap files total %d bytes exceeds cap of %d bytes", total, maxBootstrapFilesTotalBytes)
	}

	return files, nil
}

// generateControlToken returns a random 32-byte hex string, used both as
// the bearer for POST /scion/v1/exec (substrate-runtime.md §5.1) and, in the
// fallback nonce (see (*SubstrateRuntime).bootstrapNonce), as the
// bootstrap bearer itself.
func generateControlToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("substrate: generate control token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// waitForHealthz polls GET /scion/v1/healthz through the router until it
// reports wantState, or timeout elapses, or ctx is cancelled. sleep is the
// cancellable wait between attempts (sleepWithContext in production, a
// counting/no-op variant in tests); it reports ctx.Err() if the context ends
// during the wait so a cancelled ctx returns promptly instead of blocking out
// a full backoff. The returned error wraps ctx.Err() (via %w) when the
// context is cancelled or its deadline passes.
func waitForHealthz(ctx context.Context, router *substrate.RouterClient, atespace, actorName, wantState string, timeout time.Duration, sleep func(context.Context, time.Duration) error) error {
	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond
	const maxBackoff = 5 * time.Second

	for {
		state, err := getHealthz(ctx, router, atespace, actorName)
		if err == nil && state == wantState {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("substrate: %s/%s wait for healthz state %q aborted: %w", atespace, actorName, wantState, ctxErr)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("substrate: %s/%s did not reach healthz state %q within %s: %w", atespace, actorName, wantState, timeout, err)
			}
			return fmt.Errorf("substrate: %s/%s did not reach healthz state %q within %s (last state: %q)", atespace, actorName, wantState, timeout, state)
		}
		if err := sleep(ctx, backoff); err != nil {
			return fmt.Errorf("substrate: %s/%s wait for healthz state %q aborted: %w", atespace, actorName, wantState, err)
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// sleepWithContext waits for d, or until ctx is done, whichever comes first.
// It returns ctx.Err() if the context is cancelled or its deadline passes
// during the wait, and nil if the full duration elapsed. It is the production
// value of waitForHealthz's sleep seam, so a cancelled poll returns promptly
// rather than blocking out the remaining backoff.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func getHealthz(ctx context.Context, router *substrate.RouterClient, atespace, actorName string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, healthzRequestTimeout)
	defer cancel()

	resp, err := router.Do(ctx, atespace, actorName, http.MethodGet, substrateHealthzPath, nil, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("healthz returned status %d", resp.StatusCode)
	}
	var hz healthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&hz); err != nil {
		return "", fmt.Errorf("decode healthz response: %w", err)
	}
	return hz.State, nil
}

// errBootstrapHijacked is returned by postBootstrap when the control server
// answers 409: someone bootstrapped this actor before the broker's own
// request landed.
//
// Under the fallback nonce (substrate-runtime.md §5.2: any bearer accepted,
// correctness resting on "first bootstrap wins" plus a NetworkPolicy
// restricting router ingress to the broker namespace), the broker is
// supposed to be the only caller that can ever reach this endpoint before
// it is claimed — it sends its POST immediately after the actor reaches
// RUNNING. A 409 on the broker's own first attempt therefore means another
// caller reached the control server first: either the NetworkPolicy has a
// gap, or something else in the broker's namespace raced it. That is not a
// benign retry to swallow; it is the only signal available that the
// fallback's trust assumption was violated, so Run treats it as a
// compromise indicator (see Run's handling of this sentinel).
var errBootstrapHijacked = errors.New("bootstrap rejected: actor was already bootstrapped by another caller")

// bootstrapPathErrorMiddle and bootstrapPathErrorTail are the two fixed
// substrings of pkg/sciontool/substrate's bootstrapPathError.Error() wire
// text: "<code>: bootstrap file <quoted path> rejected: <detail>". They are
// the client-side half of that wire contract — parseBootstrapPathError
// splits on them rather than a regexp so an arbitrary path (quotes,
// whitespace, anything strconv.Quote can round-trip) never needs escaping
// twice.
const (
	bootstrapPathErrorMiddle = ": bootstrap file "
	bootstrapPathErrorTail   = " rejected: "
)

// parseBootstrapPathError extracts the stable code, the offending path, and
// the human-readable detail from a 422 response body shaped like
// pkg/sciontool/substrate's bootstrapPathError.Error(). ok is false if body
// doesn't match that shape (a server that doesn't speak this contract, or a
// future body format this client hasn't caught up to yet) — callers must
// still surface body as-is rather than silently dropping it.
func parseBootstrapPathError(body string) (code, path, detail string, ok bool) {
	body = strings.TrimSpace(body)
	i := strings.Index(body, bootstrapPathErrorMiddle)
	if i < 0 {
		return "", "", "", false
	}
	code = body[:i]
	rest := body[i+len(bootstrapPathErrorMiddle):]

	// The path is a Go-quoted string (strconv.Quote on the server side), so
	// find exactly where it ends with strconv.QuotedPrefix rather than
	// strings.Index-ing for bootstrapPathErrorTail directly in rest: the
	// quoted path itself can legitimately contain the literal substring
	// " rejected: " (e.g. a directory named "not rejected: yet"), which would
	// make a naive Index find a tail *inside* the quotes and hand
	// strconv.Unquote a truncated, unparsable fragment.
	q, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return "", "", "", false
	}
	afterQuote := rest[len(q):]
	if !strings.HasPrefix(afterQuote, bootstrapPathErrorTail) {
		return "", "", "", false
	}
	p, err := strconv.Unquote(q)
	if err != nil {
		return "", "", "", false
	}
	detail = afterQuote[len(bootstrapPathErrorTail):]
	return code, p, detail, true
}

// bootstrapPathRejectedError is returned by postBootstrap when the control
// server answers 422 for a bootstrap file whose Path failed validation (see
// pkg/sciontool/substrate's bootstrapPathError — this is the client-side
// mirror of that wire contract). Code and Path are configuration, never
// secret (see deploy/substrate/README.md's "No symlink traversal in a
// target's path" note), and are safe to log and return as-is. If the body
// doesn't parse (parseBootstrapPathError's ok is false), Code and Path stay
// empty and Detail carries the raw body, so nothing is silently dropped
// either way.
type bootstrapPathRejectedError struct {
	code   string
	path   string
	detail string
}

func (e *bootstrapPathRejectedError) Error() string {
	if e.code == "" && e.path == "" {
		return fmt.Sprintf("substrate: bootstrap rejected a file path (422): %s", e.detail)
	}
	// %q, not %s, for e.path: parseBootstrapPathError unquotes the path out
	// of the 422 body, so a newline or other control byte in a configured
	// target would otherwise land raw in this error's text. Quoting also
	// makes an empty path visible as "" instead of turning "file  (...)"
	// into a confusing double space.
	return fmt.Sprintf("substrate: bootstrap rejected file %q (%s): %s", e.path, e.code, e.detail)
}

// maxEmbeddedErrorBytes bounds how much of a control-server-supplied body or
// stderr this file ever embeds directly in a returned error's message:
// postBootstrap's and doExec's non-2xx response bodies, and a failed exec
// command's captured stderr.
// The control server's own output caps (substrate-runtime.md §5.1) are
// sized for a diagnostic response body, not an error string — embedding up
// to 4 MiB of stderr in a Go error turns one failed exec into a
// multi-megabyte log line. truncateForError enforces this, after
// redaction.
const maxEmbeddedErrorBytes = 4096

// maxErrorBodyReadBytes bounds how much of a non-2xx response body doExec
// and postBootstrap read before redacting it and truncating the result to
// maxEmbeddedErrorBytes. Reading well past the embedded size matters for
// redaction: a secret that straddles the final cut is redacted as a whole
// before the cut is made, and one that straddles this read limit instead
// begins far beyond maxEmbeddedErrorBytes, so none of it survives the
// truncation either.
const maxErrorBodyReadBytes = 64 * 1024

// truncateForError bounds s to maxEmbeddedErrorBytes for embedding in an
// error's message, appending a marker naming how many bytes were cut so the
// truncation itself is never mistaken for the whole message. The cut backs
// off to a UTF-8 rune boundary so it never splits a multi-byte character.
//
// Truncation must come after redaction: a secret cut in half no longer
// matches the value a redaction pass searches for, so truncating first
// would leave its leading bytes in the message.
func truncateForError(s string) string {
	if len(s) <= maxEmbeddedErrorBytes {
		return s
	}
	cut := maxEmbeddedErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...[truncated %d bytes]", s[:cut], len(s)-cut)
}

// postBootstrap sends the bootstrap payload through the router, authorized
// with nonce (substrate-runtime.md §5.3). Any non-2xx status is an error; 409
// specifically becomes errBootstrapHijacked (see its doc comment) rather
// than being treated as an idempotent no-op, and 422 becomes
// bootstrapPathRejectedError (see its doc comment) rather than falling into
// the generic "other write failures" case below.
//
// redact is applied to the full non-2xx response body before any part of it
// is truncated and embedded in the returned error, so a secret straddling
// the truncation point is still recognized and removed. nil means no
// redaction.
func postBootstrap(ctx context.Context, router *substrate.RouterClient, atespace, actorName, nonce string, req bootstrapRequest, redact func(string) string) error {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	ctx, cancel := context.WithTimeout(ctx, bootstrapRequestTimeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("substrate: marshal bootstrap request: %w", err)
	}

	resp, err := router.Do(ctx, atespace, actorName, http.MethodPost, substrateBootstrapPath, bytes.NewReader(body), map[string]string{
		"Authorization": "Bearer " + nonce,
		"Content-Type":  "application/json",
	})
	if err != nil {
		return fmt.Errorf("substrate: POST bootstrap to %s/%s: %w", atespace, actorName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusConflict {
		return errBootstrapHijacked
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyReadBytes))
	msg := redact(string(raw))
	if resp.StatusCode == http.StatusUnprocessableEntity {
		code, path, detail, ok := parseBootstrapPathError(msg)
		if !ok {
			detail = msg
		}
		return &bootstrapPathRejectedError{code: code, path: path, detail: truncateForError(detail)}
	}
	return fmt.Errorf("substrate: bootstrap %s/%s failed: status %d: %s", atespace, actorName, resp.StatusCode, truncateForError(msg))
}

// maxExecStdinBytes bounds the raw (pre-base64) stdin payload doExec will
// send. Derived from the control server's own exported limits
// (pkg/sciontool/substrate.MaxExecBodyBytes and .ExecEnvelopeAllowanceBytes)
// rather than a duplicated number: the whole exec request body — argv,
// metadata, and the base64-encoded Stdin field together — must fit within
// MaxExecBodyBytes, and base64 expands N raw bytes to (4/3)*N encoded bytes,
// so the raw byte cap is (MaxExecBodyBytes - ExecEnvelopeAllowanceBytes) * 3/4.
const maxExecStdinBytes = (substrateserve.MaxExecBodyBytes - substrateserve.ExecEnvelopeAllowanceBytes) * 3 / 4

// readExecStdin reads all of stdin into memory, capped at
// maxExecStdinBytes+1 so an over-limit reader is rejected outright instead
// of being silently truncated. The returned error never includes any of the
// content read — stdin exists specifically to carry secrets.
func readExecStdin(stdin io.Reader) ([]byte, error) {
	if stdin == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxExecStdinBytes+1))
	if err != nil {
		return nil, fmt.Errorf("substrate: read exec stdin: %w", err)
	}
	if len(data) > maxExecStdinBytes {
		return nil, fmt.Errorf("substrate: exec stdin exceeds %d bytes", maxExecStdinBytes)
	}
	return data, nil
}

// errStdinUnsupported marks a doExec failure caused specifically by the
// control server not confirming StdinSupported for a request that sent
// stdin — as distinct from a transport error, an authorization failure, or
// any other way doExec can fail. A caller that wants to name version skew
// specifically, and only that, checks errors.Is(err, errStdinUnsupported)
// rather than treating every doExec error the same way.
var errStdinUnsupported = errors.New("control server did not confirm stdin support")

// doExec sends argv through the router to the actor's control server,
// authorized with the actor's control_token (substrate-runtime.md §4). When
// stdin is non-empty, it is delivered via execRequest.Stdin rather than
// embedded in argv, and a response that doesn't confirm StdinSupported is
// treated as a hard failure rather than a silent no-op: see doExec's
// StdinSupported check below and execResponse's doc comment for why version
// skew must fail loudly instead of running the command without its input.
//
// redact is applied to the full non-2xx response body and to a failed
// command's full stderr before either is truncated for embedding in the
// returned error, so a secret straddling the truncation point is still
// recognized and removed. nil means no redaction.
func doExec(ctx context.Context, router *substrate.RouterClient, atespace, actorName, controlToken string, argv []string, stdin []byte, user string, timeout time.Duration, redact func(string) string) (execResponse, error) {
	var out execResponse
	if redact == nil {
		redact = func(s string) string { return s }
	}

	// The HTTP round trip needs longer than timeout_s itself: the control
	// server enforces timeout_s server-side and then still has to write the
	// response, and the request has to reach it and come back. Racing the
	// client's deadline against the server's own enforcement would cut exec
	// calls short.
	ctx, cancel := context.WithTimeout(ctx, timeout+execTimeoutSlack)
	defer cancel()

	body, err := json.Marshal(execRequest{
		Argv:     argv,
		User:     user,
		TimeoutS: int(timeout.Seconds()),
		Stdin:    stdin,
	})
	if err != nil {
		return out, fmt.Errorf("substrate: marshal exec request: %w", err)
	}

	resp, err := router.Do(ctx, atespace, actorName, http.MethodPost, substrateExecPath, bytes.NewReader(body), map[string]string{
		"Authorization": "Bearer " + controlToken,
		"Content-Type":  "application/json",
	})
	if err != nil {
		return out, fmt.Errorf("substrate: POST exec to %s/%s: %w", atespace, actorName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyReadBytes))
		return out, fmt.Errorf("substrate: exec on %s/%s failed: status %d: %s", atespace, actorName, resp.StatusCode, truncateForError(redact(string(msg))))
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("substrate: decode exec response from %s/%s: %w", atespace, actorName, err)
	}
	if len(stdin) > 0 && !out.StdinSupported {
		// The control server didn't echo back confirmation that it
		// understood Stdin — an older sciontool image in the actor would
		// silently ignore the field and still report a normal exit code,
		// which must never be mistaken for the command having received its
		// input. Fail closed rather than trust an ambiguous success. Wraps
		// errStdinUnsupported specifically (rather than just this message)
		// so a caller like ExecWithStdin's capability probe can tell this
		// exact condition apart from every other way doExec can fail, and
		// only name version skew for this one.
		return out, fmt.Errorf("substrate: exec on %s/%s: %w; refusing to treat the result as having received it", atespace, actorName, errStdinUnsupported)
	}
	if out.ExitCode != 0 {
		return out, fmt.Errorf("substrate: exec on %s/%s exited %d: %s", atespace, actorName, out.ExitCode, truncateForError(redact(out.Stderr)))
	}
	return out, nil
}
