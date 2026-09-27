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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestCommandTreeHasNoGroveTerms walks the whole scion command tree,
// including hidden commands, and fails if any command or flag surface still
// mentions the retired "grove" term. Hidden commands and hidden flags are
// still surfaces a caller can invoke, so they are checked exactly like
// visible ones. Every violation found is reported together, naming the full
// command path and the offending field, rather than stopping at the first
// one.
func TestCommandTreeHasNoGroveTerms(t *testing.T) {
	var violations []string

	record := func(path, field, value string) {
		violations = append(violations, path+": "+field+" contains \"grove\" (value: "+value+")")
	}

	checkString := func(path, field, value string) {
		if strings.Contains(strings.ToLower(value), "grove") {
			record(path, field, value)
		}
	}

	// seen dedupes flags across the walk so a single persistent flag
	// defined high in the tree is reported at most once, at the command
	// that owns it, instead of once per descendant that inherits it.
	seen := map[*pflag.Flag]bool{}
	checkFlag := func(path, kind string, f *pflag.Flag) {
		if seen[f] {
			return
		}
		seen[f] = true
		checkString(path, kind+" flag --"+f.Name+" Name", f.Name)
		checkString(path, kind+" flag --"+f.Name+" Shorthand", f.Shorthand)
		checkString(path, kind+" flag --"+f.Name+" Usage", f.Usage)
		checkString(path, kind+" flag --"+f.Name+" Deprecated", f.Deprecated)
		checkString(path, kind+" flag --"+f.Name+" ShorthandDeprecated", f.ShorthandDeprecated)
	}

	commandCount := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		commandCount++
		path := c.CommandPath()

		checkString(path, "Use", c.Use)
		checkString(path, "Short", c.Short)
		checkString(path, "Long", c.Long)
		checkString(path, "Example", c.Example)
		checkString(path, "Deprecated", c.Deprecated)
		for _, alias := range c.Aliases {
			checkString(path, "Aliases", alias)
		}
		for _, s := range c.SuggestFor {
			checkString(path, "SuggestFor", s)
		}

		// LocalFlags covers both flags registered on this command's own
		// Flags() (local) and its own PersistentFlags() (persistent);
		// InheritedFlags covers persistent flags this command picked up from
		// an ancestor. Neither call mutates parse state, so this is cheap to
		// do for every command, including hidden ones. The walk visits
		// parents before children, so checkFlag's seen map reports each
		// flag once, at the command where it is defined or first
		// encountered.
		pflags := c.PersistentFlags()
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			kind := "local"
			if pflags.Lookup(f.Name) != nil {
				kind = "persistent"
			}
			checkFlag(path, kind, f)
		})
		c.InheritedFlags().VisitAll(func(f *pflag.Flag) {
			checkFlag(path, "inherited", f)
		})

		for _, child := range c.Commands() {
			walk(child)
		}
	}

	walk(rootCmd)

	// Guard against a vacuous pass: if command registration ever moved out
	// of package init() (for example to lazy or Execute-time registration),
	// the walk above would visit only rootCmd and still report zero
	// violations. Require a populated tree, including a known subcommand.
	if commandCount < 2 {
		t.Fatalf("walked only %d command(s); command tree not populated", commandCount)
	}
	if _, _, err := rootCmd.Find([]string{"project"}); err != nil {
		t.Fatalf("rootCmd.Find([]string{\"project\"}) failed, command tree not populated: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("found %d \"grove\" surface(s) in the command tree:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
