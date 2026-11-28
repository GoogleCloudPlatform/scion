package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestScriptsBash32Compatible statically checks every cr-ci/scripts/*.sh file
// for bash 4+ constructs so the deploy scripts stay compatible with macOS
// /bin/bash (3.2).
func TestScriptsBash32Compatible(t *testing.T) {
	files, err := filepath.Glob("*.sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no *.sh files found in cr-ci/scripts")
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash is required")
	}

	forbidden := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"associative/global/nameref/case declare", regexp.MustCompile(`\b(declare|typeset|local)\s+-[a-zA-Z]*[Aglun]`)},
		{"mapfile or readarray", regexp.MustCompile(`\b(mapfile|readarray)\b`)},
		{"coproc", regexp.MustCompile(`\bcoproc\b`)},
		{"case-modification expansion (,,/^^)", regexp.MustCompile(`\$\{[^}]*(,,|\^\^)\}`)},
		{"conditional -v test", regexp.MustCompile(`\[\[[^\]]*\s-v\s`)},
		{"|& pipe stderr", regexp.MustCompile(`\|&`)},
		{"&>> append redirect", regexp.MustCompile(`&>>`)},
		{"case fallthrough (;& / ;;&)", regexp.MustCompile(`;;?&`)},
		{"negative array index", regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\[-[0-9]+\]\}`)},
		{"bash >= 4 version gate", regexp.MustCompile(`BASH_VERSINFO\[0\]\s*<\s*4`)},
	}

	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			cmd := exec.Command(bash, "-n", f)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("bash -n %s failed: %v\n%s", f, err, out)
			}

			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				for _, rule := range forbidden {
					if rule.re.MatchString(line) {
						t.Errorf("%s:%d uses bash 4+ construct (%s): %s", f, i+1, rule.name, trimmed)
					}
				}
			}
		})
	}
}
