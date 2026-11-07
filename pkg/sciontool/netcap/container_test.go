//go:build linux && netcapcontainer

/*
Copyright 2026 The Scion Authors.
*/

package netcap

// Container-level check that sciontool init, after setting up metadata
// interception, leaves no CAP_NET_ADMIN in the harness or in any of its
// own threads: the bounding, effective and permitted sets lack it,
// `sudo iptables -t nat -F` from the harness fails, and the interception
// REDIRECT is still in place. Passthrough is checked to be unchanged, and
// there the same sudo flush succeeds (a positive control for the probe).
// A container without CAP_SETPCAP, where the drop cannot succeed, must
// make init exit non-zero without starting the harness. A block-mode
// container whose image lacks iptables, with the interception marked
// required, must be refused before init writes any credential.
//
// Needs a docker daemon that can grant NET_ADMIN, and the sciontool that
// ships: build it through image-build/scion-base's builder stage (its
// settings, including CGO_ENABLED=0 and the guard that enforces it) and
// copy /usr/local/bin/sciontool out of that stage, then:
//
//	SCION_NETCAP_TEST_SCIONTOOL=<that binary> \
//	  go test -tags netcapcontainer -run TestContainer -v ./pkg/sciontool/netcap/
//
// Mutation check: build sciontool with DropNetAdmin returning nil before
// its first syscall and point SCION_NETCAP_TEST_SCIONTOOL at it; the assign,
// block and drop-failure subtests must fail.
//
// Optional: SCION_NETCAP_TEST_BASE overrides the test image's base image
// (the Dockerfile's default is pinned by digest).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testImage = "scion-netcap-test:local"
	// noToolsImage is testImage built without iptables.
	noToolsImage = "scion-netcap-test-notools:local"
	metadataPort = "18380"
	probeDir     = "/tmp/netcap-probe"

	// interceptionRequired is the marker the Docker runtime sets when it
	// grants NET_ADMIN for the metadata interception (metadata
	// InterceptionEnvVar and InterceptionRequired).
	interceptionRequired = "SCION_METADATA_INTERCEPTION=required"

	// refusalText is the fixed Hub-visible refusal, pinned verbatim by
	// cmd/sciontool/commands' TestMetadataProtectionRefusalText_Golden.
	refusalText = "agent start refused: the configured metadata protection could not be set up in this container. Provide the required host capabilities, or set the metadata mode to passthrough explicitly to opt out."

	// dummyTransportToken stands in for the bootstrap transport credential:
	// it is not a credential, only something init would stage to a file.
	dummyTransportToken = "netcap-test-not-a-credential"
	transportTokenPath  = "/home/scion/.scion/transport-token"
)

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return dockerWithin(t, 5*time.Minute, args...)
}

func dockerWithin(t *testing.T, limit time.Duration, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	// WaitDelay bounds Wait when the CLI has exited or been killed but
	// something it started still holds the output pipe. It cannot bound a
	// CLI the test is not allowed to kill; waitExit's short calls cover that.
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// waitExit waits up to limit for container name to exit and returns its
// exit code. It polls with short docker inspect calls instead of one
// long-lived docker wait: a docker wait the test cannot kill (for example
// when docker is a wrapper that runs the CLI through sudo, so the process
// the test started belongs to root) would otherwise outlive the limit and
// block the test, where it has to fail.
func waitExit(t *testing.T, name string, limit time.Duration) (string, error) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		out, err := dockerWithin(t, 15*time.Second, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name)
		if err == nil {
			if status, code, ok := strings.Cut(out, " "); ok && (status == "exited" || status == "dead") {
				return code, nil
			}
		}
		if time.Now().After(deadline) {
			// The last inspect result makes a container that vanished or
			// could not be inspected distinguishable from one still running.
			return "", fmt.Errorf("container did not exit before the %s deadline; last inspect: %q, error: %v", limit, out, err)
		}
		time.Sleep(time.Second)
	}
}

