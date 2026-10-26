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

package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Host-side git invocations.
//
// The hub and the broker run git on the host in repositories that agent
// containers can also write: a linked checkout whose .git is mounted into
// worktree-per-agent containers, the shared checkout of a hub-native
// worktree-per-agent project, and the shared clone of a shared-workspace
// project. Git reads behaviour from the repository's own config and hooks,
// so these invocations run without repository hooks and without
// repository-defined commands:
//
//   - HostGitArgs is the fixed set of -c settings every host-side call
//     carries (hooks, fsmonitor, pager, the ext transport, submodule
//     recursion, merge signature checks).
//   - HostGitEnv drops GIT_* variables that choose a repository, add config
//     or name programs from the environment git runs with.
//   - HostGitCommand adds, for commands that write the work tree or talk to
//     a remote, settings derived from the repository's own config: filter
//     drivers defined there are replaced with the global/system definition
//     (or disabled), and for remote operations a repository config with keys
//     outside a small allowlist is refused. It checks again after the
//     command that the repository config it depended on did not change.
//
// Global and system config are the host user's and stay in effect: git-lfs
// installs its filter there, so LFS content is still checked out. Command
// line settings (-c) take precedence over every config file, including
// files pulled in with include.path, which is why pinning works without
// rewriting the repository config.
//
// The repository config these commands read can still be written by agents
// on runtimes where it is mounted read-write. The check after each command
// detects a lasting change; keeping agents from writing the repository's git
// config and hooks at all is done by the runtime mounts.

// HostGitArgs returns the -c settings every host-side git invocation in a
// repository that agents can write carries, ahead of the subcommand:
//
//   - core.hooksPath=/dev/null: no hook runs (post-checkout,
//     reference-transaction, post-merge, ...). Hooks are repository content
//     here, not host configuration.
//   - core.fsmonitor=false: no file-system monitor command runs when git
//     reads the index.
//   - core.pager=cat: never starts a configured pager.
//   - protocol.ext.allow=never: the ext:: transport, which runs a command,
//     is never used, whatever a config file allows.
//   - submodule.recurse=false, fetch.recurseSubmodules=false: git never
//     works inside submodules, whose own config (.git/modules/<name>) is not
//     part of the checks here, and never fetches from submodule URLs.
//   - merge.verifySignatures=false, commit.gpgsign=false, tag.gpgsign=false:
//     no git invocation runs gpg.program to verify or make a signature.
//   - protocol.file.allow=user: the file:// and local-path transports are
//     allowed for a top-level operation the user drives directly (git treats
//     those as GIT_PROTOCOL_FROM_USER=1), which includes a hub clone URL that is
//     a local path, but are blocked for git's own indirect uses such as
//     submodule fetches. It is not restricted to command-line URLs: a top-level
//     fetch of a configured file:// remote is still allowed.
func HostGitArgs() []string {
	return []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.pager=cat",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.file.allow=user",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "merge.verifySignatures=false",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}
}

// hostGitNetworkArgs are added to HostGitArgs for a remote operation: a
// transport allow-list that denies every protocol except the ones a hub
// fetch legitimately uses, so no repository or environment setting can
// re-enable, say, the ext or a custom helper transport. file is not listed
// here; HostGitArgs' protocol.file.allow=user already permits a local-path
// clone URL given on the command line while refusing a config-sourced one.
var hostGitNetworkArgs = []string{
	"-c", "protocol.allow=never",
	"-c", "protocol.http.allow=always",
	"-c", "protocol.https.allow=always",
	"-c", "protocol.ssh.allow=always",
	"-c", "protocol.git.allow=always",
}

// hostGitEnvKept are the GIT_* variables HostGitEnv keeps. They choose the
// host user's own config files and the transport and authentication the
// host operator set up; none of them chooses a repository or adds
// repository config.
var hostGitEnvKept = map[string]bool{
	"GIT_CONFIG_GLOBAL":   true,
	"GIT_CONFIG_SYSTEM":   true,
	"GIT_CONFIG_NOSYSTEM": true,
	"GIT_SSH":             true,
	"GIT_SSH_COMMAND":     true,
	"GIT_SSH_VARIANT":     true,
	"GIT_ASKPASS":         true,
	"GIT_TERMINAL_PROMPT": true,
	"GIT_SSL_CAINFO":      true,
	"GIT_SSL_CAPATH":      true,
	"GIT_ALLOW_PROTOCOL":  true,
	"GIT_HTTP_USER_AGENT": true,
	// Only stops repository discovery from walking above the listed
	// directories.
	"GIT_CEILING_DIRECTORIES": true,
	// Makes git treat every repository as owned by someone else (stricter
	// safe.directory checks); used by tests.
	"GIT_TEST_ASSUME_DIFFERENT_OWNER": true,
}

