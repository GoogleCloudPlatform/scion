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
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Deny-by-default repository config policy.
//
// Before a host-side git command that writes the work tree or talks to a
// remote runs in a repository agents can write, its config is read as data
// (never through git, which would run include directives and could split a
// -c pin at an '=') and checked against an allow-list. Anything outside the
// list, and any include/includeIf directive, refuses the command. The fixed
// -c pins (HostGitArgs), the environment scrub (HostGitEnv) and the
// before/after comparison stay on top as defence in depth.
//
// Two policies:
//   - RepoPolicyManaged: a repository Scion creates and owns — a hub-native
//     shared base, a shared-workspace clone. Only the keys git and Scion
//     write for it are allowed.
//   - RepoPolicyLinked: the user's own linked checkout. A wider list of keys
//     that cannot run a command or redirect network traffic is allowed too,
//     so ordinary local config does not stop an agent starting. Keys whose
//     value can carry a command are value-checked.
//
// Keys that execute a command or redirect the network (core.sshCommand,
// core.hooksPath, core.pager, core.editor, core.fsmonitor, credential.*,
// url.*.insteadOf, http.*, gpg.*, any *.program, alias.*, remote.* upload/
// receive/proxy/vcs) are never on either list, so deny-by-default refuses
// them. The read is as-data and does not depend on git's own config
// resolution, so a subsection containing '=' or quotes stays part of the
// subsection name and the full key is checked.

// RepoConfigPolicy selects the allow-list a host-side git command uses.
type RepoConfigPolicy int

const (
	// RepoPolicyManaged is the strict list for a Scion-managed repository.
	RepoPolicyManaged RepoConfigPolicy = iota
	// RepoPolicyLinked is the wider list for the user's own checkout.
	RepoPolicyLinked
)

// Audience is who reads the errors a host-side git command returns.
type Audience int

const (
	// AudienceLocal is the user at the local CLI. A refusal names the
	// repository directory.
	AudienceLocal Audience = iota
	// AudienceRemote is anyone else: an agent, the Hub, a Hub log. A
	// RepoConfigRefusedError, and the errors HostGitCommand returns for
	// repository discovery and its bound, carry no host path. Other errors,
	// such as a failure to locate, read or parse the repository config, can
	// still name one.
	AudienceRemote
)

// RepoConfigRefusedError reports that a repository config key (or an
// include directive) is not allowed for a host-side git command or an agent
// start.
//
// Error never includes Value: a refused value can carry a secret. Every fmt
// verb and every slog handler prints Error() (Format, LogValue), never the
// fields.
type RepoConfigRefusedError struct {
	Dir    string
	Key    string
	Value  string
	Reason string
	// Seeded marks a refusal from an agent-start (seed) check rather than
	// from a host-side git command. Error then names the remedy for that case.
	Seeded bool
	// Layer names, by a fixed or relative label, the config file, submodule
	// or combined layers the refusal concerns (never a raw path).
	Layer string
	// Audience selects the host-side wording. For AudienceRemote the
	// constructor leaves Dir empty and Error() names no directory. A seeded
	// refusal prints no Dir either way.
	Audience Audience
}

// newRepoConfigRefusedError is the only constructor of RepoConfigRefusedError.
// key is the raw dotted key (section.subsection.name, as git prints it); the
// error carries refusalKeyName(key), so no refusal prints userinfo carried in
// a subsection.
//
// For AudienceRemote it drops dir, so a remote refusal carries no host path
// in any field.
func newRepoConfigRefusedError(dir, layer, key, value, reason string, seeded bool, audience Audience) *RepoConfigRefusedError {
	if audience == AudienceRemote {
		dir = ""
	}
	return &RepoConfigRefusedError{Dir: dir, Key: refusalKeyName(key), Value: value, Reason: reason, Seeded: seeded, Layer: layer, Audience: audience}
}

// refusalKeyName returns the name a refusal prints for a raw dotted key. A key
// without a subsection is returned unchanged. Otherwise the key is split as git
// prints it (sections and names never contain a dot, so the first and last dots
// delimit the subsection) and named by notedKeyName, the one producer of
// printed key names.
func refusalKeyName(key string) string {
	first := strings.Index(key, ".")
	last := strings.LastIndex(key, ".")
	if first < 0 || first == last {
		return key
	}
	return notedKeyName(gitConfigVar{Section: key[:first], Subsection: key[first+1 : last], HasSubsection: true, Name: key[last+1:]})
}

