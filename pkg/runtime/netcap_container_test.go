//go:build linux && netcapcontainer

/*
Copyright 2026 The Scion Authors.
*/

package runtime

// Container-level check that Docker/Podman agent containers launched with
// runtime-managed metadata interception (netns-setup sidecar) have no
// CAP_NET_ADMIN or CAP_NET_RAW in PID 1, in the harness, or in any
// `exec` session (--user root or --user scion), while the metadata
// REDIRECT rule installed by the sidecar remains intact and reachable and
// `sudo` still works for non-network commands.
//
// Both the agent container's capability flags and the netns-setup sidecar's
// argv are derived from Scion's own argv builders (buildCommonRunArgs and
// buildNetnsSetupArgs) so the container test stays coupled to production code.
//
// Needs a container daemon that can grant NET_ADMIN to the setup sidecar, and
// the sciontool that ships: build it through image-build/scion-base's
// builder stage (its settings, including CGO_ENABLED=0 and the guard that
// enforces it) and copy /usr/local/bin/sciontool out of that stage, then:
//
//	SCION_NETCAP_TEST_SCIONTOOL=<that binary> \
//	  go test -tags netcapcontainer -run TestContainer -v ./pkg/runtime/
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

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
)

const (
	testImage    = "scion-netcap-test:local"
	metadataPort = "18380"
	probeDir     = "/tmp/netcap-probe"

	// Kernel capability bit numbers (linux/capability.h).
	netcapBitNetAdmin = 12
	netcapBitNetRaw   = 13

	// interceptionRequired and interceptionSourceRuntime are the markers the
	// Docker/Podman runtime sets when runtime-managed interception is active.
	interceptionRequired      = MetadataInterceptionEnvVar + "=" + MetadataInterceptionRequired
	interceptionSourceRuntime = MetadataInterceptionSourceEnvVar + "=" + MetadataInterceptionSourceRuntime

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
	mustDocker(t, append(args, filepath.Join("testdata", "netcap_container"))...)
}

func capHasBit(t *testing.T, hex string, bit uint) bool {
	t.Helper()
	v, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
	if err != nil {
		t.Fatalf("parse capability mask %q: %v", hex, err)
	}
	return v&(1<<bit) != 0
}

func capHasNetAdmin(t *testing.T, hex string) bool {
	t.Helper()
	return capHasBit(t, hex, netcapBitNetAdmin)
}

func capHasNetRaw(t *testing.T, hex string) bool {
	t.Helper()
	return capHasBit(t, hex, netcapBitNetRaw)
}

// agentCapFlagsFromRuntime extracts the capability flags (--cap-drop, --cap-add,
// --privileged) emitted by buildCommonRunArgs for a Docker agent container in
// the given metadata mode.
func agentCapFlagsFromRuntime(t *testing.T, mode string) []string {
	t.Helper()
	cfg := RunConfig{
		RuntimeName:          "docker",
		Harness:              &harness.Generic{},
		Name:                 "netcap-agent",
		UnixUsername:         "scion",
		Image:                testImage,
		Task:                 "probe",
		Env:                  []string{"SCION_METADATA_MODE=" + mode, "SCION_METADATA_PORT=" + metadataPort},
		MetadataInterception: mode == "assign" || mode == "block",
	}
	commonArgs, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs(%q): %v", mode, err)
	}
	var flags []string
	for i := 0; i < len(commonArgs); i++ {
		switch commonArgs[i] {
		case "--cap-drop", "--cap-add":
			if i+1 < len(commonArgs) {
				flags = append(flags, commonArgs[i], commonArgs[i+1])
				i++
			}
		case "--privileged":
			flags = append(flags, commonArgs[i])
		default:
			if strings.HasPrefix(commonArgs[i], "--cap-drop=") ||
				strings.HasPrefix(commonArgs[i], "--cap-add=") ||
				strings.HasPrefix(commonArgs[i], "--privileged=") {
				flags = append(flags, commonArgs[i])
			}
		}
	}
	return flags
}

// createAgent creates and starts an agent container using the capability flags
// emitted by buildCommonRunArgs. When runSidecar is true (normal assign/block
// launch), it also runs the short-lived netns-setup container built from
// buildNetnsSetupArgs sharing the agent's network namespace.
func createAgent(t *testing.T, sciontool, mode string, runSidecar bool, extra ...string) string {
	t.Helper()
	name := fmt.Sprintf("scion-netcap-%s-%d", mode, time.Now().UnixNano())
	args := []string{"create", "--name", name}
	args = append(args, agentCapFlagsFromRuntime(t, mode)...)
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
	cid := mustDocker(t, args...)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := docker(t, "logs", cid)
			t.Logf("container logs:\n%s", logs)
		}
		_, _ = docker(t, "rm", "-f", cid)
	})
	mustDocker(t, "cp", sciontool, cid+":/usr/local/bin/sciontool")
	mustDocker(t, "start", cid)

	if runSidecar {
		runNetnsSetupSidecar(t, sciontool, cid, mode)
	}
	return cid
}

