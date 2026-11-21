//go:build linux && netcapcontainer

/*
Copyright 2026 The Scion Authors.
*/

package netcap

// Container-level check that Docker agent containers launched with
// runtime-managed metadata interception (netns-setup sidecar) have no
// CAP_NET_ADMIN in their OCI spec, in PID 1, in the harness, or in any
// `docker exec` session (--user root or --user scion), while the metadata
// REDIRECT rule installed by the sidecar remains intact and reachable and
// `sudo` still works for non-network commands.
//
// Needs a docker daemon that can grant NET_ADMIN to the setup sidecar, and
// the sciontool that ships: build it through image-build/scion-base's
// builder stage (its settings, including CGO_ENABLED=0 and the guard that
// enforces it) and copy /usr/local/bin/sciontool out of that stage, then:
//
//	SCION_NETCAP_TEST_SCIONTOOL=<that binary> \
//	  go test -tags netcapcontainer -run TestContainer -v ./pkg/sciontool/netcap/
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
	testImage    = "scion-netcap-test:local"
	metadataPort = "18380"
	probeDir     = "/tmp/netcap-probe"

	// interceptionRequired and interceptionSourceRuntime are the markers the
	// Docker/Podman runtime sets when runtime-managed interception is active.
	interceptionRequired      = "SCION_METADATA_INTERCEPTION=required"
	interceptionSourceRuntime = "SCION_METADATA_INTERCEPTION_SOURCE=runtime"

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
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

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
	args := []string{"build", "-q", "-t", testImage}
	if base := os.Getenv("SCION_NETCAP_TEST_BASE"); base != "" {
		args = append(args, "--build-arg", "BASE="+base)
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

// createAgent creates and starts an agent container WITHOUT --cap-add NET_ADMIN.
// When runSidecar is true (normal assign/block launch), it also runs the
// short-lived netns-setup container sharing the agent's network namespace.
func createAgent(t *testing.T, sciontool, mode string, runSidecar bool, extra ...string) string {
	t.Helper()
	name := fmt.Sprintf("scion-netcap-%s-%d", mode, time.Now().UnixNano())
	args := []string{"create", "--name", name}
	if mode == "assign" || mode == "block" {
		args = append(args, "-e", interceptionRequired, "-e", interceptionSourceRuntime)
	}
	args = append(args, extra...)
	args = append(args,
		"-e", "SCION_METADATA_MODE="+mode,
		"-e", "SCION_METADATA_PORT="+metadataPort,
		"-e", "SCION_HOST_UID=1000",
		"-e", "SCION_HOST_GID=1000",
		"-e", "SCION_TELEMETRY_ENABLED=false",
		"--entrypoint", "/usr/local/bin/sciontool",
		testImage, "init", "--", "/usr/local/bin/netcap-harness.sh")
	mustDocker(t, args...)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := docker(t, "logs", name)
			t.Logf("container logs:\n%s", logs)
		}
		_, _ = docker(t, "rm", "-f", name)
	})
	mustDocker(t, "cp", sciontool, name+":/usr/local/bin/sciontool")
	mustDocker(t, "start", name)

	if runSidecar {
		runNetnsSetupSidecar(t, sciontool, name, mode)
	}
	return name
}

// runNetnsSetupSidecar runs the short-lived netns-setup container sharing
// agentName's network namespace, mirroring pkg/runtime/common.go's
// runNetnsSetup.
func runNetnsSetupSidecar(t *testing.T, sciontool, agentName, mode string, extraEnv ...string) {
	t.Helper()
	sidecarName := agentName + "-netns-setup"
	args := []string{
		"create", "--name", sidecarName,
		"--network", "container:" + agentName,
		"--cap-add", "NET_ADMIN",
		"--user", "0",
		"--entrypoint", "/usr/local/bin/sciontool",
		"-e", "SCION_METADATA_MODE=" + mode,
		"-e", "SCION_METADATA_PORT=" + metadataPort,
		"--label", "scion.netns-setup=" + agentName,
	}
	for _, e := range extraEnv {
		args = append(args, "-e", e)
	}
	args = append(args, testImage, "netns-setup")
	mustDocker(t, args...)
	defer func() { _, _ = docker(t, "rm", "-f", sidecarName) }()
	mustDocker(t, "cp", sciontool, sidecarName+":/usr/local/bin/sciontool")
	mustDocker(t, "start", "-a", sidecarName)
}

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

func probe(t *testing.T, name, file string) string {
	t.Helper()
	return mustDocker(t, "exec", name, "cat", probeDir+"/"+file)
}

// redirectPresent checks the nat OUTPUT REDIRECT rule from a short-lived
// probe container sharing the agent's network namespace with NET_ADMIN,
// because the agent container itself has no CAP_NET_ADMIN even in docker exec.
func redirectPresent(t *testing.T, name string) bool {
	t.Helper()
	_, err := docker(t, "run", "--rm",
		"--network", "container:"+name,
		"--cap-add", "NET_ADMIN",
		"--user", "0",
		testImage,
		"iptables", "-t", "nat", "-C", "OUTPUT",
		"-d", "169.254.169.254", "-p", "tcp", "--dport", "80",
		"-j", "REDIRECT", "--to-port", metadataPort)
	return err == nil
}

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