func (e *RepoConfigRefusedError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "not on the host-side git allow-list"
	}
	var msg string
	if e.Seeded {
		// The agent reads no host global git config, so the only remedy is to
		// remove the setting from the layer that sets it. A structural refusal
		// (a <placeholder> key) names a submodule directory or git link, and a
		// ".git/<name>" key names a file, not a setting. No config key starts
		// with either: a section name starts with a letter, digit or '-'
		// (isConfigNameChar), and a legacy [section.sub] header cannot start
		// with '.' (parseSection).
		remedy := "remove it from the repository's .git/config"
		switch {
		case strings.HasPrefix(e.Key, "<"):
			remedy = "remove or repair the submodule directory or git link it names"
		case strings.HasPrefix(e.Key, ".git/"):
			remedy = "remove that file from the repository's .git directory"
		case e.Layer != "":
			remedy = "remove it from the git config file that sets it"
		}
		msg = fmt.Sprintf("agent git config refused: repository config sets %s, which is %s; %s", e.Key, reason, remedy)
	} else {
		where := e.Dir
		if e.Audience == AudienceRemote {
			where = "the project repository"
		}
		msg = fmt.Sprintf("git in %s refused: repository config sets %s, which is %s; remove it from the repository's .git/config (or config.worktree)", where, e.Key, reason)
	}
	if e.Layer != "" {
		msg += " (in " + e.Layer + ")"
	}
	return msg
}

// Format prints Error() for every verb and flag (%v, %+v, %#v, %s, %q, ...),
// so no format string can print the fields, Dir and Value among them. It has
// a value receiver so a dereferenced copy formats the same way; fmt prints a
// nil pointer as "<nil>".
func (e RepoConfigRefusedError) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, e.Error())
}

// LogValue makes every slog handler log Error(), never the fields. It has a
// value receiver for the same reason as Format.
func (e RepoConfigRefusedError) LogValue() slog.Value {
	return slog.StringValue(e.Error())
}

// standardLFSFilter is the exact filter.lfs.* command set `git lfs install`
// writes. filter.lfs.* is allowed only when it matches byte for byte.
var standardLFSFilter = map[string]string{
	"clean":   "git-lfs clean -- %f",
	"smudge":  "git-lfs smudge -- %f",
	"process": "git-lfs filter-process",
}

// lfsAccessValues are the git-lfs lfs.<url>.access auth modes we accept.
// negotiate and ntlm are excluded: they would let git-lfs use ambient
// Kerberos/NTLM credentials from the broker host.
var lfsAccessValues = map[string]bool{"basic": true, "none": true}

// isGitBool reports whether v is a git boolean literal.
func isGitBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "false", "yes", "no", "on", "off", "1", "0", "":
		return true
	}
	return false
}

