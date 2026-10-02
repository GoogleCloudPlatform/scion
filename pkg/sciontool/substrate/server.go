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

package substrate

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

const (
	// maxBootstrapBodyBytes bounds the bootstrap request body (env, files,
	// start_cmd, control_token) via http.MaxBytesReader, so an over-limit
	// body fails closed (the read errors out at the limit) rather than being
	// buffered without bound.
	//
	// This is the binding limit on the whole path, measured against the
	// actual transport: the router's ingress listener for this route has no
	// request-body cap of its own (no buffer filter, no max_request_bytes
	// anywhere in its filter chain, and per_connection_buffer_limit_bytes —
	// a flow-control watermark, not a cap — is unset on both the listener
	// and the upstream cluster) and streams the body through uninspected;
	// its route timeout is 300s. 64 MiB here is therefore the number that
	// matters, and pkg/runtime/substrate_bootstrap.go's
	// maxBootstrapFilesTotalBytes (16 MiB decoded, ~21.3 MiB base64) is sized
	// with headroom under it — see that constant's doc comment for the
	// full accounting (base64 4/3 expansion plus the JSON envelope). If a
	// smaller router-side cap is ever added, that constant is the one to
	// shrink; this one should stay comfortably above it.
	maxBootstrapBodyBytes = 64 * 1024 * 1024

	// MaxExecBodyBytes bounds the exec request body (argv, metadata, and an
	// optional base64-encoded Stdin payload — no large payloads are expected
	// here; Stdin exists to carry a short secret like a token, not a bulk
	// transfer). Exported so pkg/runtime can derive its own Stdin size cap
	// from this single authoritative limit instead of duplicating the
	// number — see ExecEnvelopeAllowanceBytes.
	MaxExecBodyBytes = 1 * 1024 * 1024

	// ExecEnvelopeAllowanceBytes is a conservative upper bound on the
	// non-Stdin portion of an ExecRequest's JSON encoding (Argv, User,
	// TimeoutS, and JSON structural overhead), reserved out of
	// MaxExecBodyBytes so a client can size its own Stdin cap without
	// knowing the real Argv/User/TimeoutS sizes in advance. A real request's
	// Argv is a handful of short arguments (e.g. resetAuth's fixed write
	// script), far under this.
	ExecEnvelopeAllowanceBytes = 4096

	// defaultFileMode is used when a bootstrap file entry omits mode (0).
	// 0o600 (owner read/write only), not a more permissive default: a
	// bootstrap file can carry secret content (a CA bundle, an env file
	// with credentials), and a caller that wants it more widely readable
	// must say so explicitly via a non-zero Mode rather than relying on a
	// default that happens to be permissive.
	defaultFileMode = 0o600

	// privilegeDropPreconditionFailedMsg is the fixed, secret-free response
	// body when PrivilegeDropChecker rejects a bootstrap (see its doc
	// comment). It never includes the checker's own error, which is
	// cmd-layer-defined and outside this package's control: the real reason
	// (missing capability, missing scion user, unset SCION_HOST_UID/GID) is
	// only logged server-side.
	privilegeDropPreconditionFailedMsg = "bootstrap rejected: privilege-drop precondition failed"
)

// InitRunner runs the equivalent of `sciontool init -- <argv...>` in
// process. forwardTermSignal must be false when called from Bootstrap: see
// commands.InitRunOptions.ForwardTermSignal for why substrate-serve must own
// SIGTERM handling itself.
//
// The concrete implementation (cmd/sciontool/commands.RunInit) lives in the
// cmd layer; Server takes it as a function value so this package never
// imports cmd/sciontool/commands. This is the "reuse, don't fork" seam for
// the init logic required by substrate-runtime.md §5.1.
type InitRunner func(argv []string, forwardTermSignal bool) int

