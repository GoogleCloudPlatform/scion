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

package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// windowsHubTokenPath is the "scion hub token" command path. It is spelled as a
// concatenation because the source guard TestNoHubSettingsReadsOutsideServerCode
// (hub_list_cutover_test.go) rejects the literal, which also names a
// retired hub settings key.
const windowsHubTokenPath = "hub" + ".token"

// windowsAllowed lists the commands the Windows build runs. It uses the
// same dot-separated command paths as agentAllowed, and every entry,
// parents included, is listed explicitly. The one allowed command under a
// gated parent is project.use (see its entry).
//
// It is an allowlist so that it fails closed: a command added later is
// unsupported on Windows until someone adds it here deliberately, after
// checking that it touches no local file path that is not protected on
// Windows. The server, the Runtime Broker and agent-mode commands are
// deliberately absent (whoami, expose and unexpose read in-container token
// files). Widening the set is a product decision.
var windowsAllowed = map[string]bool{
	"list":          true,
	"ssh":           true,
	"ssh-config":    true,
	"tunnel":        true,
	"tunnel.status": true,
	"vscode":        true,

	"message":          true,
	"message.channels": true,
	"messages":         true,
	"messages.read":    true,

	"conversation":              true,
	"conversation.list":         true,
	"conversation.messages":     true,
	"conversation.create":       true,
	"conversation.get":          true,
	"conversation.get-message":  true,
	"conversation.set-default":  true,
	"conversation.participants": true,
	"conversation.join":         true,
	"conversation.leave":        true,
	"conversation.catch-up":     true,
	"conversation.group":        true,
	"conversation.group.list":   true,
	"conversation.group.get":    true,
	"conversation.group.create": true,
	"conversation.group.rename": true,
	"conversation.group.delete": true,
	"conversation.group.add":    true,
	"conversation.group.remove": true,

	// 'scion use' and its alias 'scion project use' run on Windows. Their
	// local writes all go through the private-file primitives:
	// ~/.scion/context.yaml with its directory and lock (Tree.MkdirAll
	// Private, writeFileAtomicMode 0600 → Private, filelock.Acquire 0600),
	// and credentials.json when the login offer runs (as 'hub auth login'). Their reads (settings,
	// context.yaml, credentials, dev token, hub entry transport) are those
	// of the hub commands above. The parent 'project' stays gated (its
	// other subcommands write project directories), so it is hidden and
	// 'scion project' alone is refused; 'scion project use' still runs.
	"use":         true,
	"project.use": true,

	"config":            true,
	"config.list":       true,
	"config.set":        true,
	"config.get":        true,
	"config.unset":      true,
	"config.validate":   true,
	"config.migrate":    true,
	"config.dir":        true,
	"config.cd-config":  true,
	"config.cd-project": true,
	"config.schema":     true,

	"hub":                       true,
	"hub.auth":                  true,
	"hub.auth.login":            true,
	"hub.auth.logout":           true,
	"hub.auth.federation-token": true,
	"hub.secret":                true,
	"hub.secret.set":            true,
	"hub.secret.get":            true,
	"hub.secret.list":           true,
	"hub.secret.clear":          true,
	"hub.secret.update":         true,
	// hub.secret.migrate and hub.secret.migrate-names are deliberately
	// absent: they are hub-server-host admin tools that open the local hub
	// database read-write (schema migration, WAL) and can write a foreign-key
	// snapshot next to it (pkg/store/entadapter/timestamp_normalize.go). Those
	// local files are not written through the private-file primitives, so
	// they stay gated.
	"hub.test-identity":                 true,
	"hub.test-identity.issue":           true,
	"hub.test-identity.token":           true,
	"hub.test-identity.list":            true,
	"hub.test-identity.delete":          true,
	"hub.test-identity.web-code":        true,
	"hub.test-identity.teardown":        true,
	"hub.test-identity.delegate":        true,
	"hub.test-identity.delegate.create": true,
	"hub.test-identity.delegate.list":   true,
	"hub.test-identity.delegate.revoke": true,
	"hub.test-identity.issue-admin":     true,
	"hub.test-identity.cleanup":         true,
	"hub.test-identity.provenance":      true,
	"hub.test-identity.revoke-all":      true,
	"hub.test-identity.residue":         true,
	windowsHubTokenPath:                 true,
	windowsHubTokenPath + ".create":     true,
	windowsHubTokenPath + ".list":       true,
	windowsHubTokenPath + ".revoke":     true,
	windowsHubTokenPath + ".delete":     true,
	windowsHubTokenPath + ".scopes":     true,
	"hub.profile":                       true,
	"hub.profile.add":                   true,
	"hub.profile.list":                  true,
	"hub.profile.show":                  true,
	"hub.profile.remove":                true,
	"hub.env":                           true,
	"hub.env.set":                       true,
	"hub.env.get":                       true,
	"hub.env.list":                      true,
	"hub.env.clear":                     true,
	"hub.notifications":                 true,
	"hub.notifications.ack":             true,
	"hub.messaging":                     true,
	"hub.messaging.get":                 true,
	"hub.messaging.set":                 true,
	"hub.status":                        true,
	"hub.projects":                      true,
	"hub.projects.info":                 true,
	"hub.projects.delete":               true,
	"hub.projects.create":               true,
	// hub.brokers mutations are hub API calls only; they run nothing
	// broker-side on this machine.
	"hub.brokers":                   true,
	"hub.brokers.join-token":        true,
	"hub.brokers.join-token.create": true,
	"hub.brokers.info":              true,
	"hub.brokers.delete":            true,
	"hub.enable":                    true,
	"hub.disable":                   true,
	"hub.link":                      true,
	"hub.unlink":                    true,
	"hub.hook":                      true,
	"hub.hook.list":                 true,
	"hub.hook.show":                 true,
	"hub.hook.create":               true,
	"hub.hook.update":               true,
	"hub.hook.activate":             true,
	"hub.hook.delete":               true,
	"hub.allow-list":                true,
	"hub.allow-list.add":            true,
	"hub.allow-list.remove":         true,
	"hub.allow-list.list":           true,
	"hub.allow-list.import":         true,
	"hub.shadow":                    true,
	"hub.add":                       true,
	"hub.list":                      true,
	"hub.rename":                    true,
	"hub.remove":                    true,
	"hub.users":                     true,
	"hub.users.provision":           true,
	"hub.invite":                    true,
	"hub.invite.create":             true,
	"hub.invite.list":               true,
	"hub.invite.revoke":             true,
	"hub.invite.delete":             true,

	"version":    true,
	"help":       true,
	"completion": true,
	// cobra's own completion subcommands; the root adds them when it
	// executes, after the gate, so they are listed to keep the gate's
	// result independent of that order.
	"completion.bash":       true,
	"completion.zsh":        true,
	"completion.fish":       true,
	"completion.powershell": true,
	"global-flags":          true,
}