// validLFSURLSubsection reports whether an lfs.<url>.* subsection is an
// http(s) URL with a host and no whitespace or control characters;
// lfs.<url>.access and lfs.<url>.locksverify are allowed only under such a
// subsection.
func validLFSURLSubsection(sub string) bool {
	if sub == "" {
		return false
	}
	for _, r := range sub {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(sub)
	if err != nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return (s == "http" || s == "https") && u.Host != ""
}

// submoduleUpdateAllowed are the submodule.<name>.update values that run no
// command (never a "!command").
var submoduleUpdateAllowed = map[string]bool{
	"none": true, "checkout": true, "rebase": true, "merge": true,
}

// repoConfigAllowOptions carries context a value check needs.
type repoConfigAllowOptions struct {
	// worktreePath, when set, is the one broker-known path core.worktree
	// may equal. Empty refuses any core.worktree.
	worktreePath string
	// seed marks a check of an agent's seeded copy of the repository config.
	// The agent's git reads that copy without the HostGitArgs pins, so the
	// keys in seedUnpinnedRefuse are refused too.
	seed bool
	// audience is the host-side refusal's audience. A seed refusal always
	// uses AudienceRemote.
	audience Audience
}

// refusalAudience is the audience of a refusal: AudienceRemote for a seeded
// one (it reaches the agent), else audience.
func refusalAudience(seeded bool, audience Audience) Audience {
	if seeded {
		return AudienceRemote
	}
	return audience
}

// allowVar reports whether v is allowed under policy, with a reason when it
// is not. It never allows a key that can run a command, read or write an
// arbitrary host path, or redirect the network; keys whose value may carry
// one of those are value-checked. topLevel below means [section] (no
// subsection) — [section ""] (an empty quoted subsection) is a different
// section and is treated as subsectioned, so it is checked against the
// subsectioned rules rather than the top-level ones.
func allowVar(policy RepoConfigPolicy, v gitConfigVar, opts repoConfigAllowOptions) (bool, string) {
	sec := v.Section
	sub := v.Subsection
	name := v.Name
	topLevel := !v.HasSubsection
	hasSub := v.HasSubsection

	// include/includeIf pull in other files: always refused (the caller
	// refuses before reaching here too).
	if sec == "include" || sec == "includeif" {
		return false, "an include directive"
	}
	// Any variable named "program" names an executable (gpg.program,
	// gpg.ssh.program, ...).
	if name == "program" {
		return false, "a program setting"
	}

	// Keys allowed under both policies: what git and Scion write for a
	// repository they manage.
	switch sec {
	case "core":
		if topLevel {
			switch name {
			case "repositoryformatversion", "filemode", "bare", "logallrefupdates",
				"ignorecase", "precomposeunicode", "symlinks":
				return true, ""
			case "worktree":
				if opts.worktreePath != "" && v.Value == opts.worktreePath {
					return true, ""
				}
				return false, "a work-tree redirect"
			}
		}
	case "extensions":
		if topLevel {
			switch name {
			case "worktreeconfig", "relativeworktrees", "objectformat", "refstorage":
				return true, ""
			}
		}
	case "remote":
		if hasSub && (name == "url" || name == "fetch") {
			return true, ""
		}
	case "branch":
		if hasSub {
			switch name {
			case "remote", "merge", "rebase":
				return true, ""
			}
		}
		if topLevel && name == "autosetupmerge" {
			return true, ""
		}
	case "gc":
		if topLevel && name == "auto" {
			return true, ""
		}
	case "user":
		if topLevel && (name == "name" || name == "email") {
			return true, ""
		}
	case "lfs":
		// The only keys git-lfs itself writes to repository config, under both
		// policies (it writes repositoryformatversion even with the smudge
		// skipped, and <url>.access after authenticating). Each is value-checked
		// and none redirects, executes or names a host path. Every other lfs.*
		// key is refused in the linked section below (and outright for managed).
		if topLevel && name == "repositoryformatversion" && v.Value == "0" {
			return true, ""
		}
		if hasSub && name == "access" {
			if !validLFSURLSubsection(sub) {
				return false, "a git-lfs access key with a non-URL subsection"
			}
			if lfsAccessValues[strings.ToLower(v.Value)] {
				return true, ""
			}
			return false, "a git-lfs access mode outside {basic, none}"
		}
		if hasSub && name == "locksverify" {
			if !validLFSURLSubsection(sub) {
				return false, "a git-lfs locksverify key with a non-URL subsection"
			}
			if isGitBool(v.Value) {
				return true, ""
			}
			return false, "a non-boolean git-lfs locksverify"
		}
	}

	if policy == RepoPolicyManaged {
		return false, "not on the managed-repository allow-list"
	}

	// Wider list for the user's own linked checkout: keys that run nothing,
	// read or write no arbitrary host path, and redirect no network traffic.
	// A few keys that would run a command are allowed here only because a
	// fixed -c pin on every host-side call (HostGitArgs) overrides their
	// repository value — core.hooksPath, core.fsmonitor, core.pager and
	// submodule.recurse — so such a repository value never runs a command.
	switch sec {
	case "core":
		if topLevel {
			switch name {
			case "autocrlf", "eol", "safecrlf", "whitespace", "commentchar",
				"quotepath", "longpaths", "protecthfs", "protectntfs",
				"checkstat", "trustctime", "untrackedcache", "fsyncmethod",
				"splitindex", "preloadindex", "abbrev",
				"sparsecheckout", "sparsecheckoutcone",
				// Neutralised by the HostGitArgs pins on every call.
				"hookspath", "fsmonitor", "pager":
				return true, ""
			}
		}
	case "remote":
		if hasSub {
			switch name {
			case "pushurl", "tagopt", "prune", "prunetags", "mirror",
				"fetchrecursesubmodules", "followremotehead":
				return true, ""
			case "uploadpack", "receivepack", "proxy", "vcs":
				return false, "a remote command or proxy override"
			}
		}
	case "branch":
		if hasSub {
			switch name {
			case "description", "pushremote", "mergeoptions", "autosetuprebase":
				return true, ""
			}
		}
		if topLevel && name == "autosetuprebase" {
			return true, ""
		}
	case "pull":
		if topLevel && (name == "rebase" || name == "ff" || name == "octopus" || name == "twohead") {
			return true, ""
		}
	case "push":
		if topLevel {
			switch name {
			case "default", "autosetupremote", "followtags", "gpgsign":
				return true, ""
			}
		}
	case "fetch":
		if topLevel {
			switch name {
			case "prune", "prunetags", "fsckobjects", "output", "parallel":
				return true, ""
			}
		}
	case "commit":
		if topLevel && (name == "gpgsign" || name == "verbose") {
			return true, ""
		}
	case "tag":
		if topLevel && (name == "gpgsign" || name == "sort" || name == "forcesignannotated") {
			return true, ""
		}
	case "init":
		if topLevel && name == "defaultbranch" {
			return true, ""
		}
	case "maintenance":
		// The background maintenance tasks are git built-ins; these keys only
		// enable/schedule them and name no command.
		if topLevel && (name == "auto" || name == "strategy" || name == "autodetach") {
			return true, ""
		}
		if hasSub && (name == "enabled" || name == "schedule" || name == "auto") {
			return true, ""
		}
	case "user":
		// Signing key identity (a key id or path) is inert data; a
		// gpg/ssh *.program that would act on it is refused above, which
		// refuses the whole config.
		if topLevel && name == "signingkey" {
			return true, ""
		}
	case "gpg":
		// SSH/commit signing format only — inert data. Any gpg program key is
		// refused (name == "program" above), and any gpg.<fmt>.program would
		// be too; gpg.ssh.allowedSignersFile and the like (host-file reads)
		// are not on the list.
		if topLevel && name == "format" {
			return true, ""
		}
		return false, "a gpg setting that is not inert"
	case "color":
		// color.<slot> and color.<cmd>.<slot>: enums/booleans, never a
		// command, and no color key names a program.
		if topLevel && colorTopKeys[name] {
			return true, ""
		}
		if hasSub && colorCmds[strings.ToLower(sub)] {
			return true, ""
		}
	case "advice":
		if topLevel {
			// advice.* are all booleans.
			return true, ""
		}
	case "rerere":
		if topLevel && (name == "enabled" || name == "autoupdate") {
			return true, ""
		}
	case "status":
		if topLevel && statusKeys[name] {
			return true, ""
		}
	case "diff":
		if topLevel && diffTopKeys[name] {
			return true, ""
		}
		if hasSub && diffDriverKeys[name] {
			// A named diff driver's data attributes only (never textconv,
			// command or cachetextconv).
			return true, ""
		}
	case "merge":
		if topLevel && mergeTopKeys[name] {
			return true, ""
		}
		if hasSub && (name == "name" || name == "recursive") {
			// A named merge driver's description/strategy only (never driver
			// or cmd).
			return true, ""
		}
	case "submodule":
		if topLevel && name == "recurse" {
			// Pinned off by HostGitArgs, so inert.
			return true, ""
		}
		if hasSub {
			switch name {
			case "update":
				if submoduleUpdateAllowed[strings.ToLower(v.Value)] {
					return true, ""
				}
				return false, "a submodule update command"
			case "url":
				if submoduleURLAllowed(v.Value) {
					return true, ""
				}
				return false, "a submodule URL with a non-https/ssh transport"
			case "active", "ignore", "shallow", "branch", "fetchrecursesubmodules":
				return true, ""
			}
		}
		return false, "a submodule setting that is not on the allow-list"
	case "filter":
		if sub == "lfs" && hasSub {
			if name == "required" {
				return true, ""
			}
			if want, ok := standardLFSFilter[name]; ok && v.Value == want {
				return true, ""
			}
			return false, "a non-standard git-lfs filter command"
		}
		return false, "a filter driver command"
	case "lfs":
		// Deny-by-default: only a small set of inert top-level keys. Every
		// subsectioned lfs.* (lfs.<url>.access, lfs.extension.<n>.*,
		// lfs.customtransfer.<n>.*) and the redirect/exec/host-path keys
		// (url, pushurl, storage, standalonetransferagent) are refused.
		if topLevel && lfsInertKeys[name] {
			return true, ""
		}
		return false, "a git-lfs setting that is not inert"
	}

	if policy == RepoPolicyLinked {
		return false, "not on the linked-checkout allow-list"
	}
	return false, "not on the allow-list"
}

// submoduleURLAllowed reports whether a submodule URL uses only the https or
// ssh transport (so recursion, were it ever on, could not reach an ext:: or
// file transport). Submodule recursion is pinned off regardless; this is a
// second bound.
func submoduleURLAllowed(url string) bool {
	u := strings.TrimSpace(url)
	lower := strings.ToLower(u)
	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "ssh://") {
		return true
	}
	// scp-like syntax user@host:path is ssh, but must not be a transport
	// prefix like ext::/file:/ and must not be an absolute or relative path.
	if strings.Contains(u, "://") {
		return false
	}
	if at := strings.Index(u, "@"); at > 0 {
		rest := u[at+1:]
		if colon := strings.Index(rest, ":"); colon > 0 && !strings.Contains(rest[:colon], "/") {
			return true
		}
	}
	return false
}

