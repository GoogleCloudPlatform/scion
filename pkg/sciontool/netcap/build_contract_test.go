/*
Copyright 2026 The Scion Authors.
*/

package netcap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DropNetAdmin uses syscall.AllThreadsSyscall, which returns ENOTSUP in a
// binary that links cgo, so sciontool must be built with CGO_ENABLED=0 on
// every path that produces it. TestShippedSciontoolBuildsDisableCgo pins
// that in the build definitions themselves, so a change that lets either
// path link cgo again fails here, in normal CI, before any image is built:
//
//   - image-build/scion-base/Dockerfile builds the sciontool that ships to
//     agents (the image's PID 1) and runs image-build/lib/assert-no-cgo.sh
//     on it;
//   - the Makefile's container-sciontool target (a development path) builds
//     it the same way and runs the same check.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// logicalLines joins backslash-continued lines, so a shell command split
// across lines is checked as one command.
func logicalLines(content string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		out = append(out, cur.String())
		cur.Reset()
	}
	return out
}

// makeRecipe returns the logical recipe lines of target in a Makefile.
func makeRecipe(t *testing.T, makefile, target string) []string {
	t.Helper()
	lines := logicalLines(makefile)
	for i, l := range lines {
		if strings.HasPrefix(l, target+":") {
			var recipe []string
			for _, r := range lines[i+1:] {
				if !strings.HasPrefix(r, "\t") {
					break
				}
				recipe = append(recipe, r)
			}
			return recipe
		}
	}
	t.Fatalf("Makefile has no %s target", target)
	return nil
}

func TestShippedSciontoolBuildsDisableCgo(t *testing.T) {
	t.Run("scion-base Dockerfile", func(t *testing.T) {
		df := repoFile(t, "image-build/scion-base/Dockerfile")
		var build []string
		for _, l := range logicalLines(df) {
			if strings.Contains(l, "-o /usr/local/bin/sciontool") {
				build = append(build, l)
			}
		}
		if len(build) != 1 {
			t.Fatalf("found %d sciontool build commands, want exactly 1", len(build))
		}
		// The command is one RUN with several && steps; check the step that
		// builds sciontool, then the guard step right after it.
		steps := strings.Split(build[0], "&&")
		buildIdx := -1
		for i, step := range steps {
			if strings.Contains(step, "-o /usr/local/bin/sciontool") {
				buildIdx = i
			}
		}
		if buildIdx < 0 {
			t.Fatal("no && step of the sciontool build command writes -o /usr/local/bin/sciontool")
		}
		buildStep := strings.TrimSpace(steps[buildIdx])
		if !strings.HasPrefix(buildStep, "CGO_ENABLED=0 go build ") {
			t.Errorf("sciontool build step does not start with CGO_ENABLED=0 go build: %q", buildStep)
		}
		// The guard must be the very next step and must be able to fail the
		// build: an "|| true" anywhere in the RUN would let a cgo-linked
		// sciontool through.
		const guard = "bash /opt/scion-image-build/lib/assert-no-cgo.sh /usr/local/bin/sciontool"
		if buildIdx+1 >= len(steps) || strings.TrimSpace(steps[buildIdx+1]) != guard {
			t.Errorf("the step after the sciontool build is not exactly %q", guard)
		}
		if strings.Contains(build[0], "||") {
			t.Error("the RUN that builds sciontool contains ||, which could mask a failing assert-no-cgo.sh")
		}
		if !strings.Contains(df, "COPY image-build/lib/assert-no-cgo.sh ") {
			t.Error("the builder stage does not copy in assert-no-cgo.sh")
		}
	})

	t.Run("Makefile container-sciontool", func(t *testing.T) {
		const makeGuard = "bash image-build/lib/assert-no-cgo.sh $(CONTAINER_DIR)/sciontool"
		recipe := makeRecipe(t, repoFile(t, "Makefile"), "container-sciontool")
		buildIdx, guardIdx := -1, -1
		for i, r := range recipe {
			if strings.Contains(r, "go build") && strings.Contains(r, "./cmd/sciontool") {
				buildIdx = i
				if !strings.Contains(r, "CGO_ENABLED=0") {
					t.Errorf("container-sciontool builds without CGO_ENABLED=0: %q", strings.TrimSpace(r))
				}
			}
			if strings.Contains(r, "assert-no-cgo.sh") {
				guardIdx = i
				// make ignores a recipe line's failure when its prefix holds
				// a '-', and it reads that prefix after skipping blanks and
				// after expanding variables. So the guard is pinned exactly:
				// only @, + and blanks may precede the command, and nothing
				// (such as "|| true") may follow it.
				if body := strings.TrimLeft(r, "@+ \t"); body != makeGuard {
					t.Errorf("container-sciontool guard line is not exactly %q: %q", makeGuard, strings.TrimSpace(r))
				}
			}
		}
		if buildIdx < 0 {
			t.Fatal("container-sciontool has no go build of ./cmd/sciontool")
		}
		if guardIdx < 0 {
			t.Fatal("container-sciontool does not run assert-no-cgo.sh on its output")
		}
		if guardIdx <= buildIdx {
			t.Error("container-sciontool does not run assert-no-cgo.sh after it builds sciontool")
		}
	})
}
