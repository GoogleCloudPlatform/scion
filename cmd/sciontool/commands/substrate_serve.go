/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/substrate"
)

var substrateServeAddr string

// skipRootfsFixupEnv, when set to any non-empty value, skips the rootfs
// fixup at both of its call sites (runSubstrateServe's startup call and
// substrateServeRootfsFixup's /bootstrap fallback) — never the
// privilege-drop precondition (substrateServePrivilegeDropChecker /
// checkPrivilegeDropFeasible), which is wired into the Server independently
// of this env var and always re-checks the same traversability/ownership
// conditions the fixup would have corrected. Skipping the fixup can
// therefore only make bootstrap fail closed sooner, never bypass the check.
// A real actor never has a reason to set it — see each call site for why it
// exists.
const skipRootfsFixupEnv = "SCION_SUBSTRATE_TEST_SKIP_ROOTFS_FIXUP"

// rootfsFixupSkipped reports whether skipRootfsFixupEnv is set, for both
// rootfs fixup call sites to check.
func rootfsFixupSkipped() bool {
	return os.Getenv(skipRootfsFixupEnv) != ""
}

// substrateServeCmd is the template entrypoint for the `substrate` runtime
// (substrate-runtime.md §5). It is compiled into the same sciontool binary as
// every other subcommand, so no image-build change is needed beyond what
// already builds `./cmd/sciontool/`.
var substrateServeCmd = &cobra.Command{
	Use:   "substrate-serve",
	Short: "Run the Substrate actor control server",
	Long: `substrate-serve runs sciontool as PID 1 inside a Substrate actor. It
listens on the port the inbound router targets by default (:80) and serves:

  GET  /scion/v1/healthz    liveness + bootstrap state ("awaiting-bootstrap"
                            or "running"); no auth
  POST /scion/v1/bootstrap  one-shot per-agent config (env, files, start
                            command); runs the existing "sciontool init"
                            path in-process once accepted
  POST /scion/v1/exec       authenticated exec, used for message delivery
                            and other broker-side operations

/pty, /rehydrate and /tunnel/open are not currently supported.

Known limitation: substrate-serve logs SIGTERM but does not forward
it to the harness and does not exit. Full eviction handling — suspending the
actor within the worker's 30-minute grace period instead of just surviving
the signal — is future work (see .design docs: substrate-runtime.md §11).`,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(runSubstrateServe(substrateServeAddr))
	},
}

func init() {
	rootCmd.AddCommand(substrateServeCmd)
	substrateServeCmd.Flags().StringVar(&substrateServeAddr, "addr", ":80",
		"Address to listen on (the router targets :80 by default; overridable for tests)")
}

// substrateServeInitOptions returns the InitRunOptions substrate-serve's
// InitRunner passes to RunInit for one child invocation. Unlike a plain
// static WorkingDir, ResolveWorkingDir defers resolution to RunInit itself,
// which calls it only after the workspace has actually been prepared (see
// InitRunOptions.ResolveWorkingDir's doc comment) — calling
// resolveSubstrateHarnessCwd here instead, before RunInit ever runs, would
// see the workspace in whatever state the broker's bind mount left it in,
// not the state gitCloneWorkspace and the pre-start-hook ownership fixup
// leave it in.
func substrateServeInitOptions(forwardTermSignal bool) InitRunOptions {
	return InitRunOptions{
		DisableTermSignalForwarding: !forwardTermSignal,
		// RequirePrivilegeDrop: true — substrate always starts the actor as
		// UID 0, so a failed/skipped privilege drop can only mean "still
		// root," never a legitimate rootless outcome (see
		// InitRunOptions.RequirePrivilegeDrop).
		RequirePrivilegeDrop: true,
		// DisablePortForwarding: true — Substrate's egress is HTTP(S)-only
		// and default-deny (see InitRunOptions.DisablePortForwarding).
		DisablePortForwarding: true,
		// DisableReExec: true — RunInit runs embedded in substrate-serve's
		// PID 1, which receives its secrets in-process (bootstrap body plus
		// os.Setenv), never through its own execve environment; re-execing
		// would replace this bootstrapped PID 1 with a fresh, unbootstrapped
		// one (see InitRunOptions.DisableReExec).
		DisableReExec:     true,
		ResolveWorkingDir: substrateResolveHarnessWorkingDir,
	}
}