// Explicit per-section key sets for the linked-checkout policy. They list
// keys that carry only data or enums (never a command, path or program), so
// nothing in these sections can make host-side git execute. Any key not
// listed is refused by deny-by-default.
var (
	colorTopKeys = set("ui", "pager", "advice", "showbranch")
	colorCmds    = set("branch", "diff", "decorate", "grep", "interactive",
		"log", "push", "remote", "status", "transport", "add")
	statusKeys = set("short", "branch", "showstash", "showuntrackedfiles",
		"submodulesummary", "relativepaths", "displaycommentprefix",
		"renamelimit", "renames", "aheadbehind")
	diffTopKeys = set("algorithm", "renames", "renamelimit", "indentheuristic",
		"colormoved", "colormovedws", "context", "interhunkcontext",
		"mnemonicprefix", "noprefix", "wordregex", "submodule", "relative",
		"ignoresubmodules", "dirstat", "statgraphwidth", "wserrorhighlight",
		"suppressblankempty", "colorwords", "tabsize")
	diffDriverKeys = set("binary", "wordregex", "xfuncname")
	mergeTopKeys   = set("conflictstyle", "ff", "renamelimit", "renames",
		"stat", "log", "autostash", "verbosity", "directoryrenames",
		"renormalize", "branchdesc")
	// Inert git-lfs keys (timeouts, counts, booleans, in-repo globs). No
	// URL, path, storage location, custom transfer or extension command.
	lfsInertKeys = set("concurrenttransfers", "dialtimeout", "tlstimeout",
		"activitytimeout", "fetchrecentrefsdays", "fetchrecentcommitsdays",
		"fetchrecentremoterefs", "fetchrecentalways", "pruneoffsetdays",
		"pruneverifyremotealways", "locksverifyeverywhere", "locksverify",
		"skipdownloaderrors", "setlockablereadonly", "fetchinclude",
		"fetchexclude", "batch", "transfermaxretries", "contenttype")
)

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// checkRepoConfigPolicy reads, as data, every repository config file git
// would read for a command in dir — the common .git/config, the main
// worktree's config.worktree (in the common dir), and this worktree's
// config.worktree, all always (see repoConfigFiles) — and returns a
// *RepoConfigRefusedError for the first include directive or key the policy
// does not allow. It then cross-checks the parsed repository-scope entries
// against git's own --show-scope listing (crossCheckAgainstGit) and refuses
// if git resolves any entry the parser did not, so a parser divergence fails
// closed. It fails closed on anything it cannot locate, read or parse.
func checkRepoConfigPolicy(ctx context.Context, dir string, env []string, policy RepoConfigPolicy, opts repoConfigAllowOptions) error {
	if err := refuseRootCommonDirRedirect(dir, false, opts.audience); err != nil {
		return err
	}
	parsed, err := parseRepoConfig(dir, env)
	if err != nil {
		return err
	}
	if err := checkRepoConfigVars(dir, "", parsed, policy, opts, nil); err != nil {
		return err
	}
	return crossCheckAgainstGit(ctx, dir, env, parsed, "", false, opts.audience)
}