// PrivilegeDropChecker is InitRunner's synchronous companion: when the
// privilege drop that setupHostUser is about to attempt cannot succeed,
// Run() must return an error and the broker must delete the actor, not just
// avoid starting the harness. Exiting the actor's PID 1 after the fact is
// too late, because /bootstrap has already answered 200 by then.
//
// handleBootstrap calls this synchronously — after req.Env has been applied
// to the process environment (so SCION_HOST_UID/GID are visible), but
// before it commits to a 200 response or starts the in-process init — and
// treats a non-nil error as a bootstrap failure: a non-2xx response, with
// the init runner never invoked. The broker's postBootstrap already treats
// any non-2xx as an error, and Run already runs its cleanup() (delete the
// actor and its egress policy) on any postBootstrap error, so wiring the
// check in here reuses that existing failure path end to end rather than
// adding new plumbing to pkg/runtime.
//
// The concrete implementation lives in the cmd layer (same reason as
// InitRunner: this package must never import cmd/sciontool/commands) and is
// deliberately cheap and side-effect-free — it re-checks that setupHostUser's
// realignment is expected to succeed (capabilities, the scion user, the
// host UID/GID) without performing it a second time.
//
// Optional: a Server built without one (e.g. most existing tests, and any
// runtime other than substrate-serve) skips the check entirely.
type PrivilegeDropChecker func() error

// RootfsFixup is PrivilegeDropChecker's fallback companion: handleBootstrap
// calls it, right before the precondition, as a defensive second call site
// for the same rootfs fixup substrate-serve's own startup already runs
// (see the cmd layer's fixupRootfsForScion for what it fixes and why). It
// should normally be a no-op here, since the golden snapshot already has
// the corrected rootfs baked in from startup; this only matters if a
// pre-snapshot actor somehow reaches /bootstrap without having gone
// through that startup path.
//
// Optional, for the same reason as PrivilegeDropChecker: a Server built
// without one (most existing tests) skips it entirely.
type RootfsFixup func()

// Server implements the `sciontool substrate-serve` control server
// (substrate-runtime.md §5.1): healthz, one-shot bootstrap, and authenticated
// exec. /pty, /rehydrate and /tunnel/open are not currently supported.
type Server struct {
	nonceVerifier      NonceVerifier
	runInit            InitRunner
	privilegeDropCheck PrivilegeDropChecker
	rootfsFixup        RootfsFixup

	// chownUID/chownGID own bootstrap-written files and directories,
	// matching the "scion" user's ownership. -1 means "don't chown"
	// (e.g. no scion user found, or running as a non-root test).
	chownUID int
	chownGID int

	mu           sync.Mutex
	bootstrapped bool
	initFailed   bool
	controlToken string
}

// Option configures a Server at construction time.
type Option func(*Server)

// WithNonceVerifier overrides the bootstrap nonce verifier. Default is
// FirstBootstrapWinsVerifier (substrate-runtime.md §5.2 fallback).
func WithNonceVerifier(v NonceVerifier) Option {
	return func(s *Server) { s.nonceVerifier = v }
}

// WithInitRunner sets the function used to run the init path after a
// successful bootstrap. Required for a Server that will actually serve
// bootstrap requests; tests that only exercise auth/state may omit it.
func WithInitRunner(r InitRunner) Option {
	return func(s *Server) { s.runInit = r }
}

// WithPrivilegeDropChecker sets the synchronous precondition handleBootstrap
// runs before committing to 200 OK and starting the in-process init. See
// PrivilegeDropChecker's doc comment.
func WithPrivilegeDropChecker(c PrivilegeDropChecker) Option {
	return func(s *Server) { s.privilegeDropCheck = c }
}

// WithRootfsFixup sets the fallback rootfs fixup handleBootstrap runs right
// before the privilege-drop precondition. See RootfsFixup's doc comment.
func WithRootfsFixup(f RootfsFixup) Option {
	return func(s *Server) { s.rootfsFixup = f }
}

// WithChownOwner overrides the uid/gid used to chown bootstrap-written
// files. Defaults to the "scion" system user, when present.
func WithChownOwner(uid, gid int) Option {
	return func(s *Server) { s.chownUID, s.chownGID = uid, gid }
}