func execCapBnd(t *testing.T, name, user string) string {
	t.Helper()
	return mustDocker(t, "exec", "--user", user, name, "sh", "-c",
		"grep '^CapBnd:' /proc/self/status | awk '{print $2}'")
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
			name := createAgent(t, sciontool, mode, true)
			waitProbe(t, name)

			// AC 1: agent container OCI spec has no NET_ADMIN.
			capAdd := mustDocker(t, "inspect", "--format", "{{.HostConfig.CapAdd}}", name)
			if strings.Contains(capAdd, "NET_ADMIN") {
				t.Errorf("agent container HostConfig.CapAdd = %q, must not contain NET_ADMIN", capAdd)
			}

			// AC 1: harness, PID 1, and exec sessions (--user root and --user scion) have no CAP_NET_ADMIN.
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
			for _, user := range []string{"root", "scion"} {
				if bnd := execCapBnd(t, name, user); capHasNetAdmin(t, bnd) {
					t.Errorf("docker exec --user %s CapBnd = %s still has CAP_NET_ADMIN", user, bnd)
				}
			}

			// AC 2: sudo iptables flush fails from both harness and exec session, and redirect survives.
			if rc := probe(t, name, "sudo-flush-rc"); rc == "0" {
				t.Errorf("sudo iptables -t nat -F succeeded from the harness; log:\n%s", probe(t, name, "sudo-flush.log"))
			}
			if out, err := docker(t, "exec", "--user", "scion", name, "sudo", "-n", "iptables", "-t", "nat", "-F", "OUTPUT"); err == nil {
				t.Errorf("docker exec --user scion sudo iptables -t nat -F OUTPUT succeeded unexpectedly: %s", out)
			}
			if !redirectPresent(t, name) {
				t.Error("metadata REDIRECT rule is gone")
			}

			// AC 2: curl 169.254.169.254 from an exec session reaches the emulator.
			httpCode := mustDocker(t, "exec", "--user", "scion", name,
				"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
				"-H", "Metadata-Flavor: Google", "http://169.254.169.254/")
			if httpCode != "200" {
				t.Errorf("curl http://169.254.169.254/ returned HTTP %s, want 200", httpCode)
			}

			// AC 4: sudo still works for a non-network command in an exec session.
			sudoUID := mustDocker(t, "exec", "--user", "scion", name, "sudo", "-n", "id", "-u")
			if sudoUID != "0" {
				t.Errorf("docker exec --user scion sudo -n id -u = %q, want 0", sudoUID)
			}
		})
	}

	// AC 3: missing runtime source marker refuses before writing credentials.
	t.Run("missing source marker refused before credentials", func(t *testing.T) {
		name := fmt.Sprintf("scion-netcap-nosrc-%d", time.Now().UnixNano())
		mustDocker(t, "create", "--name", name,
			"-e", interceptionRequired,
			"-e", "SCION_METADATA_MODE=block",
			"-e", "SCION_METADATA_PORT="+metadataPort,
			"-e", "SCION_HOST_UID=1000",
			"-e", "SCION_HOST_GID=1000",
			"-e", "SCION_TELEMETRY_ENABLED=false",
			"-e", "SCION_TRANSPORT_TOKEN="+dummyTransportToken,
			"--entrypoint", "/usr/local/bin/sciontool",
			testImage, "init", "--", "/usr/local/bin/netcap-harness.sh")
		t.Cleanup(func() { _, _ = docker(t, "rm", "-f", name) })
		mustDocker(t, "cp", sciontool, name+":/usr/local/bin/sciontool")
		mustDocker(t, "start", name)

		code, err := waitExit(t, name, 60*time.Second)
		if err != nil {
			t.Fatalf("init did not exit before deadline: %v", err)
		}
		if code == "0" {
			t.Fatal("init exited 0 despite missing runtime source marker")
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

	// AC 3: when the netns-setup sidecar is skipped, init waits for the
	// redirect probe, times out, cleans up the transport token, writes the
	// fixed refusal text, and exits non-zero without starting the harness.
	t.Run("skipped sidecar refuses after probe timeout", func(t *testing.T) {
		name := createAgent(t, sciontool, "block", false,
			"-e", "SCION_TRANSPORT_TOKEN="+dummyTransportToken)
		code, err := waitExit(t, name, 210*time.Second)
		if err != nil {
			t.Fatalf("init did not exit before deadline when sidecar was skipped: %v", err)
		}
		if code == "0" {
			t.Fatal("init exited 0 although the redirect sidecar was skipped")
		}
		dir := t.TempDir()
		if _, err := docker(t, "cp", name+":"+probeDir+"/done", dir); err == nil {
			t.Error("the harness ran although the redirect probe never succeeded")
		}
		if _, err := docker(t, "cp", name+":"+transportTokenPath, dir); err == nil {
			t.Error("transport token file survived the refusal")
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
