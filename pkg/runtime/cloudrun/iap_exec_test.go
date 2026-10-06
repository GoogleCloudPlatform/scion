package cloudrun

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// shParse runs input through a real POSIX shell (sh, invoked locally — no
// ssh, no network) and returns the argv it produces from `set -- <input>`.
// This mirrors what a remote login shell does with the command string ssh
// hands it, letting the tests assert on shell-parsed results rather than on
// assumptions about shell grammar.
func shParse(t *testing.T, input string) []string {
	t.Helper()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("skipping: shell-parsing test requires a POSIX sh in PATH: %v", err)
	}

	script := "set -- " + input + "; for a in \"$@\"; do printf '%s\\000' \"$a\"; done"
	cmd := exec.Command("sh", "-c", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c failed: %v (stderr: %s)", err, stderr.String())
	}

	parts := strings.Split(out.String(), "\x00")
	// Trailing element after the last NUL separator is always empty.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func TestShellQuoteRoundTripsAsOneLiteralWord(t *testing.T) {
	cases := []string{
		"plain",
		"has spaces",
		"semi;colon",
		"pipe|char",
		"$(command substitution)",
		"`backticks`",
		"and&&and",
		"quote's here",
		"",
		"--flag=value with spaces",
		"newline\nembedded",
	}

	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			quoted := shellQuote(want)
			got := shParse(t, quoted)
			if len(got) != 1 {
				t.Fatalf("shellQuote(%q) = %q, shell parsed it into %d words: %q", want, quoted, len(got), got)
			}
			if got[0] != want {
				t.Fatalf("shellQuote(%q) = %q, round-tripped to %q", want, quoted, got[0])
			}
		})
	}
}

func TestQuoteRemoteCommandPreservesEachArgumentLiterally(t *testing.T) {
	args := []string{
		"echo",
		"hello world",
		"a;b|c&&d",
		"$(whoami)",
		"it's fine",
		"",
	}

	joined := quoteRemoteCommand(args)
	got := shParse(t, joined)

	if len(got) != len(args) {
		t.Fatalf("quoteRemoteCommand(%q) = %q, shell parsed it into %d words, want %d: %q", args, joined, len(got), len(args), got)
	}
	for i, want := range args {
		if got[i] != want {
			t.Fatalf("argument %d: shell parsed %q, want %q (joined command: %q)", i, got[i], want, joined)
		}
	}
}

func TestQuoteRemoteCommandEmpty(t *testing.T) {
	if got := quoteRemoteCommand(nil); got != "" {
		t.Fatalf("quoteRemoteCommand(nil) = %q, want empty string", got)
	}
}

func TestAppendRemoteCommandNilLeavesArgsUnchanged(t *testing.T) {
	base := []string{"-p", "2222", "-i", "keyfile", "user@host"}

	got := appendRemoteCommand(base, nil)

	if len(got) != len(base) {
		t.Fatalf("appendRemoteCommand with nil cmdArgs changed arg count: got %d args %q, want %d args %q", len(got), got, len(base), base)
	}
	for i := range base {
		if got[i] != base[i] {
			t.Fatalf("appendRemoteCommand with nil cmdArgs changed arg %d: got %q, want %q", i, got[i], base[i])
		}
	}
}

func TestAppendRemoteCommandAddsExactlyOneElement(t *testing.T) {
	base := []string{"-p", "2222", "-i", "keyfile", "user@host"}
	cmdArgs := []string{"ls", "-la", "some dir; echo marker"}

	got := appendRemoteCommand(base, cmdArgs)

	if len(got) != len(base)+1 {
		t.Fatalf("appendRemoteCommand added %d elements, want exactly 1: got %q", len(got)-len(base), got)
	}
	for i, want := range base {
		if got[i] != want {
			t.Fatalf("appendRemoteCommand changed a preceding arg %d: got %q, want %q", i, got[i], want)
		}
	}

	remote := got[len(got)-1]
	parsed := shParse(t, remote)
	if len(parsed) != len(cmdArgs) {
		t.Fatalf("remote command element %q parsed into %d words, want %d: %q", remote, len(parsed), len(cmdArgs), parsed)
	}
	for i, want := range cmdArgs {
		if parsed[i] != want {
			t.Fatalf("remote command argument %d: shell parsed %q, want %q", i, parsed[i], want)
		}
	}
}