// NewServer creates a Server ready to be mounted with Handler().
func NewServer(opts ...Option) *Server {
	s := &Server{
		nonceVerifier: FirstBootstrapWinsVerifier{},
	}
	s.chownUID, s.chownGID = lookupScionOwner()
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// lookupScionOwner resolves the "scion" system user's uid/gid. It returns
// (-1, -1) if the user can't be found (e.g. in a unit test environment),
// which callers treat as "skip chown".
func lookupScionOwner() (int, int) {
	u, err := user.Lookup("scion")
	if err != nil {
		return -1, -1
	}
	uid, errU := strconv.Atoi(u.Uid)
	gid, errG := strconv.Atoi(u.Gid)
	if errU != nil || errG != nil {
		return -1, -1
	}
	return uid, gid
}

// Handler returns the http.Handler for the control server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/scion/v1/healthz", s.handleHealthz)
	mux.HandleFunc("/scion/v1/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/scion/v1/exec", s.handleExec)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := StateAwaitingBootstrap
	bootstrapped, initFailed := s.bootstrapState()
	if initFailed {
		state = StateInitFailed
	} else if bootstrapped {
		state = StateRunning
	}
	writeJSON(w, http.StatusOK, HealthzResponse{State: state})
}

func (s *Server) isBootstrapped() bool {
	bootstrapped, _ := s.bootstrapState()
	return bootstrapped
}