func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(t, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func buildTestImage(t *testing.T) {
	t.Helper()
	buildImage(t, testImage)
}

// buildImage builds the test Dockerfile as tag, with any extra build args.
func buildImage(t *testing.T, tag string, buildArgs ...string) {
	t.Helper()
	args := []string{"build", "-q", "-t", tag}
	if base := os.Getenv("SCION_NETCAP_TEST_BASE"); base != "" {
		args = append(args, "--build-arg", "BASE="+base)
	}
	for _, a := range buildArgs {
		args = append(args, "--build-arg", a)
	}
	mustDocker(t, append(args, filepath.Join("testdata", "container"))...)
}

func capHasNetAdmin(t *testing.T, hex string) bool {
	t.Helper()
	v, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
	if err != nil {
		t.Fatalf("parse capability mask %q: %v", hex, err)
	}
	return v&(1<<capNetAdmin) != 0
}

// createAgent creates and starts a container from testImage that runs
// sciontool init as PID 1 with NET_ADMIN granted, plus any extra docker
// create arguments. In assign and block mode it also sets the required
// marker, as the docker runtime does when it grants NET_ADMIN.
func createAgent(t *testing.T, sciontool, mode string, extra ...string) string {
	t.Helper()
	return createAgentFrom(t, testImage, sciontool, mode, extra...)
}

func createAgentFrom(t *testing.T, image, sciontool, mode string, extra ...string) string {
	t.Helper()
	name := fmt.Sprintf("scion-netcap-%s-%d", mode, time.Now().UnixNano())
	args := []string{"create", "--name", name, "--cap-add", "NET_ADMIN"}
	if mode == "assign" || mode == "block" {
		args = append(args, "-e", interceptionRequired)
	}
	args = append(args, extra...)
	args = append(args,
		"-e", "SCION_METADATA_MODE="+mode,
		"-e", "SCION_METADATA_PORT="+metadataPort,
		"-e", "SCION_HOST_UID=1000",
		"-e", "SCION_HOST_GID=1000",
		"-e", "SCION_TELEMETRY_ENABLED=false",
		"--entrypoint", "/usr/local/bin/sciontool",
		image, "init", "--", "/usr/local/bin/netcap-harness.sh")
	mustDocker(t, args...)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := docker(t, "logs", name)
			t.Logf("container logs:\n%s", logs)
		}
		_, _ = docker(t, "rm", "-f", name)
	})
	// docker cp writes the binary as root inside the container, which
	// rootexec-style ownership checks expect.
	mustDocker(t, "cp", sciontool, name+":/usr/local/bin/sciontool")
	mustDocker(t, "start", name)
	return name
}

// waitProbe waits for the harness probe to finish.
func waitProbe(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := docker(t, "exec", name, "test", "-f", probeDir+"/done"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("harness probe did not finish within 90s")
		}
		time.Sleep(time.Second)
	}
}

// startAgent creates an agent container and waits for its harness probe.
func startAgent(t *testing.T, sciontool, mode string) string {
	t.Helper()
	name := createAgent(t, sciontool, mode)
	waitProbe(t, name)
	return name
}

func probe(t *testing.T, name, file string) string {
	t.Helper()
	return mustDocker(t, "exec", name, "cat", probeDir+"/"+file)
}

// redirectPresent checks the nat rule from a `docker exec` process, which
// has the container's full capability set (not init's), so it can read the
// table regardless of the drop.
func redirectPresent(t *testing.T, name string) bool {
	t.Helper()
	_, err := docker(t, "exec", name, "iptables", "-t", "nat", "-C", "OUTPUT",
		"-d", "169.254.169.254", "-p", "tcp", "--dport", "80",
		"-j", "REDIRECT", "--to-port", metadataPort)
	return err == nil
}

// pid1ThreadCaps returns, for every thread of PID 1 (sciontool init), the
// hex values of its CapBnd, CapEff and CapPrm lines.
func pid1ThreadCaps(t *testing.T, name string) []string {
	t.Helper()
	out := mustDocker(t, "exec", name, "sh", "-c",
		"grep -h -E '^Cap(Bnd|Eff|Prm):' /proc/1/task/*/status")
	var vals []string
	for _, line := range strings.Split(out, "\n") {
		if _, v, ok := strings.Cut(line, ":"); ok {
			vals = append(vals, strings.TrimSpace(v))
		}
	}
	if len(vals) < 3 || len(vals)%3 != 0 {
		t.Fatalf("read %d capability values from PID 1's threads, want a non-zero multiple of 3", len(vals))
	}
	return vals
}

func anyHasNetAdmin(t *testing.T, vals []string) bool {
	t.Helper()
	for _, v := range vals {
		if capHasNetAdmin(t, v) {
			return true
		}
	}
	return false
}