// substrateResolveHarnessWorkingDir is the InitRunOptions.ResolveWorkingDir
// RunInit calls for a substrate-serve-driven init run. It resolves against
// the real process environment, filesystem, and "scion" user
// (defaultSubstrateHarnessCwdDeps) — never a test's fakes, since production
// always reaches this function through substrateServeInitOptions above.
func substrateResolveHarnessWorkingDir() (string, error) {
	workingDir, err := resolveSubstrateHarnessCwd(defaultSubstrateHarnessCwdDeps)
	if err != nil {
		return "", err
	}
	// One line per start, quoting only the path: a later chdir failure
	// (e.g. a TOCTOU race) surfaces as a bare "permission denied" that
	// doesn't name the directory, so this is what makes that diagnosable.
	// Emitted here, at resolution time (after RunInit has prepared the
	// workspace), rather than when substrateServeInitOptions builds the
	// InitRunOptions — the value logged is the one actually resolved.
	log.Info("substrate-serve: harness working directory %q", workingDir)
	return workingDir, nil
}

// substrateHarnessCwdDeps groups resolveSubstrateHarnessCwd's external
// dependencies so tests can substitute them — the same reasoning as
// privilegeDropPreconditionDeps: no test should depend on this machine's
// real SCION_WORKSPACE_PATH, filesystem, or "scion" user.
type substrateHarnessCwdDeps struct {
	getenv func(string) string
	stat   func(string) (os.FileInfo, error)
	// evalSymlinks resolves a candidate to its target, the same way
	// dirUsableForScion needs to in order to check the target's own
	// ancestors (stat alone follows the final symlink but says nothing
	// about what's above it) — see dirUsableForScion's doc comment.
	evalSymlinks func(string) (string, error)
	lookupUser   func(string) (*user.User, error)
}

// defaultSubstrateHarnessCwdDeps wires resolveSubstrateHarnessCwd to the
// real process environment, filesystem, and "scion" user.
var defaultSubstrateHarnessCwdDeps = substrateHarnessCwdDeps{
	getenv:       os.Getenv,
	stat:         os.Stat,
	evalSymlinks: filepath.EvalSymlinks,
	// Wraps the scionUserLookup var in a closure, not its current value, for
	// the same reason as defaultPrivilegeDropPreconditionDeps.lookupUser.
	lookupUser: func(username string) (*user.User, error) { return scionUserLookup(username) },
}