// bootstrapState reads bootstrapped and initFailed together under one lock.
func (s *Server) bootstrapState() (bootstrapped, initFailed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootstrapped, s.initFailed
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token, ok := bearerToken(r)
	if !ok || !s.nonceVerifier.VerifyNonce(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.isBootstrapped() {
		http.Error(w, "already bootstrapped", http.StatusConflict)
		return
	}

	var req BootstrapRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBootstrapBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "bootstrap request body exceeds limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Claim the single bootstrap slot now, before performing any side
	// effects. A second request that arrives concurrently (or after a
	// failure below) gets 409, matching "accepted once per process
	// lifetime" literally — retrying a bootstrap that failed partway
	// through is not currently supported.
	s.mu.Lock()
	if s.bootstrapped {
		s.mu.Unlock()
		http.Error(w, "already bootstrapped", http.StatusConflict)
		return
	}
	s.bootstrapped = true
	s.mu.Unlock()

	// Enforced-mode broker-delivered hook content lands under
	// hooks.EnforcedHooksDir instead of the workload's home (see
	// writeBootstrapFile's redirectEnforcedHooksPath call below); clear any
	// stale content there first so a hook removed since a previous
	// bootstrap cannot survive into this one. Fail-CLOSED, not best-effort:
	// a clear failure aborts the bootstrap before any file is written or
	// init starts, exactly like a writeBootstrapFile error below — a stale,
	// still root-owned hook a failed clear left behind must never run.
	if err := clearEnforcedHooksDir(); err != nil {
		var pathErr *bootstrapPathError
		if errors.As(err, &pathErr) {
			// Same reasoning as the writeBootstrapFile 422 branch below:
			// a path-shape rejection (here, a symlinked or non-directory
			// enforced hooks root) is answered with the error's own stable
			// Code and Path.
			log.Error("bootstrap: rejected enforced hooks dir: %v", redactErr(err))
			http.Error(w, pathErr.Error(), http.StatusUnprocessableEntity)
			return
		}
		log.Error("bootstrap: failed to clear enforced hooks dir: %v", redactErr(err))
		http.Error(w, "failed to clear enforced hooks dir", http.StatusInternalServerError)
		return
	}

	// The private, root-only scratch directory RunInit uses to stage
	// content it must both write and read back before installing the
	// result into a workload-owned location (see cmd/sciontool/commands'
	// configureSharedWorkspaceGit) gets the same fail-closed, cleared-at-
	// every-bootstrap treatment as the enforced hooks dir above, for the
	// same reason: a stale entry left by an earlier bootstrap of a reused
	// actor must never be visible to this one.
	if err := ensurePrivateTmpDir(); err != nil {
		var pathErr *bootstrapPathError
		if errors.As(err, &pathErr) {
			log.Error("bootstrap: rejected private tmp dir: %v", redactErr(err))
			http.Error(w, pathErr.Error(), http.StatusUnprocessableEntity)
			return
		}
		log.Error("bootstrap: failed to prepare private tmp dir: %v", redactErr(err))
		http.Error(w, "failed to prepare private tmp dir", http.StatusInternalServerError)
		return
	}

	for _, f := range req.Files {
		if err := s.writeBootstrapFile(f); err != nil {
			// Deliberately do not include the file's content or the
			// underlying error's arguments in the response/log beyond the
			// path — content_b64 may carry secrets.
			var pathErr *bootstrapPathError
			if errors.As(err, &pathErr) {
				// A path-shape rejection (symlink traversal, or an invalid
				// path) is the caller's fault, not a server error: answer
				// 422 with the error's stable Code and its own Path, so the
				// broker can surface both without depending on this generic
				// message's exact text (see deploy/substrate/README.md's "No
				// symlink traversal in a target's path" note).
				// Path is configuration, not secret — file content is what
				// must never appear here, and pathErr.Error() never
				// includes it.
				//
				// Log redactErr(err) alone, not pathErr.path separately:
				// err.Error() (== pathErr.Error()) already names the path,
				// quoted via strconv.Quote, so it is always a single line —
				// logging pathErr.path a second time here would repeat it
				// unquoted, letting an embedded newline split the log line
				// (or forge a second one).
				log.Error("bootstrap: rejected file: %v", redactErr(err))
				http.Error(w, pathErr.Error(), http.StatusUnprocessableEntity)
				return
			}
			log.Error("bootstrap: failed to write file %q: %v", f.Path, redactErr(err))
			http.Error(w, "failed to write bootstrap files", http.StatusInternalServerError)
			return
		}
	}

	for k, v := range req.Env {
		if isPID1RootLookupEnvVar(k) {
			// req.Env is broker/operator-supplied workload environment,
			// applied here directly onto substrate-serve's own PID 1 —
			// which still runs as root and execs a number of things itself
			// before ever dropping privilege. This process's root-context
			// exec sites never resolve a bare command name through $PATH
			// at all (see pkg/sciontool/rootexec), but PATH, LD_*,
			// BASH_ENV, ENV, and IFS influence process behavior more
			// broadly than just that one lookup (LD_PRELOAD in particular
			// affects the dynamic loader for anything this process execs
			// with an inherited environment, regardless of how the
			// executable's path was found) — so none of them are ever
			// allowed to reach PID 1's own environment via this loop,
			// no matter what the request asks for. Unlike the rest of
			// req.Env, an operator-supplied override for one of these keys
			// does not reach the harness child either: supervisor.Run
			// builds the child's environment from this process's own
			// os.Environ() (pkg/sciontool/supervisor/supervisor.go),
			// which is exactly what this refusal keeps clean. The harness
			// runs with whatever PATH/LD_*/etc. this image already has,
			// not a request-supplied override.
			log.Debug("bootstrap: refusing to set %s on substrate-serve's own PID 1 environment", k)
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			log.Error("bootstrap: failed to set env var %s: %v", k, err)
		}
	}

	// Call site 2 (fallback): re-run the rootfs fixup right before the
	// privilege-drop precondition, which depends on it (traversability and
	// home ownership). See RootfsFixup's doc comment for why this is
	// normally a no-op.
	if s.rootfsFixup != nil {
		s.rootfsFixup()
	}

	// Synchronous privilege-drop precondition (see PrivilegeDropChecker's
	// doc comment): must run after req.Env lands in the process environment
	// (SCION_HOST_UID/GID come from there) and before the response commits
	// to 200 or the init runner starts. bootstrapped is left as already
	// claimed above — there is no bootstrap retry, and the broker is
	// expected to delete this actor on the non-2xx response below.
	// s.controlToken is NOT set yet (see below): /exec's own gate requires
	// both bootstrapped and a non-empty, matching controlToken, so leaving
	// it unset here means a bootstrap that fails past this point — this
	// check or any later one — never leaves /exec authenticatable with the
	// token from a bootstrap that never actually completed.
	if s.privilegeDropCheck != nil {
		if err := s.privilegeDropCheck(); err != nil {
			log.Error("bootstrap: privilege-drop precondition failed: %v", redactErr(err))
			http.Error(w, privilegeDropPreconditionFailedMsg, http.StatusInternalServerError)
			return
		}
	}

	if s.runInit != nil {
		childArgs := []string{"sh", "-c", req.StartCmd}
		go func() {
			// forwardTermSignal=false: substrate-serve is PID 1 and owns
			// SIGTERM itself (log, don't forward — see
			// cmd/substrate_serve.go and InitRunOptions.ForwardTermSignal).
			exitCode := s.runInit(childArgs, false)
			log.Info("substrate-serve: in-process init exited with code %d", exitCode)
			// The control server deliberately stays up regardless of
			// exitCode — Substrate does not observe PID 1 exiting as a
			// failure signal (the actor stays running and keeps its
			// worker either way), so there is nothing to gain and exec-
			// based diagnosis to lose by exiting here. Flip healthz to a
			// distinct, HTTP-reachable state instead, so a caller that
			// knows to check it can tell the difference from a genuinely
			// running harness. The primary failure signal is the direct
			// Hub report reportInitFailure (cmd/sciontool/commands) already
			// makes from inside RunInit itself, before RunInit (and so
			// runInit here) returns.
			if exitCode != 0 {
				s.mu.Lock()
				s.initFailed = true
				s.mu.Unlock()
			}
		}()
	} else {
		log.Error("bootstrap: no InitRunner configured; child process was not started")
	}

	// Published only now, after every step that can still fail this
	// request has succeeded: see the comment above the privilege-drop
	// check for why an earlier publish would let /exec authenticate with
	// a token from a bootstrap that never actually completed.
	s.mu.Lock()
	s.controlToken = req.ControlToken
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// isPID1RootLookupEnvVar reports whether key must never be applied to
// substrate-serve's own PID 1 process environment via req.Env, regardless
// of what a bootstrap request asks for: PATH, LD_* (LD_PRELOAD and
// LD_LIBRARY_PATH above all), BASH_ENV, ENV, and IFS all influence how a
// process — or anything it execs while still inheriting its environment —
// resolves and runs code, not merely which value a workload script sees.
func isPID1RootLookupEnvVar(key string) bool {
	switch key {
	case "PATH", "BASH_ENV", "ENV", "IFS":
		return true
	}
	return strings.HasPrefix(key, "LD_")
}

// writeBootstrapFile decodes and writes one bootstrap file, creating any
// missing parent directories and chowning both the file and any directories
// this call created to the scion user.
//
// A *bootstrapPathError return means the file's Path itself is the problem
// (empty/relative, a non-directory component, or a symlink somewhere in its
// ancestry) — handleBootstrap maps that to HTTP 422 with the error's stable
// Code. Any other error is a generic write failure and stays a 500.
func (s *Server) writeBootstrapFile(f BootstrapFile) error {
	if f.Path == "" || !filepath.IsAbs(f.Path) {
		return &bootstrapPathError{code: codeBootstrapPathInvalid, path: f.Path, detail: errInvalidBootstrapPath.Error()}
	}
	content, err := base64.StdEncoding.DecodeString(f.ContentB64)
	if err != nil {
		return errInvalidBootstrapContent
	}

	// Clean f.Path once and use only the cleaned form from here on, for both
	// the parent-directory walk and the final write target. filepath.Dir
	// already cleans internally, so a ".." in f.Path never survives into
	// dir — but if the raw, uncleaned f.Path were still passed to the final
	// write below, a ".." component resolves at syscall time against
	// whatever is actually on disk there, which the kernel does not
	// necessarily agree lexically with: if an earlier component names a
	// symlink, ".." after it walks back from the link's *target*, not from
	// dir. That would let a path like /home/scion/a/../b/file resolve
	// somewhere mkdirAllTracked never checked, even though dir looks like a
	// plain, already-validated /home/scion/b. Cleaning first removes the
	// ".." lexically before any syscall sees it, so the directory
	// mkdirAllTracked walks and the path the file is ultimately written to
	// name exactly the same components.
	path := filepath.Clean(f.Path)

	// Enforced-mode redirect: a path under $HOME/.scion/hooks/ (the scion
	// home as bootstrap knows it) is delivered under the dedicated,
	// root-owned hooks.EnforcedHooksDir instead, and must never be chowned
	// to the workload the way every other bootstrap-written file is below —
	// see redirectEnforcedHooksPath's own doc comment for why. This must run
	// on the CLEANED path: matching before Clean would let a ".." or a
	// doubled separator dodge or spoof the prefix check.
	chownUID, chownGID := s.chownUID, s.chownGID
	if redirected, ok := redirectEnforcedHooksPath(path); ok {
		path = redirected
		chownUID, chownGID = -1, -1
	} else if !isWithinAgentHome(path) {
		// Every other bootstrap file target must resolve inside the agent
		// home: a root-written, workload-chowned file at an arbitrary
		// absolute path (e.g. "/etc/ld.so.preload" paired with a
		// workload-writable ".so" it names) is a root code-execution
		// primitive, not a legitimate bootstrap target. Fails closed before
		// any directory is created or anything is written — never root-
		// written, even transiently. The error names only the target's own
		// leaf component, never the full path or content.
		return &bootstrapPathError{code: codeBootstrapPathOutsideHome, path: filepath.Base(path), detail: "target does not resolve inside the agent home"}
	}

	dir := filepath.Dir(path)
	created, err := mkdirAllTracked(dir, 0o755)
	if err != nil {
		var symErr *errSymlinkComponent
		if errors.As(err, &symErr) {
			// Reject the file; name only its own Path, never the internal
			// ancestor mkdirAllTracked found the symlink at (still just a
			// path, but there's no reason to expose more than the caller
			// already gave us) or any content. See errSymlinkComponent's
			// doc comment for the threat this closes.
			return &bootstrapPathError{code: codeBootstrapPathSymlink, path: f.Path, detail: "path traverses a symlink"}
		}
		var nonDirErr *errNonDirComponent
		if errors.As(err, &nonDirErr) {
			return &bootstrapPathError{code: codeBootstrapPathInvalid, path: f.Path, detail: "a path component exists and is not a directory"}
		}
		return err
	}

	// Mask to the permission bits only: f.Mode is caller-supplied (the
	// bootstrap request body), and passing it through unmasked would let a
	// caller set setuid/setgid/sticky bits (or any other bits a future
	// os.FileMode superset might define) on a file this process, running
	// as root, is about to chown to the workload uid — a setuid-root file
	// planted this way would survive as a privilege-escalation path long
	// after bootstrap completes.
	mode := os.FileMode(f.Mode) & 0o777
	if mode == 0 {
		mode = defaultFileMode
	}
	// The leaf component itself — path's final element — is never Lstat'd or
	// otherwise checked here for being a symlink, unlike every component of
	// dir above. That is intentional, not an oversight: writeFileAtomicMode
	// below never opens path directly, only os.Rename(tmp, path), and
	// rename(2) replaces whatever directory entry currently sits at path —
	// including a symlink — rather than following it. So a pre-existing
	// symlink at the leaf is safe by construction: it is atomically replaced
	// by a new regular file, and whatever it used to point at is never
	// written through and is left untouched.
	if err := writeFileAtomicMode(dir, path, content, mode, chownUID, chownGID); err != nil {
		return err
	}

	if chownUID >= 0 && chownGID >= 0 {
		for _, d := range created {
			_ = os.Chown(d, chownUID, chownGID)
		}
	}
	return nil
}

// writeFileAtomicMode writes content to path without ever exposing it, even
// transiently, at a mode wider than requested. A naive
// os.WriteFile(path, content, mode) followed by os.Chmod(path, mode) has a
// real window between those two syscalls where a pre-existing file at path
// (e.g. one baked into the image at a looser mode, like 0644) holds the new
// secret content at its *old* mode. Anything with read access under that
// old mode can read the secret during the window.
//
// Instead: create a private temp file (os.CreateTemp defaults to 0600) in
// the same directory as path (so the final rename lands on the same
// filesystem and is therefore atomic — cross-filesystem renames are not),
// fchmod and fchown it to the final target mode/owner *before* writing any
// content, then rename it over path. By the time the content touches disk
// the file already has its final permissions; the rename is atomic, so
// there is never an instant where path exists with the new content under
// the wrong mode or owner — including path not existing yet at all.
//
// The fchown call failing is fatal (the caller aborts the whole bootstrap),
// not best-effort: this assumes substrate-serve runs as root in the actor,
// so chown(2) to the target scion uid/gid should always succeed, and a
// failure signals something genuinely wrong (a read-only or foreign
// filesystem, an unexpected capability drop) rather than an expected
// permission boundary. If substrate-serve ever runs as a non-root user
// while a "scion" target uid/gid still exists, every bootstrap would fail
// here with EPERM — chown(2) to an arbitrary uid/gid is root-only on Linux,
// with no equivalent of file-owner-can-chgrp-to-own-groups.
func writeFileAtomicMode(dir, path string, content []byte, mode os.FileMode, uid, gid int) (err error) {
	tmp, err := os.CreateTemp(dir, ".bootstrap-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Only clean up the temp file on failure: on success it has already
	// been renamed to path, so tmpPath no longer refers to anything.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if uid >= 0 && gid >= 0 {
		if err = tmp.Chown(uid, gid); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if _, err = tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token, ok := bearerToken(r)
	s.mu.Lock()
	expected := s.controlToken
	bootstrapped := s.bootstrapped
	s.mu.Unlock()

	if !bootstrapped || expected == "" || !ok ||
		subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ExecRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxExecBodyBytes+1))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Argv) == 0 {
		http.Error(w, "argv must not be empty", http.StatusBadRequest)
		return
	}

	execUser := req.User
	if execUser == "" {
		execUser = "scion"
	}
	if execUser != "scion" {
		// root is refused server-side: /exec exists to run the agent
		// workload, which always runs as the unprivileged scion user, and
		// this control server already runs as root itself to perform the
		// drop — there is no legitimate caller need for a root-level
		// command here, only additional blast radius if a caller or this
		// handler is ever compromised.
		http.Error(w, `user must be "scion"`, http.StatusBadRequest)
		return
	}

	timeout := clampExecTimeout(req.TimeoutS)

	resp := runExec(r.Context(), execUser, req.Argv, req.Stdin, timeout)
	resp.StdinSupported = true
	writeJSON(w, http.StatusOK, resp)
}