// runNetnsSetupSidecar runs the short-lived netns-setup container sharing
// agentContainerID's network namespace, deriving its full CLI argv from
// buildNetnsSetupArgs.
func runNetnsSetupSidecar(t *testing.T, sciontool, agentContainerID, mode string, extraEnv ...string) {
	t.Helper()
	env := []string{
		"SCION_METADATA_MODE=" + mode,
		"SCION_METADATA_PORT=" + metadataPort,
	}
	env = append(env, extraEnv...)
	cfg := RunConfig{
		Image: testImage,
		Env:   env,
	}
	runArgs := buildNetnsSetupArgs(agentContainerID, cfg)
	if len(runArgs) < 2 || runArgs[0] != "run" || runArgs[1] != "--rm" {
		t.Fatalf("buildNetnsSetupArgs() unexpected prefix: %v", runArgs)
	}
	// Replace "run --rm" with "create" so we can docker cp the test sciontool
	// binary before starting, while keeping every capability, security-opt,
	// name, network, user, entrypoint, env, and label flag verbatim.
	createArgs := append([]string{"create"}, runArgs[2:]...)
	sidecarName := netnsSetupContainerName(agentContainerID)
	_, _ = docker(t, "rm", "-f", sidecarName)
	mustDocker(t, createArgs...)
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

// redirectFilterAcceptPresent checks that the narrowly-scoped filter OUTPUT
// ACCEPT rule for the redirected loopback target is present.
func redirectFilterAcceptPresent(t *testing.T, name string) bool {
	t.Helper()
	_, err := docker(t, "run", "--rm",
		"--network", "container:"+name,
		"--cap-add", "NET_ADMIN",
		"--user", "0",
		testImage,
		"iptables", "-C", "OUTPUT",
		"-d", "127.0.0.1", "-p", "tcp", "--dport", metadataPort,
		"-j", "ACCEPT")
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

func anyHasNetRaw(t *testing.T, vals []string) bool {
	t.Helper()
	for _, v := range vals {
		if capHasNetRaw(t, v) {
			return true
		}
	}
	return false
}

// assertRawSocketsEPERM verifies that creating both AF_PACKET/SOCK_RAW and
// AF_INET/SOCK_RAW sockets fails with EPERM inside container name, even when
// running as root directly or via sudo from the scion user.
func assertRawSocketsEPERM(t *testing.T, name string) {
	t.Helper()
	pyCheck := `import errno, socket, sys
cases = [
    ("AF_PACKET/SOCK_RAW", (socket.AF_PACKET, socket.SOCK_RAW)),
    ("AF_INET/SOCK_RAW", (socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)),
]
for label, args in cases:
    try:
        s = socket.socket(*args)
        s.close()
        sys.exit(f"{label} succeeded unexpectedly")
    except PermissionError as e:
        if e.errno != errno.EPERM:
            sys.exit(f"{label} raised PermissionError with errno={e.errno}, want EPERM ({errno.EPERM})")
    except OSError as e:
        sys.exit(f"{label} raised OSError errno={e.errno} ({e}), want EPERM ({errno.EPERM})")
`
	if out, err := docker(t, "exec", "--user", "root", name, "python3", "-c", pyCheck); err != nil {
		t.Errorf("raw socket EPERM check as root failed: %v\n%s", err, out)
	}
	if out, err := docker(t, "exec", "--user", "scion", name, "sudo", "-n", "python3", "-c", pyCheck); err != nil {
		t.Errorf("raw socket EPERM check via sudo failed: %v\n%s", err, out)
	}
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

			// Agent container OCI spec has neither NET_ADMIN nor NET_RAW in CapAdd.
			// Effective capability absence (including CAP_NET_RAW) is verified via
			// the kernel capability sets and raw-socket EPERM checks below, since
			// engines whose default set already omits NET_RAW (e.g. Podman 5) may
			// record an empty HostConfig.CapDrop.
			capAdd := mustDocker(t, "inspect", "--format", "{{.HostConfig.CapAdd}}", name)
			if strings.Contains(capAdd, "NET_ADMIN") {
				t.Errorf("agent container HostConfig.CapAdd = %q, must not contain NET_ADMIN", capAdd)
			}
			if strings.Contains(capAdd, "NET_RAW") {
				t.Errorf("agent container HostConfig.CapAdd = %q, must not contain NET_RAW", capAdd)
			}

			// Harness, PID 1, and exec sessions (--user root and --user scion)
			// have neither CAP_NET_ADMIN nor CAP_NET_RAW.
			if uid := probe(t, name, "uid"); uid != "1000" {
				t.Errorf("harness uid = %s, want 1000 (the scion user)", uid)
			}
			for _, f := range []string{"capbnd", "capeff", "capprm"} {
				val := probe(t, name, f)
				if capHasNetAdmin(t, val) {
					t.Errorf("harness %s still has CAP_NET_ADMIN", f)
				}
				if capHasNetRaw(t, val) {
					t.Errorf("harness %s still has CAP_NET_RAW", f)
				}
			}
			pid1Caps := pid1ThreadCaps(t, name)
			if anyHasNetAdmin(t, pid1Caps) {
				t.Error("a thread of init (PID 1) still has CAP_NET_ADMIN in CapBnd, CapEff or CapPrm")
			}
			if anyHasNetRaw(t, pid1Caps) {
				t.Error("a thread of init (PID 1) still has CAP_NET_RAW in CapBnd, CapEff or CapPrm")
			}
			for _, user := range []string{"root", "scion"} {
				bnd := execCapBnd(t, name, user)
				if capHasNetAdmin(t, bnd) {
					t.Errorf("docker exec --user %s CapBnd = %s still has CAP_NET_ADMIN", user, bnd)
				}
				if capHasNetRaw(t, bnd) {
					t.Errorf("docker exec --user %s CapBnd = %s still has CAP_NET_RAW", user, bnd)
				}
			}

			// Raw-socket creation (AF_PACKET/SOCK_RAW and AF_INET/SOCK_RAW) fails
			// with EPERM even as root.
			assertRawSocketsEPERM(t, name)

			// sudo iptables flush fails from both harness and exec session, and redirect survives.
			if rc := probe(t, name, "sudo-flush-rc"); rc == "0" {
				t.Errorf("sudo iptables -t nat -F succeeded from the harness; log:\n%s", probe(t, name, "sudo-flush.log"))
			}
			if out, err := docker(t, "exec", "--user", "scion", name, "sudo", "-n", "iptables", "-t", "nat", "-F", "OUTPUT"); err == nil {
				t.Errorf("docker exec --user scion sudo iptables -t nat -F OUTPUT succeeded unexpectedly: %s", out)
			}
			if !redirectPresent(t, name) {
				t.Error("metadata REDIRECT rule is gone")
			}

			// curl 169.254.169.254 from an exec session reaches the emulator.
			httpCode := mustDocker(t, "exec", "--user", "scion", name,
				"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
				"-H", "Metadata-Flavor: Google", "http://169.254.169.254/")
			if httpCode != "200" {
				t.Errorf("curl http://169.254.169.254/ returned HTTP %s, want 200", httpCode)
			}

			// sudo still works for a non-network command in an exec session.
			sudoUID := mustDocker(t, "exec", "--user", "scion", name, "sudo", "-n", "id", "-u")
			if sudoUID != "0" {
				t.Errorf("docker exec --user scion sudo -n id -u = %q, want 0", sudoUID)
			}
		})
	}

	// Passthrough mode also drops CAP_NET_RAW and has no CAP_NET_ADMIN.
	t.Run("passthrough drops CAP_NET_RAW", func(t *testing.T) {
		name := createAgent(t, sciontool, "passthrough", false)
		waitProbe(t, name)

		pid1Caps := pid1ThreadCaps(t, name)
		if anyHasNetAdmin(t, pid1Caps) {
			t.Error("a thread of init (PID 1) still has CAP_NET_ADMIN in CapBnd, CapEff or CapPrm")
		}
		if anyHasNetRaw(t, pid1Caps) {
			t.Error("a thread of init (PID 1) still has CAP_NET_RAW in CapBnd, CapEff or CapPrm")
		}
		assertRawSocketsEPERM(t, name)
	})

	// With SCION_INIT_FIREWALL=true plus block/assign, the least-privilege
	// sidecar (--cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW
	// --security-opt no-new-privileges) runs init-firewall.sh before installing
	// the metadata redirect, so both the firewall chain and the metadata
	// redirect are present, the redirect probe passes despite the firewall's
	// catch-all OUTPUT REJECT, and non-allowlisted egress stays rejected.
	t.Run("firewall in sidecar preserves redirect", func(t *testing.T) {
		for _, mode := range []string{"block", "assign"} {
			t.Run(mode, func(t *testing.T) {
				name := createAgent(t, sciontool, mode, false, "-e", "SCION_INIT_FIREWALL=true")
				runNetnsSetupSidecar(t, sciontool, name, mode, "SCION_INIT_FIREWALL=true")
				waitProbe(t, name)

				if _, err := docker(t, "run", "--rm",
					"--network", "container:"+name,
					"--cap-add", "NET_ADMIN",
					"--user", "0",
					testImage,
					"iptables", "-L", "SCION_TEST_FW"); err != nil {
					t.Errorf("expected SCION_TEST_FW chain created by init-firewall.sh in sidecar: %v", err)
				}
				if !redirectPresent(t, name) {
					t.Error("metadata REDIRECT rule is missing after init-firewall.sh ran in sidecar")
				}
				if !redirectFilterAcceptPresent(t, name) {
					t.Error("filter OUTPUT ACCEPT rule for redirected metadata port is missing after init-firewall.sh ran in sidecar")
				}
				httpCode := mustDocker(t, "exec", "--user", "scion", name,
					"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
					"-H", "Metadata-Flavor: Google", "http://169.254.169.254/")
				if httpCode != "200" {
					t.Errorf("curl http://169.254.169.254/ returned HTTP %s, want 200", httpCode)
				}
				for _, target := range []string{"http://192.0.2.1/", "http://192.0.2.1:" + metadataPort + "/"} {
					if out, err := docker(t, "exec", "--user", "scion", name,
						"curl", "-sS", "--connect-timeout", "3", target); err == nil {
						t.Errorf("curl %s succeeded unexpectedly under firewall: %s", target, out)
					}
				}
			})
		}
	})

	// Run the real harnesses/claude/init-firewall.sh in the least-privilege
	// netns-setup sidecar (argv derived from buildNetnsSetupArgs) on an
	// IPv6-enabled network, asserting IPv6 DROP policies + REJECT, gateway /32
	// (no /24), no tcp/22, and metadata redirect preservation.
	t.Run("hardened init-firewall.sh in sidecar on IPv6 network", func(t *testing.T) {
		fwBaseImage := "scion-netcap-fw-base:local"
		fwImage := "scion-netcap-fw-test:local"
		fwArgs := []string{
			"build", "-q", "-t", fwBaseImage,
			"--build-arg", "PACKAGES=iptables ipset iproute2 jq aggregate sudo procps curl",
		}
		if base := os.Getenv("SCION_NETCAP_TEST_BASE"); base != "" {
			fwArgs = append(fwArgs, "--build-arg", "BASE="+base)
		}
		mustDocker(t, append(fwArgs, filepath.Join("testdata", "netcap_container"))...)

		scriptBytes, err := os.ReadFile(filepath.Join("..", "..", "harnesses", "claude", "init-firewall.sh"))
		if err != nil {
			t.Fatalf("read harnesses/claude/init-firewall.sh: %v", err)
		}
		fwCtx := t.TempDir()
		if err := os.WriteFile(filepath.Join(fwCtx, "init-firewall.sh"), scriptBytes, 0o755); err != nil {
			t.Fatalf("write temp init-firewall.sh: %v", err)
		}
		fwDockerfile := fmt.Sprintf("FROM %s\nCOPY init-firewall.sh /usr/local/sbin/init-firewall.sh\nRUN chmod 0755 /usr/local/sbin/init-firewall.sh\n", fwBaseImage)
		if err := os.WriteFile(filepath.Join(fwCtx, "Dockerfile"), []byte(fwDockerfile), 0o644); err != nil {
			t.Fatalf("write temp Dockerfile: %v", err)
		}
		mustDocker(t, "build", "-q", "-t", fwImage, fwCtx)

		netName := fmt.Sprintf("scion-netcap-v6-%d", time.Now().UnixNano())
		if out, err := docker(t, "network", "create", "--ipv6", "--subnet", "fd00:db8:8994::/64", netName); err != nil {
			t.Skipf("unable to create IPv6 docker network (skipping IPv6 live test): %v\n%s", err, out)
		}
		t.Cleanup(func() { _, _ = docker(t, "network", "rm", netName) })

		name := fmt.Sprintf("scion-netcap-fw-%d", time.Now().UnixNano())
		agentArgs := []string{"create", "--name", name, "--network", netName}
		agentArgs = append(agentArgs, agentCapFlagsFromRuntime(t, "block")...)
		agentArgs = append(agentArgs,
			"-e", interceptionRequired,
			"-e", interceptionSourceRuntime,
			"-e", "SCION_INIT_FIREWALL=true",
			"-e", "SCION_METADATA_MODE=block",
			"-e", "SCION_METADATA_PORT="+metadataPort,
			"-e", "SCION_HOST_UID=1000",
			"-e", "SCION_HOST_GID=1000",
			"-e", "SCION_TELEMETRY_ENABLED=false",
			"--entrypoint", "/usr/local/bin/sciontool",
			fwImage, "init", "--", "/usr/local/bin/netcap-harness.sh")
		cid := mustDocker(t, agentArgs...)
		t.Cleanup(func() {
			if t.Failed() {
				logs, _ := docker(t, "logs", cid)
				t.Logf("container logs:\n%s", logs)
			}
			_, _ = docker(t, "rm", "-f", cid)
		})
		mustDocker(t, "cp", sciontool, cid+":/usr/local/bin/sciontool")
		mustDocker(t, "start", cid)

		runArgs := buildNetnsSetupArgs(cid, RunConfig{
			Image: fwImage,
			Env: []string{
				"SCION_METADATA_MODE=block",
				"SCION_METADATA_PORT=" + metadataPort,
				"SCION_INIT_FIREWALL=true",
			},
		})
		if len(runArgs) < 2 || runArgs[0] != "run" || runArgs[1] != "--rm" {
			t.Fatalf("buildNetnsSetupArgs() unexpected prefix: %v", runArgs)
		}
		createArgs := append([]string{"create"}, runArgs[2:]...)
		sidecarName := netnsSetupContainerName(cid)
		_, _ = docker(t, "rm", "-f", sidecarName)
		mustDocker(t, createArgs...)
		defer func() { _, _ = docker(t, "rm", "-f", sidecarName) }()
		mustDocker(t, "cp", sciontool, sidecarName+":/usr/local/bin/sciontool")
		mustDocker(t, "start", "-a", sidecarName)

		waitProbe(t, cid)

		ip6Rules := mustDocker(t, "run", "--rm",
			"--network", "container:"+cid,
			"--cap-add", "NET_ADMIN",
			"--user", "0",
			fwImage, "ip6tables", "-S")
		for _, want := range []string{
			"-P INPUT DROP",
			"-P FORWARD DROP",
			"-P OUTPUT DROP",
			"-A OUTPUT -j REJECT --reject-with icmp6-adm-prohibited",
		} {
			if !strings.Contains(ip6Rules, want) {
				t.Errorf("ip6tables -S missing %q:\n%s", want, ip6Rules)
			}
		}

		ip4Rules := mustDocker(t, "run", "--rm",
			"--network", "container:"+cid,
			"--cap-add", "NET_ADMIN",
			"--user", "0",
			fwImage, "iptables", "-S")
		if strings.Contains(ip4Rules, "/24") {
			t.Errorf("iptables -S unexpectedly contains /24 rule:\n%s", ip4Rules)
		}
		if strings.Contains(ip4Rules, "--dport 22") {
			t.Errorf("iptables -S unexpectedly contains --dport 22 rule:\n%s", ip4Rules)
		}
		if !strings.Contains(ip4Rules, "/32 -j ACCEPT") {
			t.Errorf("iptables -S missing gateway /32 ACCEPT rule:\n%s", ip4Rules)
		}

		if !redirectPresent(t, cid) {
			t.Error("metadata REDIRECT rule is missing after hardened init-firewall.sh ran in sidecar")
		}
		httpCode := mustDocker(t, "exec", "--user", "scion", cid,
			"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"-H", "Metadata-Flavor: Google", "http://169.254.169.254/")
		if httpCode != "200" {
			t.Errorf("curl http://169.254.169.254/ returned HTTP %s, want 200", httpCode)
		}
		if out, err := docker(t, "exec", "--user", "scion", cid,
			"curl", "-6", "--connect-timeout", "5", "https://example.com"); err == nil {
			t.Errorf("expected curl -6 https://example.com to fail under IPv6 DROP, succeeded: %s", out)
		}
	})

	// Missing runtime source marker refuses before writing credentials.
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
		if !strings.Contains(string(info), metadata.InterceptionMarkerMissingRefusal) {
			t.Errorf("agent-info.json does not carry the expected refusal text (%q):\n%s", metadata.InterceptionMarkerMissingRefusal, info)
		}
	})

	// When the netns-setup sidecar is skipped, init waits for the redirect
	// probe, times out, cleans up the transport token, writes the fixed refusal
	// text, and exits non-zero without starting the harness.
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
		if !strings.Contains(string(info), metadata.ProtectionUnavailableRefusal) {
			t.Errorf("agent-info.json does not carry the expected refusal text (%q):\n%s", metadata.ProtectionUnavailableRefusal, info)
		}
	})
}