// resolveSubstrateHarnessCwd picks the working directory the substrate
// harness child (and, via tmux's own cwd inheritance, its tmux session too —
// see the "agent"/"shell" window reasoning in the project log) should start
// in, mirroring the image's WORKDIR that ateom does not apply under
// Substrate (see InitRunOptions.WorkingDir). Called via
// substrateResolveHarnessWorkingDir, RunInit's InitRunOptions.ResolveWorkingDir
// hook for a substrate-serve-driven run — which RunInit invokes only after
// the workspace has been cloned and the post-pre-start-hook ownership fixup
// has run (see that field's doc comment, init.go) — so SCION_WORKSPACE_PATH
// below is checked in the state those two steps leave it in, not the state
// the broker's bind mount left it in beforehand.
//
// supervisor.Run's chdir happens via SysProcAttr.Credential AFTER the
// privilege drop to the scion uid/gid, not before, so a candidate that a
// root-only stat approves can still make the child fail to start (EACCES)
// or silently inherit substrate-serve's own cwd. Every candidate below is
// therefore verified searchable by the scion uid/gid specifically,
// including its full ancestor chain, via canSearchDir — never by trusting a
// stat this (root) process could make on its own.
//
// Resolution order:
//  1. SCION_WORKSPACE_PATH (default "/workspace"; rejected if set but not
//     absolute) if it and every ancestor directory are searchable by the
//     scion uid/gid.
//  2. The scion user's own home directory (lookupUser("scion").HomeDir —
//     normally equal to the HOME supervisor.Run sets, though supervisor
//     derives that value independently as "/home/"+Username rather than
//     from this same lookup), under the same check. This package never
//     reads substrate-serve's own $HOME.
//  3. Neither usable: an error naming every candidate tried (quoted path
//     and reason) and the uid they were checked for — nothing else. This
//     never returns "/" and never leaves cmd.Dir to inherit this process's
//     own cwd.
//
// One log line is emitted whenever a candidate is rejected, quoting only
// the path.
func resolveSubstrateHarnessCwd(d substrateHarnessCwdDeps) (string, error) {
	scionUser, err := d.lookupUser("scion")
	if err != nil {
		return "", fmt.Errorf("substrate: cannot resolve the scion user for the harness working directory: %w", err)
	}
	uid64, uidErr := strconv.ParseUint(scionUser.Uid, 10, 32)
	gid64, gidErr := strconv.ParseUint(scionUser.Gid, 10, 32)
	if uidErr != nil || gidErr != nil {
		return "", fmt.Errorf("substrate: scion user has an unparseable uid/gid")
	}
	uid, gid := uint32(uid64), uint32(gid64)

	// chosen holds tryCandidate's own filepath.Clean of whichever candidate
	// passed, so the canonical (but still logical — see dirUsableForScion's
	// doc comment on symlinks) spelling is what gets returned and, later,
	// what PWD carries — never the raw, possibly non-canonical input.
	var tried []string
	var chosen string
	tryCandidate := func(path string) bool {
		clean := filepath.Clean(path)
		ok, reason := dirUsableForScion(d, clean, uid, gid)
		if ok {
			chosen = clean
			return true
		}
		log.Info("substrate-serve: harness working directory candidate %q is not usable (%s)", clean, reason)
		tried = append(tried, fmt.Sprintf("%q (%s)", clean, reason))
		return false
	}

	workspace := d.getenv("SCION_WORKSPACE_PATH")
	switch {
	case workspace == "":
		workspace = "/workspace"
	case !filepath.IsAbs(workspace):
		log.Info("substrate-serve: SCION_WORKSPACE_PATH %q is not an absolute path", workspace)
		tried = append(tried, fmt.Sprintf("%q (not absolute)", workspace))
		workspace = ""
	}
	if workspace != "" && tryCandidate(workspace) {
		return chosen, nil
	}

	if home := scionUser.HomeDir; tryCandidate(home) {
		return chosen, nil
	}

	return "", fmt.Errorf("substrate: no usable harness working directory for uid %d: tried %s", uid, strings.Join(tried, ", "))
}

