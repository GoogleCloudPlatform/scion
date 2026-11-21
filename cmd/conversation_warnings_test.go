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
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractQuotedScionCommands finds every backtick-quoted `scion ...` or
// single-quoted 'scion ...' span in text and returns the subcommand tokens
// (after "scion", stopping before flags or placeholders).
func extractQuotedScionCommands(text string) [][]string {
	var out [][]string
	for _, quote := range []byte{'`', '\''} {
		s := text
		for {
			open := strings.IndexByte(s, quote)
			if open < 0 {
				break
			}
			rest := s[open+1:]
			closeIdx := strings.IndexByte(rest, quote)
			if closeIdx < 0 {
				break
			}
			span := strings.TrimSpace(rest[:closeIdx])
			s = rest[closeIdx+1:]
			if !strings.HasPrefix(span, "scion ") {
				continue
			}
			fields := strings.Fields(span)[1:]
			var subArgs []string
			for _, f := range fields {
				if strings.HasPrefix(f, "-") || strings.ContainsAny(f, "<>$%[]") || f == "..." {
					break
				}
				subArgs = append(subArgs, f)
			}
			if len(subArgs) > 0 {
				out = append(out, subArgs)
			}
		}
	}
	return out
}

// validateScionCommandRefs checks that each extracted subcommand path resolves
// to a real cobra command in rootCmd without leaving unconsumed subcommand tokens.
func validateScionCommandRefs(refs [][]string, source string) []string {
	var problems []string
	for _, subArgs := range refs {
		cmd, rest, err := rootCmd.Find(subArgs)
		if err != nil {
			problems = append(problems,
				fmt.Sprintf("command not found: scion %s (from %s)", strings.Join(subArgs, " "), source))
			continue
		}
		if len(rest) > 0 && cmd.HasSubCommands() && !acceptsPositionalArgs(cmd) {
			problems = append(problems,
				fmt.Sprintf("unknown subcommand %q on %q: scion %s (from %s)",
					rest[0], cmd.CommandPath(), strings.Join(subArgs, " "), source))
			continue
		}
		if cmd.Name() != subArgs[len(subArgs)-1] && !acceptsPositionalArgs(cmd) {
			problems = append(problems,
				fmt.Sprintf("command scion %s resolved to %q instead of %q (from %s)",
					strings.Join(subArgs, " "), cmd.CommandPath(), subArgs[len(subArgs)-1], source))
		} else if cmd.Name() != subArgs[len(subArgs)-1] && cmd.HasSubCommands() {
			// Runnable group command such as `scion conversation` or `scion messages`:
			// a subcommand token after it must actually exist on that group.
			problems = append(problems,
				fmt.Sprintf("unknown subcommand %q on runnable group %q: scion %s (from %s)",
					rest[0], cmd.CommandPath(), strings.Join(subArgs, " "), source))
		}
	}
	return problems
}

func TestRunConversationCreate_EmptyParticipantsWarningNamesGet(t *testing.T) {
	orig := saveConversationCreateTestState()
	defer orig.restore()

	const convID = "44444444-4444-4444-4444-444444444444"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/conversations" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(store.Conversation{
				ID:          convID,
				DisplayName: "Design Discussion",
				Kind:        "group",
				Surface:     "native",
				DriftState:  "active",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "")
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, server.URL, "proj-8366")
	convProject = ""
	convCreateJSON = false
	outputFormat = ""

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	var runErr error
	_, stderr := captureStdoutStderr(t, func() {
		runErr = runConversationCreate(cmd, []string{"Design Discussion"})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stderr, "`scion conversation list`")
	assert.Contains(t, stderr, "`scion conversation get`")
	assert.NotContains(t, stderr, "conversation show")

	refs := extractQuotedScionCommands(stderr)
	require.Len(t, refs, 2, "expected two quoted scion commands in warning: %q", stderr)
	assert.Empty(t, validateScionCommandRefs(refs, "runConversationCreate warning"))
}

// TestUserFacingWarningsReferenceExistingCommands scans user-facing warning
// strings in cmd/*.go and verifies every quoted `scion ...` or 'scion ...'
// command resolves in rootCmd (ptone/scion#8366).
func TestUserFacingWarningsReferenceExistingCommands(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		require.NoError(t, parseErr, "parsing %s", name)

		ast.Inspect(file, func(n ast.Node) bool {
			s, ok := evalStringExpr(n)
			if !ok {
				return true
			}
			lower := strings.ToLower(s)
			if !strings.Contains(lower, "warning:") {
				return false
			}
			refs := extractQuotedScionCommands(s)
			if len(refs) > 0 {
				checked += len(refs)
				pos := fset.Position(n.Pos())
				for _, p := range validateScionCommandRefs(refs, fmt.Sprintf("%s:%d", pos.Filename, pos.Line)) {
					t.Error(p)
				}
			}
			return false
		})
	}
	require.GreaterOrEqual(t, checked, 5, "expected at least 5 quoted scion commands across cmd/*.go warnings")

	t.Run("catches_nonexistent_conversation_show", func(t *testing.T) {
		refs := extractQuotedScionCommands("warning: not readable via `scion conversation list` or `scion conversation show` yet")
		require.Len(t, refs, 2)
		problems := validateScionCommandRefs(refs, "test")
		assert.NotEmpty(t, problems, "must reject nonexistent `scion conversation show`")
	})
}

func evalStringExpr(n ast.Node) (string, bool) {
	switch expr := n.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		val, err := strconv.Unquote(expr.Value)
		return val, err == nil
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, okL := evalStringExpr(expr.X)
		right, okR := evalStringExpr(expr.Y)
		if !okL || !okR {
			return "", false
		}
		return left + right, true
	default:
		return "", false
	}
}