// checkRepoConfigVars applies policy to already-parsed config variables and
// returns a *RepoConfigRefusedError for the first one it refuses. It reads no
// file and runs no git. For each variable, in this order:
//   - an include or includeIf directive is refused;
//   - with opts.seed, a key in seedUnpinnedRefuse is refused;
//   - a variable for which exempt returns true skips the allow-list (the seed
//     passes isDroppedKey: those keys are removed from the copy, not refused);
//   - allowVar decides.
//
// The include and seedUnpinnedRefuse checks come before the exemption, so no
// exemption can let either through. exempt may be nil.
//
// A refusal names the key by refusalKeyName (newRepoConfigRefusedError),
// seeded or not: a subsection that may carry userinfo, or a credential.<url>
// subsection, is never printed verbatim. A key whose subsection has no '@' and
// is not credential.<url> is named exactly as v.Key().
func checkRepoConfigVars(dir, layer string, vars []gitConfigVar, policy RepoConfigPolicy, opts repoConfigAllowOptions, exempt func(gitConfigVar) bool) error {
	refused := func(v gitConfigVar, reason string) error {
		return newRepoConfigRefusedError(dir, layer, v.Key(), v.Value, reason, opts.seed, refusalAudience(opts.seed, opts.audience))
	}
	for _, v := range vars {
		if v.Section == "include" || v.Section == "includeif" {
			return refused(v, "an include directive")
		}
		if opts.seed && seedKeyMatchesAny(seedUnpinnedRefuse, v) {
			return refused(v, "a setting that runs a command in the agent's git")
		}
		if exempt != nil && exempt(v) {
			continue
		}
		if ok, reason := allowVar(policy, v, opts); !ok {
			return refused(v, reason)
		}
	}
	return nil
}

// seedKeyRule names one config key exactly: section and variable name
// (lower-cased) and, when HasSubsection is set, the subsection (case kept). A
// rule without a subsection matches only the key without one.
type seedKeyRule struct {
	Section       string
	Subsection    string
	HasSubsection bool
	Name          string
}

// matches reports whether v is exactly the key r names.
func (r seedKeyRule) matches(v gitConfigVar) bool {
	if v.Section != r.Section || v.Name != r.Name || v.HasSubsection != r.HasSubsection {
		return false
	}
	return !r.HasSubsection || v.Subsection == r.Subsection
}

