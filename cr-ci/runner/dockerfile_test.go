package runner

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// dockerfileStage returns the Dockerfile's stage that starts with the FROM line
// containing from, up to the next FROM, with backslash continuations joined so
// that each RUN step is one line.
func dockerfileStage(t *testing.T, from string) string {
	t.Helper()
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	df := strings.ReplaceAll(string(b), "\\\n", " ")
	start := strings.Index(df, from)
	if start < 0 {
		t.Fatalf("Dockerfile has no %q", from)
	}
	stage := df[start+len(from):]
	if end := strings.Index(stage, "\nFROM "); end >= 0 {
		stage = stage[:end]
	}
	return stage
}

// TestDockerfileGitStage: git is built from source without Rust. git 2.55+ builds
// its Rust parts (libgitcore.a, through cargo) unless NO_RUST is set, and the
// stage has no Rust toolchain, so a make call without NO_RUST fails the image
// build ("cargo: not found", Makefile target target/release/libgitcore.a). The
// stage must also confirm the result ("rust: disabled"), and the final stage must
// keep its asserts: git >= 2.48 and the HTTPS transport's runtime libraries.
func TestDockerfileGitStage(t *testing.T) {
	git := dockerfileStage(t, "FROM ${UBUNTU_IMAGE} AS git\n")
	makes := regexp.MustCompile(`make -C /tmp/git-src [^&]*`).FindAllString(git, -1)
	if len(makes) != 2 {
		t.Fatalf("git stage: %d make calls, want 2 (all, install): %q", len(makes), makes)
	}
	for _, m := range makes {
		if !strings.Contains(m, " NO_RUST=YesPlease ") {
			t.Errorf("git stage make call lacks NO_RUST=YesPlease: %q", m)
		}
	}
	for _, tool := range []string{"cargo", "rustc", "rustup"} {
		if regexp.MustCompile(`apt-get install[^&]*\b` + tool + `\b`).MatchString(git) {
			t.Errorf("git stage installs %s; build git with NO_RUST instead", tool)
		}
	}
	if !strings.Contains(git, `/out/usr/local/bin/git version --build-options | grep -qx 'rust: disabled'`) {
		t.Error("git stage does not assert that the build reports rust: disabled")
	}

	final := dockerfileStage(t, "FROM ${UBUNTU_IMAGE}\n")
	for _, want := range []string{
		"COPY --from=git /out/usr/local/ /usr/local/\n",
		`printf '%s\n%s\n' 2.48 "$v" | sort -V -C`,
		"for b in git-remote-https git-http-push; do",
		`grep 'not found' <<<"$libs"`,
	} {
		if !strings.Contains(final, want) {
			t.Errorf("final stage lacks %q", want)
		}
	}
}
