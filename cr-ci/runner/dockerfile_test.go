package runner

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// dockerfileStage returns the instruction lines of the Dockerfile's stage that
// starts with the FROM line from, up to the next FROM: CRLF normalised to LF,
// comment lines dropped BEFORE backslash continuations are joined (as Docker does,
// so a comment inside a continued RUN can neither satisfy a check nor end the
// instruction), then continuations joined, so each RUN step is one line.
func dockerfileStage(t *testing.T, from string) []string {
	t.Helper()
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			kept = append(kept, l)
		}
	}
	df := strings.ReplaceAll(strings.Join(kept, "\n"), "\\\n", " ")
	start := strings.Index(df, from+"\n")
	if start < 0 {
		t.Fatalf("Dockerfile has no %q line", from)
	}
	stage := df[start+len(from)+1:]
	if end := strings.Index(stage, "\nFROM "); end >= 0 {
		stage = stage[:end]
	}
	var lines []string
	for _, l := range strings.Split(stage, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestDockerfileGitStage: git is built from source without Rust. git 2.55+ builds
// its Rust parts (libgitcore.a, through cargo) unless NO_RUST is set, and the
// stage has no Rust toolchain, so a make call without NO_RUST fails the image
// build ("cargo: not found", Makefile target target/release/libgitcore.a). The
// stage's last command must confirm the result ("rust: disabled"), no Rust tool
// may appear in it, and the final stage must keep its asserts: git >= 2.48 and the
// HTTPS transport's runtime libraries.
func TestDockerfileGitStage(t *testing.T) {
	git := dockerfileStage(t, "FROM ${UBUNTU_IMAGE} AS git")
	body := strings.Join(git, "\n")

	makes := regexp.MustCompile(`make -C /tmp/git-src [^&]*`).FindAllString(body, -1)
	if len(makes) != 2 {
		t.Fatalf("git stage: %d make calls, want 2 (all, install): %q", len(makes), makes)
	}
	for _, m := range makes {
		if !strings.Contains(m, " NO_RUST=YesPlease ") {
			t.Errorf("git stage make call lacks NO_RUST=YesPlease: %q", m)
		}
	}
	// The tools, and Debian/Ubuntu's Rust packages (rust-all, rust-src, librust-*-dev, ...).
	if tool := regexp.MustCompile(`\b(cargo|rustc|rustup)\b|\b(lib)?rust-[a-z0-9]`).FindString(body); tool != "" {
		t.Errorf("git stage uses %s; build git with NO_RUST instead of a Rust toolchain", tool)
	}

	// The assert is the stage's last RUN and its last command, so nothing after it
	// (such as "|| true") can mask a failure.
	var lastRun string
	for _, l := range git {
		if strings.HasPrefix(l, "RUN ") {
			lastRun = l
		}
	}
	const assert = " && /out/usr/local/bin/git version --build-options | grep -qx 'rust: disabled'"
	if !strings.HasSuffix(strings.TrimRight(lastRun, " \t"), assert) {
		t.Errorf("git stage's last RUN does not end with %q: %q", assert, lastRun)
	}

	finalLines := dockerfileStage(t, "FROM ${UBUNTU_IMAGE}")
	copied := false
	for _, l := range finalLines {
		copied = copied || strings.TrimSpace(l) == "COPY --from=git /out/usr/local/ /usr/local/"
	}
	if !copied {
		t.Error("final stage lacks the line COPY --from=git /out/usr/local/ /usr/local/")
	}
	final := strings.Join(finalLines, "\n")
	for _, want := range []string{
		`printf '%s\n%s\n' 2.48 "$v" | sort -V -C`,
		"for b in git-remote-https git-http-push; do",
		`grep 'not found' <<<"$libs"`,
	} {
		if !strings.Contains(final, want) {
			t.Errorf("final stage lacks %q", want)
		}
	}
}