// dirUsableForScion reports whether candidate and every ancestor directory
// up to "/" exist, are directories, and are searchable (execute bit) by
// uid/gid — the exact traversal a chdir(candidate) needs to succeed as that
// uid. candidate is filepath.Clean'd first, so a non-canonical spelling
// (e.g. "/.", "//", or "/tmp/..") can't slip past the "never '/'" guard —
// parentDirs already cleans its own output, so an uncleaned candidate could
// reach that guard already reduced to "/" and pass it. The cleaned value is
// what dirsSearchable is walked against; candidate itself is never "/"
// after cleaning.
//
// candidate must also be absolute. resolveSubstrateHarnessCwd's own switch
// already rejects a non-absolute SCION_WORKSPACE_PATH before ever calling
// here, but scionUser.HomeDir has no such upstream check, and
// filepath.Clean("") == "." (a stdlib quirk, not a filesystem fact) — so an
// /etc/passwd entry with an empty or otherwise relative home directory would
// otherwise reach here as a relative candidate that "candidate == '/'"
// doesn't catch. A relative cmd.Dir is resolved by the kernel against
// substrate-serve's OWN process cwd at chdir time (typically "/" for a
// container's PID 1 before any WORKDIR is applied), so this is a second
// "never '/'" vector, distinct from a literal "/" or a symlink resolving to
// it, and closed here at the same choke point.
//
// stat(dir) follows the final symlink in dir, but says nothing about a
// symlink's target's own ancestors — a candidate that is itself a symlink
// (e.g. "/workspace" -> "/data/ws") can pass the lexical walk above while
// still being unreachable if "/data" isn't searchable, since chdir has to
// traverse the resolved path too. So, separately, the candidate is resolved
// with EvalSymlinks and — only when that changes anything — the resolved
// path's own ancestor chain is walked the same way. An EvalSymlinks error
// (a broken symlink, a cycle, ...) makes the candidate unusable outright,
// with that error as the reason. A resolved target of exactly "/" is
// rejected outright too, for the same "never '/'" reason as the lexical
// guard above — "/" is always searchable, so it would otherwise sail
// through the resolved-chain walk below. Either way, the *candidate* (never
// the resolved path) is what the caller returns, so PWD/cmd.Dir stay
// logical.
func dirUsableForScion(d substrateHarnessCwdDeps, candidate string, uid, gid uint32) (ok bool, reason string) {
	candidate = filepath.Clean(candidate)
	if !filepath.IsAbs(candidate) {
		return false, "not absolute"
	}
	if candidate == "/" {
		return false, "refusing to use the root directory"
	}
	if ok, reason := dirsSearchable(d, append(parentDirs(candidate), candidate), uid, gid); !ok {
		return false, reason
	}

	real, err := d.evalSymlinks(candidate)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve symlinks: %q", err.Error())
	}
	// A candidate that is itself fine lexically (never "/", per the guard
	// above) can still be a symlink chain that resolves to "/" — e.g.
	// SCION_WORKSPACE_PATH or the scion HomeDir pointing at a bind mount
	// that itself symlinks to "/". "/" is always searchable by everyone, so
	// without this check dirsSearchable below would happily approve it, and
	// the caller would return the logical candidate while its EFFECTIVE cwd
	// (what chdir/PWD would actually resolve through) is "/" — exactly the
	// "never /" constraint this whole resolver exists to uphold.
	if real == "/" {
		return false, "resolves to /"
	}
	if real != candidate {
		if ok, reason := dirsSearchable(d, append(parentDirs(real), real), uid, gid); !ok {
			return false, reason
		}
	}
	return true, ""
}

// dirsSearchable reports whether every directory in dirs exists, is a
// directory, and is searchable (execute bit) by uid/gid — the shared walk
// dirUsableForScion runs once for candidate's own lexical ancestor chain
// and, when it differs, again for its resolved (symlink target) chain.
func dirsSearchable(d substrateHarnessCwdDeps, dirs []string, uid, gid uint32) (ok bool, reason string) {
	for _, dir := range dirs {
		info, err := d.stat(dir)
		if err != nil {
			return false, "missing"
		}
		if !info.IsDir() {
			return false, "not a directory"
		}
		if !canSearchDir(info, uid, gid) {
			return false, "not searchable"
		}
	}
	return true, ""
}

// substrateServePrivilegeDropChecker is the substrate.PrivilegeDropChecker
// substrate-serve wires into its Server (see checkPrivilegeDropFeasible's
// doc comment for what it actually checks).
func substrateServePrivilegeDropChecker() error {
	return checkPrivilegeDropFeasible(defaultPrivilegeDropPreconditionDeps)
}

// substrateServeRootfsFixup is the substrate.RootfsFixup substrate-serve
// wires into its Server as call site 2 (the /bootstrap fallback — see
// fixupRootfsForScion's doc comment for call site 1, substrate-serve's own
// startup, which is the primary one). Gated on skipRootfsFixupEnv the same
// way call site 1 is; see that const's doc comment for why this never
// weakens the separately-wired privilege-drop precondition.
func substrateServeRootfsFixup() {
	if rootfsFixupSkipped() {
		return
	}
	bootstrapRootfsFixup("/")
}