// HostGitEnv returns env without GIT_* variables other than hostGitEnvKept:
// no inherited GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR, GIT_INDEX_FILE,
// GIT_CONFIG_COUNT/KEY/VALUE, GIT_CONFIG_PARAMETERS, GIT_EXEC_PATH, editor or
// pager variables. Callers append the GIT_* variables they set themselves.
func HostGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") && !hostGitEnvKept[k] {
			continue
		}
		// Drop any inherited scoped-credential variable so it never reaches a host
		// git call that did not set it. ScopedCredentialHelperEnv appends it back
		// after this scrub for the one command that needs it.
		if k == scopedGitCredentialEnv {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// HostGitOptions configures HostGitCommand.
type HostGitOptions struct {
	// Config holds extra key=value settings passed as -c after HostGitArgs,
	// for example "safe.directory=<path>".
	Config []string
	// Env is the command environment; nil means HostGitEnv(os.Environ()).
	// A caller that sets it starts from HostGitEnv itself.
	Env []string
	// Network marks a command that fetches from or pushes to a remote. It
	// has no effect on the policy check (deny-by-default applies either way);
	// it only widens the before/after snapshot to every repository key.
	Network bool
	// Policy selects the deny-by-default allow-list the repository config is
	// checked against before the command runs. The zero value,
	// RepoPolicyManaged, is the strict list for a Scion-managed repository;
	// RepoPolicyLinked is the wider list for the user's own checkout.
	Policy RepoConfigPolicy
	// AllowedWorktreePath, when set, is the one path a repository-scope
	// core.worktree may equal without the command being refused.
	AllowedWorktreePath string
	// SkipLFSSmudge sets GIT_LFS_SKIP_SMUDGE=1 for the command, so a
	// host-side checkout writes git-lfs pointer files instead of resolving
	// them through the git-lfs smudge filter (which would read a tracked
	// .lfsconfig for its endpoint). Used for managed-repository checkouts;
	// agents fetch LFS objects in-container.
	SkipLFSSmudge bool
	// LFSURL, when set, pins lfs.url on the command line (which outranks a
	// tracked .lfsconfig), so a host-side checkout that does resolve LFS uses
	// a trusted endpoint. Used for linked checkouts.
	LFSURL string
}

// ErrRepoConfigChanged is returned (wrapped) when the repository config a
// host-side git command depended on changed while the command ran.
var ErrRepoConfigChanged = errors.New("the repository's git config changed while host-side git ran")

// filterDriverVars are the per-driver variables git reads for a filter
// driver (gitattributes(5)).
var filterDriverVars = []string{"clean", "smudge", "process", "required"}

// repoConfigScope reports whether a git config scope is part of the
// repository (and so writable by whoever can write the repository's .git):
// "local" is .git/config, "worktree" a worktree's config.worktree. Entries
// from included files carry the scope of the including file.
func repoConfigScope(scope string) bool {
	return scope == "local" || scope == "worktree"
}

type gitConfigEntry struct {
	scope string
	key   string
	value string
	// hasValue is false for a valueless boolean entry ("key" with no "\n"),
	// which git treats as true. It distinguishes that from an explicit empty
	// value ("key\n").
	hasValue bool
}

// parseGitConfigList parses `git config --show-scope -z --list` output:
// records "<scope>\0<key>\n<value>\0", or "<scope>\0<key>\0" for a valueless
// boolean key.
func parseGitConfigList(out []byte) []gitConfigEntry {
	fields := bytes.Split(out, []byte{0})
	var entries []gitConfigEntry
	for i := 0; i+1 < len(fields); i += 2 {
		scope := string(fields[i])
		kv := string(fields[i+1])
		key, value, hasValue := strings.Cut(kv, "\n")
		entries = append(entries, gitConfigEntry{scope: scope, key: key, value: value, hasValue: hasValue})
	}
	return entries
}

// splitConfigKey splits a config key into section, subsection and
// variable: "filter.My.Driver.smudge" is ("filter", "My.Driver", "smudge").
// The subsection is empty for a two-part key.
func splitConfigKey(key string) (section, subsection, variable string) {
	first := strings.Index(key, ".")
	last := strings.LastIndex(key, ".")
	if first < 0 {
		return key, "", ""
	}
	if first == last {
		return key[:first], "", key[last+1:]
	}
	return key[:first], key[first+1 : last], key[last+1:]
}

// readFilterPins lists the config git sees in dir (with the leading args
// lead) and returns, for every filter driver variable (clean, smudge,
// process, required) the repository config sets, a -c setting that re-sets
// it to its global/system value, or, when there is none, to the empty value
// (no command; an empty process disables the whole driver) and required to
// false. Variables the repository does not set keep their global/system
// value. This is defence in depth; the deny-by-default policy
// (checkRepoConfigPolicy) is the primary control and already refuses any
// non-standard filter driver.
func readFilterPins(ctx context.Context, dir string, lead []string, env []string) ([]string, error) {
	listArgs := append(append([]string{}, lead...), "config", "--show-scope", "-z", "--list")
	cmd := exec.CommandContext(ctx, "git", listArgs...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read git config in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}

	repoDrivers := map[string]map[string]bool{}
	trusted := map[string]string{}
	for _, e := range parseGitConfigList(out) {
		section, subsection, variable := splitConfigKey(e.key)
		isFilter := strings.EqualFold(section, "filter") && subsection != ""
		if repoConfigScope(e.scope) {
			if isFilter {
				v := strings.ToLower(variable)
				if slices.Contains(filterDriverVars, v) {
					if repoDrivers[subsection] == nil {
						repoDrivers[subsection] = map[string]bool{}
					}
					repoDrivers[subsection][v] = true
				}
			}
			continue
		}
		if (e.scope == "system" || e.scope == "global") && isFilter {
			// Later entries win, as in git: system is listed before global.
			trusted[subsection+"."+strings.ToLower(variable)] = e.value
		}
	}

	names := make([]string, 0, len(repoDrivers))
	for name := range repoDrivers {
		names = append(names, name)
	}
	sort.Strings(names)
	var pins []string
	for _, name := range names {
		for _, v := range filterDriverVars {
			if !repoDrivers[name][v] {
				continue
			}
			value, ok := trusted[name+"."+v]
			if !ok && v == "required" {
				value = "false"
			}
			pins = append(pins, "-c", "filter."+name+"."+v+"="+value)
		}
	}
	return pins, nil
}

// HostGitCmd is a host-side git command built by HostGitCommand. Run,
// Output and CombinedOutput run it and then read the repository config
// again: if the entries it was built from (repository filter drivers, and
// for a remote operation every repository key) changed, they return an
// error wrapping ErrRepoConfigChanged, after logging it, whatever the
// command's own result.
type HostGitCmd struct {
	*exec.Cmd
	verify func() error
}

// Run runs the command, then checks the repository config.
func (c *HostGitCmd) Run() error {
	return c.after(c.Cmd.Run())
}

// Output runs the command and returns its standard output, then checks the
// repository config.
func (c *HostGitCmd) Output() ([]byte, error) {
	out, err := c.Cmd.Output()
	return out, c.after(err)
}

// CombinedOutput runs the command and returns its combined output, then
// checks the repository config.
func (c *HostGitCmd) CombinedOutput() ([]byte, error) {
	out, err := c.Cmd.CombinedOutput()
	return out, c.after(err)
}

func (c *HostGitCmd) after(runErr error) error {
	if c.verify == nil {
		return runErr
	}
	if err := c.verify(); err != nil {
		slog.Error("host-side git: repository config changed while the command ran",
			"dir", c.Dir, "args", c.Args, "error", err)
		if runErr != nil {
			return fmt.Errorf("%w (command error: %v)", err, runErr)
		}
		return err
	}
	return runErr
}

// HostGitCommand builds a git command run in dir (cmd.Dir) with HostGitArgs,
// opts.Config and the repository filter-driver pins ahead of args. Use it for
// host-side commands that write the work tree (worktree add, checkout,
// switch, merge), refresh the index (status) or talk to a remote
// (opts.Network).
//
// Before building the command it reads the repository config files as data
// (never through git) and refuses, returning a *RepoConfigRefusedError
// without running anything, when the config holds an include directive or a
// key outside opts.Policy's allow-list (checkRepoConfigPolicy). It then reads
// the config once through git to compute the filter pins and snapshots the
// sensitive keys; running the returned command re-checks the policy and the
// sensitive keys afterwards (HostGitCmd, verifyConfigStable). The policy read
// is the primary control; the pins and HostGitArgs are defence in depth.
func HostGitCommand(ctx context.Context, dir string, opts HostGitOptions, args ...string) (*HostGitCmd, error) {
	if dir == "" {
		return nil, fmt.Errorf("HostGitCommand: dir is required")
	}
	env := opts.Env
	if env == nil {
		env = HostGitEnv(os.Environ())
	}
	if opts.SkipLFSSmudge {
		env = append(env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	allowOpts := repoConfigAllowOptions{worktreePath: opts.AllowedWorktreePath}
	if err := checkRepoConfigPolicy(ctx, dir, env, opts.Policy, allowOpts); err != nil {
		return nil, err
	}
	// Snapshot the sensitive keys before the command, to detect any change to
	// them while it runs (verifyConfigStable).
	before, err := repoConfigSnapshot(dir, env)
	if err != nil {
		return nil, err
	}
	lead := HostGitArgs()
	if opts.Network {
		lead = append(lead, hostGitNetworkArgs...)
	}
	if opts.LFSURL != "" {
		// Pin the LFS endpoint and its auth mode; -c outranks a tracked
		// .lfsconfig. Pinning access=basic stops a committed
		// lfs.<endpoint>.access=negotiate from making git-lfs use ambient
		// Kerberos/NTLM credentials on the broker host.
		lead = append(lead, "-c", "lfs.url="+opts.LFSURL, "-c", "lfs."+opts.LFSURL+".access=basic")
	}
	for _, kv := range opts.Config {
		lead = append(lead, "-c", kv)
	}
	pins, err := readFilterPins(ctx, dir, lead, env)
	if err != nil {
		return nil, err
	}
	full := append(append(append([]string{}, lead...), pins...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = env
	verify := func() error {
		// After the command, re-run the deny-by-default policy (allow-list plus
		// the git cross-check) on the current repository config, and confirm no
		// sensitive key changed value. The command fails (ErrRepoConfigChanged)
		// on a newly-added include or refused key, or a change to a sensitive
		// key (remote url/fetch, branch tracking, submodule url/update,
		// core.worktree, user.signingkey, gpg.format, filter.lfs.*). A change
		// confined to other allowed keys is tolerated — git-lfs writes
		// lfs.repositoryformatversion (and lfs.<url>.access after auth) during
		// its own filter-process even with the smudge skipped. A fresh context:
		// the command's own may have expired.
		return verifyConfigStable(context.Background(), dir, env, opts.Policy, allowOpts, before)
	}
	return &HostGitCmd{Cmd: cmd, verify: verify}, nil
}

// hostGit returns a git command with HostGitArgs ahead of args and the
// environment from HostGitEnv, for host-side commands that neither write
// the work tree nor read the index through filters (rev-parse, worktree
// list, show-ref, branch -D, config). A caller that sets cmd.Env starts
// from HostGitEnv.
func hostGit(args ...string) *exec.Cmd {
	cmd := exec.Command("git", append(HostGitArgs(), args...)...)
	cmd.Env = HostGitEnv(os.Environ())
	return cmd
}

// DeriveLFSEndpoint returns the git-lfs API endpoint for an http(s) remote
// URL, mirroring git-lfs's own default guess: <remote>.git/info/lfs (or
// <remote>/info/lfs when the URL already ends in .git). It reports ok=false
// for a non-http(s) remote (ssh/scp, where git-lfs negotiates the endpoint
// over SSH and no static URL can be pinned), so the caller can fall back to
// skipping the smudge. The point is to pin a trusted endpoint on the command
// line, outranking any endpoint a tracked .lfsconfig would supply.
func DeriveLFSEndpoint(remote string) (endpoint string, ok bool) {
	parsed, err := url.Parse(strings.TrimSpace(remote))
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return "", false
	}
	// Drop any userinfo, query and fragment: userinfo would otherwise put a
	// stored credential on the git argv (visible in host process listings),
	// and a query would end up spliced into the path.
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".git") {
		parsed.Path += "/info/lfs"
	} else {
		parsed.Path += ".git/info/lfs"
	}
	return parsed.String(), true
}

// ScopedCredentialHelperEnv returns the GIT_CONFIG_* environment that
// installs a one-shot credential helper returning the token, scoped to the
// clone URL's scheme and host only (credential.<scheme>://<host>.helper), so
// git offers the token to that host and to no other endpoint (for example an
// LFS endpoint a tracked .lfsconfig might name). It returns ok=false when the
// URL has no usable http(s) host, so the caller can decide (a plain host-wide
// helper is never used for an untrusted endpoint). base is appended to, so an
// existing GIT_CONFIG_COUNT is respected.
func ScopedCredentialHelperEnv(base []string, cloneURL, token string) (env []string, ok bool) {
	scheme, host, ok := httpSchemeHost(cloneURL)
	if !ok {
		return base, false
	}
	start := existingConfigCount(base)
	key := fmt.Sprintf("credential.%s://%s.helper", scheme, host)
	// The helper text is a constant that reads the credential from the environment
	// at run time (scopedGitCredentialEnv), so the credential value appears in no
	// config value and in no argv — git runs a "!" helper via `sh -c <text>`, and
	// the text is the same for every call. The value is carried only in this git
	// command's environment (below); HostGitEnv scrubs scopedGitCredentialEnv from
	// every host git call, so an inherited value never reaches an unrelated command.
	//
	// First reset the generic credential helper list to empty, so no other
	// configured helper (for example a global `credential.helper = store`) runs
	// and persists the token, or offers credentials to any other host. Then add
	// the one-shot helper scoped to the clone URL's host. NOTE: the empty
	// `credential.helper` reset below is the single sanctioned use of that
	// unscoped key; the TestNoUnscopedHostGitCredentials guard exempts this file
	// for exactly this line.
	return append(base,
		scopedGitCredentialEnv+"="+token,
		"GIT_CONFIG_COUNT="+strconv.Itoa(start+2),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=credential.helper", start),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=", start),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", start+1, key),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", start+1, scopedCredentialHelperText),
	), true
}

// NoCredentialHelperEnv returns base, for a git command that talks to
// cloneURL, with:
//   - the configured credential helper list reset to empty, so no helper of
//     the host user's (a credential store, also one scoped to a URL) is
//     asked;
//   - the configured extra HTTP headers reset to empty. git picks
//     http.<url>.extraHeader settings by the most specific matching URL and
//     skips a less specific reset, so the reset is keyed on cloneURL itself,
//     which no other setting for this request can be more specific than;
//   - askpass disabled: an empty GIT_ASKPASS stops git's askpass lookup, and
//     with GIT_TERMINAL_PROMPT=0 git then fails instead of prompting.
//
// It does not change other git authentication sources, for example
// ~/.netrc, TLS client certificates (http.sslCert) or Kerberos/Negotiate.
func NoCredentialHelperEnv(base []string, cloneURL string) []string {
	start := existingConfigCount(base)
	return append(base,
		"GIT_ASKPASS=",
		"GIT_CONFIG_COUNT="+strconv.Itoa(start+2),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=credential.helper", start),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=", start),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=http.%s.extraHeader", start+1, cloneURL),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=", start+1),
	)
}

// scopedGitCredentialEnv is the environment variable the scoped credential helper
// reads at run time. It is set only in the environment of the single git command
// that needs it, and scrubbed by HostGitEnv from every host git call. Its value is
// readable through /proc/<pid>/environ only by the same uid (and root).
const scopedGitCredentialEnv = "SCION_SCOPED_GIT_CREDENTIAL"

// scopedCredentialHelperText is the constant git credential-helper command. It
// expands scopedGitCredentialEnv at run time, so no credential value is ever
// formatted into a config value or passed on an argv.
const scopedCredentialHelperText = `!f() { echo username=oauth2; echo "password=${` + scopedGitCredentialEnv + `}"; }; f`

// existingConfigCount returns the GIT_CONFIG_COUNT already set in env, or 0.
func existingConfigCount(env []string) int {
	n := 0
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
				n = parsed
			}
		}
	}
	return n
}

// httpSchemeHost returns the lower-cased scheme ("http"/"https") and the host
// (with port) of an http(s) URL.
func httpSchemeHost(raw string) (scheme, host string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false
	}
	s := strings.ToLower(u.Scheme)
	if (s != "http" && s != "https") || u.Host == "" {
		return "", "", false
	}
	return s, u.Host, true
}