func TestContainer(t *testing.T) {
	sciontool := os.Getenv("SCION_NETCAP_TEST_SCIONTOOL")
	if sciontool == "" {
		t.Skip("SCION_NETCAP_TEST_SCIONTOOL not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	buildTestImage(t)

	for _, mode := range []string{"assign", "block"} {
		t.Run(mode, func(t *testing.T) {
			name := startAgent(t, sciontool, mode)

			if uid := probe(t, name, "uid"); uid != "1000" {
				t.Errorf("harness uid = %s, want 1000 (the scion user)", uid)
			}
			for _, f := range []string{"capbnd", "capeff", "capprm"} {
				if capHasNetAdmin(t, probe(t, name, f)) {
					t.Errorf("harness %s still has CAP_NET_ADMIN", f)
				}
			}
			if anyHasNetAdmin(t, pid1ThreadCaps(t, name)) {
				t.Error("a thread of init (PID 1) still has CAP_NET_ADMIN in CapBnd, CapEff or CapPrm")
			}
			if rc := probe(t, name, "sudo-flush-rc"); rc == "0" {
				t.Errorf("sudo iptables -t nat -F succeeded from the harness; log:\n%s", probe(t, name, "sudo-flush.log"))
			}
			if !redirectPresent(t, name) {
				t.Error("metadata REDIRECT rule is gone")
			}
		})
	}

	t.Run("passthrough unchanged", func(t *testing.T) {
		// The dummy transport value also gives the refusal case below a
		// positive control: a start that is not refused stages it to
		// transportTokenPath, so that path is the right one to check.
		name := createAgent(t, sciontool, "passthrough", "-e", "SCION_TRANSPORT_TOKEN="+dummyTransportToken)
		waitProbe(t, name)
		if _, err := docker(t, "cp", name+":"+transportTokenPath, t.TempDir()); err != nil {
			t.Errorf("passthrough: no transport token file at %s; the refusal case's absence check would prove nothing: %v", transportTokenPath, err)
		}
		if !anyHasNetAdmin(t, pid1ThreadCaps(t, name)) {
			t.Error("passthrough: init dropped CAP_NET_ADMIN, want unchanged")
		}
		// Positive control for the probe: with the capability kept, the
		// same sudo flush that must fail in assign and block succeeds.
		if rc := probe(t, name, "sudo-flush-rc"); rc != "0" {
			t.Errorf("passthrough: sudo iptables -t nat -F failed (rc %s); the probe cannot tell a dropped capability from a broken probe; log:\n%s",
				rc, probe(t, name, "sudo-flush.log"))
		}
		if redirectPresent(t, name) {
			t.Error("passthrough: unexpected REDIRECT rule")
		}
	})

	// The bounding-set drop needs CAP_SETPCAP. Without it, init installs
	// the interception and then cannot drop CAP_NET_ADMIN, so it must exit
	// non-zero without ever starting the harness.
	t.Run("drop failure fails closed", func(t *testing.T) {
		name := createAgent(t, sciontool, "assign", "--cap-drop", "SETPCAP")
		code, err := waitExit(t, name, 90*time.Second)
		if err != nil {
			t.Fatalf("init did not exit before the 90s deadline although the capability drop could not succeed: %v", err)
		}
		if code == "0" {
			t.Fatalf("init exited 0 although the capability drop could not succeed")
		}
		if _, err := docker(t, "cp", name+":"+probeDir+"/done", t.TempDir()); err == nil {
			t.Error("the harness ran although the capability drop failed")
		}
	})

	// With the interception marked required and no iptables in the image,
	// init must refuse the start before it writes any credential: exit
	// non-zero, no harness, no transport token file, and the fixed refusal
	// text in agent-info.json (the broker relays it to the Hub).
	t.Run("missing tools refused before credentials", func(t *testing.T) {
		buildImage(t, noToolsImage, "PACKAGES=sudo procps")
		t.Cleanup(func() { _, _ = docker(t, "rmi", noToolsImage) })
		name := createAgentFrom(t, noToolsImage, sciontool, "block",
			"-e", "SCION_TRANSPORT_TOKEN="+dummyTransportToken)
		code, err := waitExit(t, name, 90*time.Second)
		if err != nil {
			t.Fatalf("init did not exit before the 90s deadline although the metadata protection cannot be set up: %v", err)
		}
		if code == "0" {
			t.Fatalf("init exited 0 although the metadata protection cannot be set up")
		}
		dir := t.TempDir()
		if _, err := docker(t, "cp", name+":"+probeDir+"/done", dir); err == nil {
			t.Error("the harness ran although the start was refused")
		}
		if _, err := docker(t, "cp", name+":"+transportTokenPath, dir); err == nil {
			t.Error("init wrote the transport token file before refusing the start")
		}
		mustDocker(t, "cp", name+":/home/scion/agent-info.json", dir)
		info, err := os.ReadFile(filepath.Join(dir, "agent-info.json"))
		if err != nil {
			t.Fatalf("read agent-info.json: %v", err)
		}
		if !strings.Contains(string(info), refusalText) {
			t.Errorf("agent-info.json does not carry the fixed refusal text:\n%s", info)
		}
	})
}