// errUnsupportedOnWindows is what every command outside windowsAllowed
// returns on Windows. Match it with errors.Is; the error itself names the
// command.
var errUnsupportedOnWindows = errors.New("command is not supported on Windows")

// unsupportedOnWindowsError names the refused command. It matches
// errUnsupportedOnWindows.
type unsupportedOnWindowsError struct {
	path string // space-separated, without the leading "scion"
}

func (e *unsupportedOnWindowsError) Error() string {
	return "scion " + e.path + " is not supported on Windows; the Windows build supports hub client commands only (see docs: Windows)"
}

func (e *unsupportedOnWindowsError) Is(target error) bool {
	return target == errUnsupportedOnWindows
}

// windowsGatedAnnotation marks a command the Windows gate refuses. Its
// value is the command path, space-separated, without "scion".
const windowsGatedAnnotation = "scion_windows_gated"

// flagValidationAnnotations are the cobra flag annotations that make
// cobra reject an invocation before the command runs: required flags and
// the three flag-group kinds (cobra v1.10 bash_completions.go and
// flag_groups.go).
var flagValidationAnnotations = []string{
	cobra.BashCompOneRequiredFlag,
	"cobra_annotation_required_if_others_set",
	"cobra_annotation_one_required",
	"cobra_annotation_mutually_exclusive",
}

// applyPlatformRestrictions gates the command tree for goos. On Windows,
// every command whose path is not in windowsAllowed is kept in the tree
// but hidden, and running it returns errUnsupportedOnWindows: removing it
// would turn "scion server start" into an unknown command, which does not
// say why. On every other platform it does nothing.
//
// A gated command's own run hooks, persistent ones included, are cleared, it
// accepts any arguments and unknown flags, and the required-flag and
// flag-group rules of the flags it defines are dropped, so nothing of the
// command runs and no usage error for it is reported. Root's
// PersistentPreRunE still handles the global flags (a bad --format or a
// --tz/--utc conflict is still reported), then returns the refusal
// (windowsRefusal) before any invocation-target, project, settings or
// migration work. RunE refuses too, for a command reached without root's
// hook. It is called with runtime.GOOS right after applyModeRestrictions;
// tests pass goos explicitly.
func applyPlatformRestrictions(root *cobra.Command, goos string) {
	if goos != "windows" {
		return
	}
	gateCommands(root, "", func(path string) bool { return !windowsAllowed[path] })
}