// seedKeyMatchesAny reports whether v is named by any rule in rules.
func seedKeyMatchesAny(rules []seedKeyRule, v gitConfigVar) bool {
	for _, r := range rules {
		if r.matches(v) {
			return true
		}
	}
	return false
}

// seedUnpinnedRefuse are the keys RepoPolicyLinked allows only because a
// HostGitArgs pin overrides them on every host-side call. An agent's git reads
// its seeded copy of the repository config without those pins, and repository
// config overrides the agent's global config, so in the copy each of these
// would run a command: the seed refuses them.
var seedUnpinnedRefuse = []seedKeyRule{
	{Section: "core", Name: "hookspath"}, // runs hooks from a directory the config chooses
	{Section: "core", Name: "fsmonitor"}, // a command run by status and other index reads
	{Section: "core", Name: "pager"},     // a command run for paged output
}

// seedUnpinnedSafe are the other HostGitArgs pin keys RepoPolicyLinked allows.
// Each is seeded unchanged: without the pin its repository value still runs no
// command the config chooses and redirects nothing. The guard test requires
// every pin key Linked allows to be in exactly one of seedUnpinnedRefuse and
// seedUnpinnedSafe.
var seedUnpinnedSafe = []seedKeyRule{
	// A boolean that makes checkout, pull and similar recurse into submodules.
	// In the repository config, every submodule.<name>.url Linked allows is
	// https or ssh, and every submodule.<name>.update it allows is a built-in
	// mode, never "!command". Residual: the seed does not check a submodule's
	// own config (.git/modules/<name>/config), which git reads when it works
	// inside a populated submodule, with or without this key.
	{Section: "submodule", Name: "recurse"},
	// A boolean that signs commits with git's default signing program. Linked
	// refuses every *.program key and every gpg.* key except gpg.format, so the
	// repository cannot choose the program (or gpg.ssh.defaultKeyCommand).
	{Section: "commit", Name: "gpgsign"},
	// As commit.gpgsign, for annotated tags.
	{Section: "tag", Name: "gpgsign"},
}

// PrepareSeededGitConfig builds the agent's seeded copy of a repository
// config from raw. It parses raw exactly once and then, on those parsed
// variables only:
//   - checks them against policy (checkRepoConfigVars with the seed option and
//     the isDroppedKey exemption), returning a *RepoConfigRefusedError naming the
//     first refused key;
//   - removes credential-bearing material (redactGitConfigVars, as
//     RedactGitConfigSecrets does);
//   - removes every variable for which strip returns true (strip may be nil);
//     section and name are lower-cased, subsection keeps its case;
//   - serializes what survives.
//
// What was checked is what is written: the result is re-serialized from the
// checked variables, never copied from raw. stripped lists the redacted names of
// the keys the redaction changed or dropped. A parse error is returned so the
// caller can fail closed.
func PrepareSeededGitConfig(raw []byte, policy RepoConfigPolicy, strip func(section, subsection string, hasSubsection bool, name string) bool) (out []byte, stripped []string, err error) {
	return prepareSeededGitConfig(raw, parseGitConfigFile, policy, strip)
}

// prepareSeededGitConfig is PrepareSeededGitConfig with the parser passed in,
// so a test can observe that raw is parsed exactly once and that the written
// bytes come from the variables that parse returned.
func prepareSeededGitConfig(raw []byte, parse func([]byte) ([]gitConfigVar, error), policy RepoConfigPolicy, strip func(section, subsection string, hasSubsection bool, name string) bool) ([]byte, []string, error) {
	vars, err := parse(raw)
	if err != nil {
		return nil, nil, err
	}
	if err := checkRepoConfigVars("", "", vars, policy, repoConfigAllowOptions{seed: true}, isDroppedKey); err != nil {
		return nil, nil, err
	}
	kept, stripped := redactGitConfigVars(vars)
	if strip != nil {
		filtered := kept[:0]
		for _, v := range kept {
			if strip(v.Section, v.Subsection, v.HasSubsection, v.Name) {
				continue
			}
			filtered = append(filtered, v)
		}
		kept = filtered
	}
	return serializeGitConfig(kept), stripped, nil
}