// verifySelfBinaryRootOwned verifies that the binary actually running as
// this process — resolved via "/proc/self/exe", the kernel's own magic
// symlink to the running inode (rootexec.SelfExe; see its own doc comment
// for why this, and not os.Executable(), is the right way to name "the
// binary that is really running") — is a trusted, root-owned regular file:
// every real directory in its resolved path, and the file itself, owned by
// uid 0 and free of the group- and other-write bits. This is the identical
// fd-walk check rootexec.Resolve applies to any bare command name it looks
// up (dirfd.OpenNoFollowRootOwnedFile), reused here to verify PID 1's own
// on-disk identity instead of an external tool's.
//
// Run once, before this process ever reports healthy: a substrate actor
// whose PID 1 is somehow reached through a workload-writable path — rather
// than this build's own absolute, image-installed
// "/usr/local/bin/sciontool" (see buildActorTemplate's Command field,
// pkg/runtime/substrate_template.go) — refuses to start at all instead of
// serving traffic, and possibly a broker's authenticated requests, from an
// unverified binary.
func verifySelfBinaryRootOwned() error {
	link, err := os.Readlink(rootexec.SelfExe())
	if err != nil {
		return fmt.Errorf("resolve running executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return fmt.Errorf("resolve running executable's own symlink chain: %w", err)
	}
	f, err := dirfd.OpenNoFollowRootOwnedFile(resolved)
	if err != nil {
		return fmt.Errorf("running executable %s is not a trusted, root-owned binary: %w", resolved, err)
	}
	defer func() { _ = f.Close() }()

	// The two steps above verify a PATH — readlink's own on-disk string,
	// re-resolved a second time — not the in-memory image actually
	// running. Opening "/proc/self/exe" itself (rather than a path derived
	// from it) always names the exact running inode: it's a kernel magic
	// symlink resolved against this process's own in-kernel reference, not
	// a second userspace path lookup, so it can't be redirected by
	// anything that happened to the on-disk path between the two steps
	// above. Comparing its inode against the just-verified file's binds
	// this whole check to the binary that is actually running, not merely
	// to whatever a fresh lookup of the same name finds right now.
	running, err := os.Open(rootexec.SelfExe())
	if err != nil {
		return fmt.Errorf("open running executable: %w", err)
	}
	defer func() { _ = running.Close() }()
	return sameInode(running, f)
}

// sameInode reports an error unless a and b are open file descriptors on
// the identical underlying inode (matching device and inode number, the
// only identity a filesystem actually guarantees — two different paths, or
// even the same path resolved twice, can otherwise land on different
// files).
func sameInode(a, b *os.File) error {
	var sa, sb syscall.Stat_t
	if err := syscall.Fstat(int(a.Fd()), &sa); err != nil {
		return fmt.Errorf("fstat %s: %w", a.Name(), err)
	}
	if err := syscall.Fstat(int(b.Fd()), &sb); err != nil {
		return fmt.Errorf("fstat %s: %w", b.Name(), err)
	}
	if sa.Dev != sb.Dev || sa.Ino != sb.Ino {
		return fmt.Errorf("%s (dev=%d ino=%d) is not the same file as %s (dev=%d ino=%d)",
			a.Name(), sa.Dev, sa.Ino, b.Name(), sb.Dev, sb.Ino)
	}
	return nil
}

// runSelfBinaryIntegrityCheck is runSubstrateServe's own call to
// verifySelfBinaryRootOwned, as a package var for the same reason as
// startupRootfsFixup: a test driving runSubstrateServe in-process runs as
// the `go test` binary itself — owned by whichever uid built it, not root,
// and never at the path this actor's own Command names — so the real check
// would fail for a reason that has nothing to do with what the test
// actually exercises. Gated by the same rootfsFixupSkipped() condition as
// startupRootfsFixup below, not a separate env var: skipRootfsFixupEnv's
// own contract ("a real actor never has a reason to set it") applies
// identically here, and this check is meaningless against a binary that
// isn't really this actor's own PID-1 image in the first place.
var runSelfBinaryIntegrityCheck = verifySelfBinaryRootOwned

// startupRootfsFixup is call site 1's own call, as a package var — the same
// reason as startReaper: a test driving runSubstrateServe needs to observe
// (and assert the ordering of) this call without it resolving the real
// "scion" user or touching a real rootfs.
var startupRootfsFixup = fixupRootfsForScionUser

// bootstrapRootfsFixup is call site 2's own call, as a package var for the
// same reason as startupRootfsFixup: a test driving substrateServeRootfsFixup
// needs to observe whether it ran without resolving the real "scion" user or
// touching a real rootfs.
var bootstrapRootfsFixup = fixupRootfsForScionUser

// substrateServeInitRunner builds the substrate.InitRunner that delegates to
// runInit, passing along the InitRunOptions substrateServeInitOptions built
// (including its ResolveWorkingDir closure). Extracted from
// newSubstrateServeServer so a test can drive it directly without standing
// up a Server.
//
// This wrapper never resolves the harness working directory itself and
// never short-circuits runInit: it always delegates, and it is RunInit —
// after preparing the workspace — that calls ResolveWorkingDir and fails
// closed with exitCodeNoUsableHarnessCwd (never invoking the harness) if
// that errors. See
// InitRunOptions.ResolveWorkingDir's doc comment (init.go) for why that
// placement matters and StateInitFailed's doc comment (pkg/sciontool/
// substrate) for why RunInit's own exit code is not otherwise acted on
// here: substrate-serve does not exit the process on a non-zero init.
func substrateServeInitRunner(runInit func(argv []string, opts InitRunOptions) int) substrate.InitRunner {
	return func(argv []string, forwardTermSignal bool) int {
		return runInit(argv, substrateServeInitOptions(forwardTermSignal))
	}
}

// newSubstrateServeServer builds the *substrate.Server substrate-serve
// mounts, wiring both the init runner and the privilege-drop precondition
// (see PrivilegeDropChecker's doc comment). Extracted so a test can drive
// the exact same wiring runSubstrateServe uses — including a missing or
// disabled precondition regressing back to silently accepting the request —
// without starting an HTTP listener.
//
// runInit is a parameter, not the real RunInit called directly, precisely
// so a test exercising this wiring can never reach the real RunInit. A
// test that stubs it and then removes WithPrivilegeDropChecker (the
// regression this wiring exists to catch) must see its stub called and
// fail on that assertion — not have the real RunInit write this machine's
// real agent-info.json. Calling the real RunInit directly here would let
// bootstrap wrongly returning 200 under that regression drive the real
// RunInit for real, in-process, where no test assertion could see it.
func newSubstrateServeServer(runInit func(argv []string, opts InitRunOptions) int) *substrate.Server {
	return substrate.NewServer(
		substrate.WithPrivilegeDropChecker(substrateServePrivilegeDropChecker),
		substrate.WithRootfsFixup(substrateServeRootfsFixup),
		substrate.WithInitRunner(substrateServeInitRunner(runInit)),
	)
}

// newSubstrateHTTPServer builds the *http.Server substrate-serve listens
// with. Extracted so a test can assert the timeout wiring directly, without
// starting a real listener.
//
// ReadHeaderTimeout/ReadTimeout/IdleTimeout guard against a client that
// opens a connection and then trickles bytes (or none at all) — without
// them, http.Server has no bound on how long it will hold a connection open
// waiting on a slow or stalled peer, letting a handful of such connections
// exhaust the actor's file descriptors. Every route (healthz has no body;
// bootstrap and exec both fully decode a size-bounded body via
// http.MaxBytesReader/io.LimitReader before doing any work —
// pkg/sciontool/substrate/server.go) reads its whole request before
// responding, so ReadTimeout (which bounds header-through-body) cannot cut
// off a route that is still streaming a request body to the handler as it
// arrives.
//
// WriteTimeout is deliberately NOT set: handleExec runs the requested
// command (bounded by its own timeout_s, up to maxExecTimeout = 10 minutes —
// pkg/sciontool/substrate/exec.go) before writing any response, so a
// WriteTimeout shorter than that would sever a legitimate long-running
// exec's response after the command already completed; no single
// WriteTimeout fits every route.
func newSubstrateHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func runSubstrateServe(addr string) int {
	// substrate-serve is PID 1 inside the actor: reap reparented zombies the
	// same way `sciontool init` does. RunInit (invoked after bootstrap)
	// starts its own reaper too; a second startReaper call is harmless
	// (each independently drains SIGCHLD via WNOHANG) and this one covers
	// the awaiting-bootstrap window before RunInit ever runs. See
	// startReaper's own doc comment (init.go) for why this is a package var
	// rather than calling supervisor.StartReaper directly.
	startReaper()

	// Call site 1 (primary): fix up the rootfs before /healthz can ever
	// report ready. This runs during the golden boot, so the corrected
	// rootfs is captured in the snapshot and a restored actor never redoes
	// the copy-up of the whole home tree — doing this only in /bootstrap
	// would copy up the entire home (including harness installs) on every
	// actor start and hurt warm start. See fixupRootfsForScion's doc
	// comment for what it actually fixes and why.
	//
	// rootfsFixupSkipped is checked here, not left implicit: this call site
	// runs against a real, unscrubbed "/" and real "scion" home whenever
	// this binary is actually exec'd rather than driven in-process by a
	// test (e.g. TestSubstrateServeCommand_Integration_SIGTERMNotForwarded's
	// real subprocess) — no test-binary TestMain sandboxing reaches a real
	// exec'd child's own process. There is no legitimate reason for a real
	// actor to ever set skipRootfsFixupEnv: see its own doc comment for why
	// this is safe to skip (the precondition below is unaffected). Not read
	// through the injectable startupRootfsFixup var: this check is about
	// whether to call it at all, which a test replacing that var already
	// controls directly.
	if !rootfsFixupSkipped() {
		startupRootfsFixup("/")

		// Self-binary integrity check: before /healthz can ever report
		// ready, verify PID 1's own running image is a trusted, root-owned
		// binary. See verifySelfBinaryRootOwned's own doc comment for what
		// this catches and why it fails closed rather than starting the
		// HTTP server at all.
		if err := runSelfBinaryIntegrityCheck(); err != nil {
			log.Error("substrate-serve: self-binary integrity check failed: %v", err)
			return 1
		}
	}

	srv := newSubstrateServeServer(RunInit)
	httpServer := newSubstrateHTTPServer(addr, srv.Handler())

	// substrate-runtime.md §5.6: log SIGTERM and keep running. Do not forward
	// it to the harness and do not exit — an evicting worker sends SIGTERM
	// with a 30-minute grace period before SIGKILL, and killing the harness
	// immediately would turn a recoverable eviction into a lost agent. The
	// broker is expected to react to the eviction notice (reported to the
	// Hub by RunInit's own state reporting) and drive a real suspend/resume
	// as future work; today this only guarantees survival of the signal
	// itself.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM)
	// signal.Stop followed by close lets the goroutine below exit via its
	// range loop on any return path, instead of leaking a goroutine parked
	// on sigChan for the life of the process on every call. See the
	// project log for why this matters even though production only ever
	// calls this once.
	defer func() {
		signal.Stop(sigChan)
		close(sigChan)
	}()
	go func() {
		for sig := range sigChan {
			log.Info("substrate-serve: received %s; not forwarding to harness (known limitation; full eviction handling is future work)", sig)
		}
	}()

	log.Info("substrate-serve listening on %s", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("substrate-serve: ListenAndServe failed: %v", err)
		return 1
	}
	return 0
}