// maxExecTimeoutSeconds is maxExecTimeout expressed in whole seconds, the
// same unit as ExecRequest.TimeoutS, so clampExecTimeout can bound the
// caller-supplied value *before* multiplying it by time.Second.
const maxExecTimeoutSeconds = int(maxExecTimeout / time.Second)

// clampExecTimeout converts a caller-supplied ExecRequest.TimeoutS into a
// bounded time.Duration: timeoutS <= 0 means "unspecified", using
// defaultExecTimeout; otherwise the result is timeoutS seconds, clamped to
// maxExecTimeout.
//
// The clamp is a comparison against maxExecTimeoutSeconds, in seconds,
// strictly before any multiplication by time.Second: timeoutS is
// caller-supplied and, multiplied first, can overflow time.Duration's int64
// range (e.g. a TimeoutS near math.MaxInt64) and wrap into a small or
// negative value. Left unguarded, a wrapped-negative duration would fail
// runExec's "timeout > 0" check and silently run with NO timeout at all —
// removing the maxExecTimeout cap entirely rather than clamping to it.
func clampExecTimeout(timeoutS int) time.Duration {
	if timeoutS <= 0 {
		return defaultExecTimeout
	}
	if timeoutS > maxExecTimeoutSeconds {
		return maxExecTimeout
	}
	return time.Duration(timeoutS) * time.Second
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. ok is false if the header is missing or malformed.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	if token == "" {
		return "", false
	}
	return token, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