// refuseRootCommonDirRedirect refuses a host-side git command or, when seeded
// is true, an agent start, when the repository's `.git` directory carries a
// root-level `commondir` or `gitdir` file. A normal repository never has one
// there (a `commondir`/`gitdir` belongs inside a worktree admin dir,
// `.git/worktrees/<name>/`); at the `.git` root it
// makes git treat the repository itself as a linked worktree and resolve the
// common dir — the place it reads config, hooks and refs from — through an
// agent-writable path. The container-side read-only `.git` root closes this for
// agents; this guard protects the user's host git for the layouts we do not mount
// read-only (shared-plain and any pre-existing state). A linked worktree, whose
// `.git` is a gitfile pointing at its admin dir, legitimately resolves a
// commondir and is not the target here, so only a `.git` directory is checked.
// The seeded form is an agent-start (seed) refusal with the remote audience,
// so the constructor leaves Dir empty and it prints no host path; the
// host-side form keeps dir for the local audience.
func refuseRootCommonDirRedirect(dir string, seeded bool, audience Audience) error {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil || !fi.IsDir() {
		return nil
	}
	for _, name := range []string{"commondir", "gitdir"} {
		if _, err := os.Lstat(filepath.Join(gitPath, name)); err == nil {
			reason := "a root-level " + name + " would redirect the repository's common git directory"
			return newRepoConfigRefusedError(dir, "", ".git/"+name, "", reason, seeded, refusalAudience(seeded, audience))
		}
	}
	return nil
}

// parseRepoConfig reads, as data, every repository config file git would read
// for a command in dir (repoConfigFiles) and returns the parsed variables.
// It fails closed on any file it cannot read or parse.
func parseRepoConfig(dir string, env []string) ([]gitConfigVar, error) {
	files, err := repoConfigFiles(dir, env)
	if err != nil {
		return nil, fmt.Errorf("git in %s: cannot locate the repository config to check it: %w", dir, err)
	}
	var parsed []gitConfigVar
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("git in %s: cannot read %s: %w", dir, f, err)
		}
		vars, err := parseGitConfigFile(data)
		if err != nil {
			return nil, fmt.Errorf("git in %s: cannot parse %s (refusing): %w", dir, f, err)
		}
		parsed = append(parsed, vars...)
	}
	return parsed, nil
}

// isSensitiveConfigKey reports whether a full config key is one of the
// allowed, value-sensitive keys that git-lfs (and no other legitimate
// in-command writer) never writes, so any change to it while a host-side
// command runs is rejected. The subsection case is preserved by
// splitConfigKey; section and name are already lower-case in a snapshot key.
func isSensitiveConfigKey(key string) bool {
	section, subsection, name := splitConfigKey(key)
	hasSub := strings.Count(key, ".") >= 2
	switch section {
	case "remote":
		return hasSub && (name == "url" || name == "fetch")
	case "branch":
		return hasSub && (name == "remote" || name == "merge")
	case "submodule":
		return hasSub && (name == "update" || name == "url")
	case "core":
		return !hasSub && name == "worktree"
	case "user":
		return !hasSub && name == "signingkey"
	case "gpg":
		return !hasSub && name == "format"
	case "filter":
		return hasSub && subsection == "lfs"
	}
	return false
}

// repoConfigSnapshot returns the current repository-config values at dir
// (parsed as data), keyed by full config key. A key's value is the ordered
// list of all its values (git config is multi-valued and order-significant,
// for example several remote.<n>.fetch refspecs or two remote.<n>.url lines),
// so a reordering or an inserted value is not collapsed away.
func repoConfigSnapshot(dir string, env []string) (map[string][]string, error) {
	parsed, err := parseRepoConfig(dir, env)
	if err != nil {
		return nil, err
	}
	m := make(map[string][]string, len(parsed))
	for _, v := range parsed {
		m[v.Key()] = append(m[v.Key()], v.Value)
	}
	return m, nil
}

// verifyConfigStable is the post-command check: the config must still pass the
// deny-by-default policy (including the git cross-check), and no sensitive key
// may have changed its ordered value list since before. A change confined to
// other allowed keys (for example git-lfs writing lfs.repositoryformatversion
// during its own filter-process) is tolerated and logged by key name only. Any
// failure returns an error wrapping ErrRepoConfigChanged.
func verifyConfigStable(ctx context.Context, dir string, env []string, policy RepoConfigPolicy, opts repoConfigAllowOptions, before map[string][]string) error {
	if err := checkRepoConfigPolicy(ctx, dir, env, policy, opts); err != nil {
		return fmt.Errorf("%w: %v", ErrRepoConfigChanged, err)
	}
	after, err := repoConfigSnapshot(dir, env)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRepoConfigChanged, err)
	}
	seen := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		seen[k] = struct{}{}
	}
	for k := range after {
		seen[k] = struct{}{}
	}
	var changedOther []string
	for k := range seen {
		b, a := before[k], after[k]
		// A missing key (nil) is distinct from a key present with an empty
		// value ([]string{""}): slices.Equal(nil, [""]) is false.
		if slices.Equal(b, a) {
			continue
		}
		if isSensitiveConfigKey(k) {
			return fmt.Errorf("%w: sensitive repository config key %q changed while the command ran", ErrRepoConfigChanged, k)
		}
		changedOther = append(changedOther, k)
	}
	if len(changedOther) > 0 {
		slog.Debug("host-side git: allowed repository config keys changed during the command (tolerated)",
			"dir", dir, "keys", changedOther)
	}
	return nil
}

