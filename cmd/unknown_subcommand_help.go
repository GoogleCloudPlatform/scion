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
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// unknownSubcommandHelpError reports "<group> <unknown> --help" as an
// unknown command (ptone/scion#4579).
//
// Command groups such as artifact, hub and config are Runnable with an Args
// validator that rejects positional arguments, so "scion artifact nosuch"
// fails with "unknown command". Cobra handles --help before it validates
// arguments, though, so "scion artifact nosuch --help" printed the group's
// help and exited 0, and a caller probing for a command got a false
// positive.
//
// It returns an error only when every one of these holds, so every other
// help path is unchanged: args ask for help (--help or -h, before any
// "--"); the command args resolve to has subcommands and is Runnable; at
// least one positional argument is left once flags and their values are
// removed (Find returns the leftover arguments with their flags); and the
// command's own Args validator rejects those positional arguments with an
// error other than pflag.ErrHelp (which the groups use for "help" and for
// no arguments). A command that accepts positional arguments passes its
// validator and is untouched.
func unknownSubcommandHelpError(root *cobra.Command, args []string) error {
	if !helpRequested(args) {
		return nil
	}
	target, rest, err := root.Find(args)
	if err != nil || target == nil || !target.HasSubCommands() || !target.Runnable() {
		return nil
	}
	positional := positionalArgs(target, rest)
	if len(positional) == 0 {
		return nil
	}
	if verr := target.ValidateArgs(positional); verr == nil || errors.Is(verr, pflag.ErrHelp) {
		return nil
	}
	return fmt.Errorf("unknown command %q for %q", positional[0], target.CommandPath())
}

// helpRequested reports whether args hold --help or -h before a "--"
// terminator.
func helpRequested(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--help", "-h":
			return true
		}
	}
	return false
}

// positionalArgs returns the positional arguments of args for cmd: args
// with flags and their values removed, up to a "--" terminator. It follows
// cobra's own stripFlags (which Find uses but does not export): a long flag
// written "--name value" or a short flag "-n value" consumes the next
// argument unless the flag is a boolean (or has another no-value default);
// "--name=value" and combined short flags consume nothing more. Flags are
// looked up among cmd's own and inherited flags; --help and -h are
// booleans, though cobra registers the help flag only when the command
// runs. It reads the flag sets and parses nothing.
func positionalArgs(cmd *cobra.Command, args []string) []string {
	local, inherited := cmd.Flags(), cmd.InheritedFlags()
	lookup := func(name string) *pflag.Flag {
		if f := local.Lookup(name); f != nil {
			return f
		}
		return inherited.Lookup(name)
	}
	lookupShort := func(name string) *pflag.Flag {
		if f := local.ShorthandLookup(name); f != nil {
			return f
		}
		return inherited.ShorthandLookup(name)
	}
	takesValue := func(f *pflag.Flag) bool { return f == nil || f.NoOptDefVal == "" }
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return out
		case a == "--help" || a == "-h":
		case strings.HasPrefix(a, "--") && !strings.Contains(a, "="):
			if takesValue(lookup(a[2:])) {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) == 2 && a != "-":
			if takesValue(lookupShort(a[1:])) {
				i++
			}
		case strings.HasPrefix(a, "-"):
		case a != "":
			out = append(out, a)
		}
	}
	return out
}
