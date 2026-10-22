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

package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestSecretReadCommandsMoved checks that sciontool secret get and list are
// hidden stubs that fail with a message naming the scion command, whatever
// arguments and flags they are given, and that secret set is still a real
// command (ptone/scion#5013).
func TestSecretReadCommandsMoved(t *testing.T) {
	sub := func(name string) *cobra.Command {
		for _, c := range secretCmd.Commands() {
			if c.Name() == name {
				return c
			}
		}
		return nil
	}

	for name, target := range map[string]string{"get": "secret get --value", "list": "secret list"} {
		t.Run(name, func(t *testing.T) {
			stub := sub(name)
			if stub == nil {
				t.Fatalf("sciontool secret %s is not registered", name)
			}
			if !stub.Hidden {
				t.Errorf("sciontool secret %s should be hidden", name)
			}
			if !stub.DisableFlagParsing {
				t.Errorf("sciontool secret %s should accept any flags", name)
			}
			err := stub.RunE(stub, []string{"MY_KEY", "--scope", "user"})
			if err == nil {
				t.Fatalf("sciontool secret %s should fail", name)
			}
			want := "sciontool secret " + name + " has moved to 'scion " + target + "'"
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
		})
	}

	set := sub("set")
	if set == nil {
		t.Fatal("sciontool secret set must stay registered")
	}
	if set.Hidden || set.Run == nil {
		t.Error("sciontool secret set must stay a visible, working command")
	}
}