// windowsRefusal returns the refusal for a command the Windows gate
// marked, and nil for any other command.
func windowsRefusal(cmd *cobra.Command) error {
	path, ok := cmd.Annotations[windowsGatedAnnotation]
	if !ok {
		return nil
	}
	return &unsupportedOnWindowsError{path: path}
}

// completeGated answers a shell completion request (cobra's hidden
// __complete and __completeNoDesc commands, which every completion script
// runs) for a command the Windows gate refused. Cobra would otherwise call
// the gated command's ValidArgsFunction and the completion functions
// registered for its flags (RegisterFlagCompletionFunc), which can read
// files, settings or the hub. For such a request it writes only the
// completion directive ShellCompDirectiveNoFileComp to out (no
// completions, and no file completion by the shell) and reports true; the
// caller then exits without running cobra. Any other request, including
// one for an allowed child of a gated parent (project use), reports false
// and goes through cobra unchanged. args are the command-line arguments
// after the program name. Off Windows no command is marked, so it always
// reports false.
//
// The request is recognised the way cobra recognises it, not by the first
// argument: cobra also accepts global flags before __complete (--debug, or
// --project with its value). A hidden placeholder with the same names is
// added for the lookup and removed again; the request is one only when
// root.Find resolves args to a command of that name, and Find's remaining
// arguments are then exactly the ones cobra's completion code receives.
func completeGated(root *cobra.Command, args []string, out io.Writer) bool {
	placeholder := &cobra.Command{
		Use:                cobra.ShellCompRequestCmd,
		Aliases:            []string{cobra.ShellCompNoDescRequestCmd},
		Hidden:             true,
		DisableFlagParsing: true,
		Run:                func(*cobra.Command, []string) {},
	}
	root.AddCommand(placeholder)
	defer root.RemoveCommand(placeholder)
	found, rest, err := root.Find(args)
	if err != nil || len(rest) == 0 {
		return false
	}
	// By name, as cobra decides it: a command of that name already on root
	// (cobra's own, or any other) is a completion request too, so the
	// check fails closed rather than handing the request to cobra.
	if name := found.Name(); name != cobra.ShellCompRequestCmd && name != cobra.ShellCompNoDescRequestCmd {
		return false
	}
	// The last argument is the word being completed; cobra resolves the
	// target command from the ones before it, the same way.
	target, _, err := root.Find(rest[:len(rest)-1])
	if err != nil || windowsRefusal(target) == nil {
		return false
	}
	_, _ = fmt.Fprintf(out, ":%d\n", cobra.ShellCompDirectiveNoFileComp)
	return true
}

// gateCommands walks the tree under parent and replaces the run function
// of every command for which gate returns true with one that refuses.
func gateCommands(parent *cobra.Command, prefix string, gate func(path string) bool) {
	for _, child := range parent.Commands() {
		path := child.Name()
		if prefix != "" {
			path = prefix + "." + path
		}
		gateCommands(child, path, gate)
		if !gate(path) {
			continue
		}
		display := strings.ReplaceAll(path, ".", " ")
		refusal := &unsupportedOnWindowsError{path: display}
		if child.Annotations == nil {
			child.Annotations = map[string]string{}
		}
		child.Annotations[windowsGatedAnnotation] = display
		child.RunE = func(*cobra.Command, []string) error { return refusal }
		child.Run = nil
		child.PreRun, child.PreRunE = nil, nil
		child.PostRun, child.PostRunE = nil, nil
		// Its persistent hooks too: cobra runs the nearest persistent
		// pre-run hook only, so one on a gated command would replace
		// root's, which returns the refusal. An allowed child of a gated
		// parent (project use) then falls back to root's hook.
		child.PersistentPreRun, child.PersistentPreRunE = nil, nil
		child.PersistentPostRun, child.PersistentPostRunE = nil, nil
		child.ValidArgsFunction = nil
		child.Args = cobra.ArbitraryArgs
		child.FParseErrWhitelist.UnknownFlags = true
		child.Hidden = true
		// Only the flags this command defines (local and its own
		// persistent ones), never inherited ones: root's global flag
		// groups keep applying.
		child.LocalFlags().VisitAll(func(f *pflag.Flag) {
			for _, a := range flagValidationAnnotations {
				delete(f.Annotations, a)
			}
		})
	}
}