// crossCheckAgainstGit lists the repository-scope config git itself resolves
// (local and worktree scope) and refuses if git resolves any entry that is not
// in the parsed set, so any divergence between the as-data parser and git
// fails closed. git runs with the same host-side pins and scrubbed
// environment. dir is where git runs; a seeded refusal has the remote
// audience, so the constructor leaves its Dir empty.
func crossCheckAgainstGit(ctx context.Context, dir string, env []string, parsed []gitConfigVar, layer string, seeded bool, audience Audience) error {
	args := append(HostGitArgs(), "config", "--show-scope", "-z", "--list")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git in %s: cannot list the repository config to cross-check it: %w", dir, err)
	}
	// Compare keys exactly: git lower-cases the section and variable and keeps
	// the subsection's case, and gitConfigVar.Key() does the same, so no extra
	// lower-casing is needed (and subsections are case-sensitive).
	parsedSet := make(map[string]int, len(parsed))
	for _, v := range parsed {
		parsedSet[v.Key()+"\x00"+v.Value]++
	}
	// Every repository-scope entry git actually resolves must appear in the
	// parsed set. The parsed set may hold more — config.worktree is parsed
	// even when extensions.worktreeConfig is off, which git then ignores — so
	// this is a subset check, not equality: it catches any key or value the
	// parser missed or read differently (a parser divergence), which is the
	// security-relevant direction, while allowing the intentional extra.
	for _, e := range parseGitConfigList(out) {
		if !repoConfigScope(e.scope) {
			continue
		}
		// A valueless boolean lists as the key alone; the parser stores it as
		// "true". Match the two.
		value := e.value
		if !e.hasValue {
			value = "true"
		}
		k := e.key + "\x00" + value
		if parsedSet[k] <= 0 {
			return newRepoConfigRefusedError(dir, layer, e.key, "", "read differently by the config parser and by git; refusing to be safe", seeded, refusalAudience(seeded, audience))
		}
		parsedSet[k]--
	}
	return nil
}

// repoConfigFiles returns the config files to check for a command in dir,
// honouring GIT_COMMON_DIR / GIT_DIR in env when set: the common
// .git/config, the main worktree's config.worktree (in the common dir), and
// this worktree's config.worktree. config.worktree is checked ALWAYS, even
// when extensions.worktreeConfig is off, so a refused key placed there is
// never missed (fail closed) — git would read it the moment the extension is
// turned on. It resolves a .git gitfile pointer and a worktree's commondir
// file without running git.
func repoConfigFiles(dir string, env []string) ([]string, error) {
	commonDir, perWorktreeDir, err := resolveGitDirs(dir, env)
	if err != nil {
		return nil, err
	}
	files := []string{
		filepath.Join(commonDir, "config"),
		filepath.Join(commonDir, "config.worktree"),
	}
	if perWorktreeDir != "" && perWorktreeDir != commonDir {
		files = append(files, filepath.Join(perWorktreeDir, "config.worktree"))
	}
	return files, nil
}

// resolveGitDirs returns the common git directory and the per-worktree git
// directory for dir. env's GIT_COMMON_DIR and GIT_DIR take precedence.
func resolveGitDirs(dir string, env []string) (commonDir, perWorktreeDir string, err error) {
	envCommon := gitEnvValue(env, "GIT_COMMON_DIR")
	envGitDir := gitEnvValue(env, "GIT_DIR")

	perWorktreeDir = envGitDir
	if perWorktreeDir == "" {
		perWorktreeDir, err = gitDirFromWorkTree(dir)
		if err != nil {
			return "", "", err
		}
	}
	commonDir = envCommon
	if commonDir == "" {
		commonDir = commonDirFrom(perWorktreeDir)
	}
	return absClean(commonDir, dir), absClean(perWorktreeDir, dir), nil
}

// gitDirFromWorkTree resolves <dir>/.git: a directory is the git dir itself;
// a "gitdir: <path>" file points to the per-worktree admin directory.
func gitDirFromWorkTree(dir string) (string, error) {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return gitPath, nil
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("%s is not a gitdir pointer", gitPath)
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if target == "" {
		return "", fmt.Errorf("%s has an empty gitdir pointer", gitPath)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, nil
}

// commonDirFrom returns the common git directory for a per-worktree git
// directory: its "commondir" file, resolved, or gitDir itself.
func commonDirFrom(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	rel := strings.TrimSpace(string(data))
	if rel == "" {
		return gitDir
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(gitDir, rel)
}

func absClean(path, base string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

// gitEnvValue returns the value of the last matching KEY=... entry in env,
// or "".
func gitEnvValue(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			val = v
		}
	}
	return val
}
