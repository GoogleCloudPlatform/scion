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
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var doctorVerbose bool

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system prerequisites, runtime configuration and platform health",
	Long: `Run diagnostic checks on this machine and, when a Hub is configured, on the
Hub and the platform it manages.

Who it is for: people checking that Scion is set up and healthy, or
troubleshooting agents, from their own machine. A highly authorized
assistant agent can run it from an environment a person owns (not from
inside an agent container) with a hub-boundary access token; see
"Platform Health" below. It is not a self-check for ordinary agents:
inside an agent container, use "sciontool doctor".

Sections:
  General          git and tmux on this machine.
  Hub Health       Hub reachability (/healthz), authentication, linked
                   project service accounts, and local settings checks
                   (NFS mounts, Kubernetes profiles, the server hub
                   endpoint on server hosts, telemetry, images, broker
                   credentials).
  Platform Health  The Hub's health summary, the same data as the web
                   health dashboard: the overall status, every item that
                   needs attention (problems first), and a line each for
                   the Hub fleet, runtime brokers, agents, dispatch and
                   integrations. --verbose adds per-broker and per-Hub-
                   instance tables. Reading it needs the hub.health.read
                   permission (hub admins). Without it the section shows
                   "requires hub.health.read" and the Hub Health section
                   checks brokers and agents from the lists you can see.
  Runtime          The active container runtime. For Kubernetes runtimes
                   this includes cluster connectivity, namespace access,
                   RBAC permissions and CSI driver availability.

To run Platform Health without an interactive login, a hub admin can mint
a hub-boundary token and use it with no stored login (see "scion hub token
--help"):

  scion hub token create --boundary hub --name health-check \
    --scopes hub_health:read,broker:read,agent:list --expires 30d
  SCION_HUB_TOKEN=scion_pat_... scion doctor --format json

Exit status: doctor reports check results and does not turn them into an
exit status. It exits zero whenever it ran, even when the Hub is
unreachable, the platform health summary is unhealthy or a runtime
diagnostic check fails; read the results from the output. It exits
non-zero only when the command itself fails, for example on an invalid
flag or when it cannot write its output.

--format json prints one JSON object and nothing else on stdout. Its
"status" is fail when any check failed, warn when any check warned, and
pass otherwise. The raw health summary is under "healthSummary" when it
was readable.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor()
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().BoolVar(&doctorVerbose, "verbose", false, "Show the full runtime broker and Hub instance tables in Platform Health")
}

// doctorReport collects every check doctor runs, by section, and prints
// each one as it is added unless the output is JSON. Under --format json
// nothing is written to stdout until finish writes the single JSON object.
type doctorReport struct {
	// stdout receives the text lines, or the JSON object.
	stdout io.Writer
	// color keeps ANSI colour codes in text output.
	color bool
	// json selects JSON output.
	json    bool
	verbose bool

	General        []scionruntime.CheckResult
	HubChecks      []scionruntime.CheckResult
	PlatformHealth []scionruntime.CheckResult
	// HealthSummary is the raw health summary, when it was readable.
	HealthSummary json.RawMessage
	// Runtime is the active runtime name; empty when no project resolved.
	Runtime string
	// Checks are the runtime checks.
	Checks []scionruntime.CheckResult
}

// doctorJSON is the --format json document.
type doctorJSON struct {
	// Status is fail when any check failed, else warn when any check
	// warned, else pass. It does not affect the exit status.
	Status         string                     `json:"status"`
	General        []scionruntime.CheckResult `json:"general"`
	HubChecks      []scionruntime.CheckResult `json:"hubChecks"`
	PlatformHealth []scionruntime.CheckResult `json:"platformHealth"`
	HealthSummary  json.RawMessage            `json:"healthSummary,omitempty"`
	Runtime        string                     `json:"runtime,omitempty"`
	Checks         []scionruntime.CheckResult `json:"checks"`
}

func newDoctorReport() *doctorReport {
	return &doctorReport{
		stdout:  os.Stdout,
		color:   util.ColorEnabled(os.Stdout),
		json:    outputFormat == "json",
		verbose: doctorVerbose,
	}
}

// text writes s (which may carry colour codes) in text mode only.
func (r *doctorReport) text(s string) {
	if r.json {
		return
	}
	if !r.color {
		s = util.StripANSI(s)
	}
	_, _ = io.WriteString(r.stdout, s)
}

// heading prints a section heading in text mode.
func (r *doctorReport) heading(title string) {
	r.text(fmt.Sprintf("\n%s%s%s\n", util.Bold, title, util.Reset))
}

// add records c in *section and prints it in text mode.
func (r *doctorReport) add(section *[]scionruntime.CheckResult, c scionruntime.CheckResult) {
	// Messages can carry text from the Hub, brokers and integrations
	// (names, reasons); never let it drive the terminal.
	c.Message = stripControlChars(c.Message)
	c.Remediation = stripControlChars(c.Remediation)
	*section = append(*section, c)
	var icon string
	switch c.Status {
	case "pass":
		icon = fmt.Sprintf("%s✓%s", util.Green, util.Reset)
	case "warn":
		icon = fmt.Sprintf("%s!%s", util.Yellow, util.Reset)
	case "fail":
		icon = fmt.Sprintf("%s✗%s", util.Red, util.Reset)
	case "skip":
		icon = fmt.Sprintf("%s-%s", util.Gray, util.Reset)
	case "info":
		icon = fmt.Sprintf("%si%s", util.Gray, util.Reset)
	}
	r.text(fmt.Sprintf("  %s %s: %s\n", icon, c.Name, c.Message))
	if c.Remediation != "" && c.Status != "pass" {
		r.text(fmt.Sprintf("    → %s\n", c.Remediation))
	}
}

// finish writes the JSON document in JSON mode. Check results never make
// it return an error: the only errors are failures to produce the output
// itself.
func (r *doctorReport) finish() error {
	if !r.json {
		return nil
	}
	doc := doctorJSON{
		Status:         r.overallStatus(),
		General:        nonNilChecks(r.General),
		HubChecks:      nonNilChecks(r.HubChecks),
		PlatformHealth: nonNilChecks(r.PlatformHealth),
		HealthSummary:  r.HealthSummary,
		Runtime:        r.Runtime,
		Checks:         nonNilChecks(r.Checks),
	}
	data, mErr := json.MarshalIndent(doc, "", "  ")
	if mErr != nil {
		return mErr
	}
	if _, wErr := fmt.Fprintln(r.stdout, string(data)); wErr != nil {
		return wErr
	}
	return nil
}

// overallStatus is fail when any check failed, warn when any check warned,
// and pass otherwise.
func (r *doctorReport) overallStatus() string {
	status := "pass"
	for _, section := range [][]scionruntime.CheckResult{r.General, r.HubChecks, r.PlatformHealth, r.Checks} {
		for _, c := range section {
			switch c.Status {
			case "fail":
				return "fail"
			case "warn":
				status = "warn"
			}
		}
	}
	return status
}

func nonNilChecks(c []scionruntime.CheckResult) []scionruntime.CheckResult {
	if c == nil {
		return []scionruntime.CheckResult{}
	}
	return c
}

func runDoctor() error {
	r := newDoctorReport()
	r.text(fmt.Sprintf("%sScion Doctor%s\n\n", util.Bold, util.Reset))

	// General checks
	r.text(fmt.Sprintf("%sGeneral%s\n", util.Bold, util.Reset))
	r.add(&r.General, checkGit())
	r.add(&r.General, checkTmux())

	// Resolve settings and hub endpoint
	gp := projectPath
	if gp == "" && globalMode {
		gp = "global"
	}
	resolvedSettingsPath, _, _ := config.ResolveProjectPath(gp)
	settings, err := config.LoadSettings(resolvedSettingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load settings: %v\n", err)
	}
	if settings == nil {
		settings = &config.Settings{}
	}

	hubEP := GetHubEndpoint(settings)

	// Create the authenticated client for the Hub checks if a Hub endpoint
	// is configured.
	var hubClient hubclient.Client
	if hubEP != "" {
		if c, err := getHubClient(settings); err == nil {
			hubClient = c
		}
	}
	var linkedProjectID string
	if settings.Hub != nil {
		linkedProjectID = settings.Hub.ProjectID
	}

	r.heading("Hub Health")
	ph := r.hubChecks(hubEP, hubClient, linkedProjectID)
	// Checks of this machine's settings, reported with the Hub checks.
	r.localSettingsChecks()

	r.heading("Platform Health")
	r.platformHealthSection(ph)

	// Resolve the active runtime
	r.heading("Runtime")
	resolved, err := resolveActiveProjectPath()
	if err != nil {
		return r.noProject()
	}

	rt := scionruntime.GetRuntime(resolved, profile)
	var diag scionruntime.Diagnosable
	var opts scionruntime.DiagnosticOpts
	if d, ok := rt.(scionruntime.Diagnosable); ok {
		diag = d
		projectDir, _ := config.GetResolvedProjectDir(resolved)
		if vs, _, err := config.LoadEffectiveSettings(projectDir); err == nil && vs != nil {
			if rtConfig, _, rtErr := vs.ResolveRuntime(profile); rtErr == nil {
				opts.Namespace = rtConfig.Namespace
				opts.GKEMode = rtConfig.GKE
			}
		}
	}
	r.runtimeSection(rt.Name(), diag, opts)
	return r.finish()
}

// hubChecks runs the Hub checks (D1-D4, D9) and reads the health summary
// for the Platform Health section. When the summary is readable it
// replaces the list-based broker (D3) and agent (D4) checks; otherwise
// those run as a fallback.
func (r *doctorReport) hubChecks(hubEP string, client hubclient.Client, linkedProjectID string) doctorPlatformHealth {
	// D1: Hub Connectivity
	d1, hubAnswered := probeDoctorHubConnectivity(hubEP, client)
	r.add(&r.HubChecks, d1)
	hubConnected := d1.Status == "pass" || d1.Status == "warn"

	// D2: Hub Authentication
	r.add(&r.HubChecks, checkDoctorHubAuth(hubEP, hubConnected, client))

	// The summary is read whenever the Hub answered, even if its /healthz
	// reports unhealthy: the summary says why, and a Hub that cannot read
	// its database answers 503, which has its own message.
	ph := fetchDoctorPlatformHealth(hubEP, hubConnected || hubAnswered, client)
	if ph.summary != nil {
		r.add(&r.HubChecks, scionruntime.CheckResult{Name: "broker-connectivity", Status: "skip", Message: "Reported under Platform Health"})
		r.add(&r.HubChecks, scionruntime.CheckResult{Name: "agent-health", Status: "skip", Message: "Reported under Platform Health"})
	} else {
		// D3: Broker Connectivity (fallback)
		r.add(&r.HubChecks, checkDoctorBrokerConnectivity(hubEP, hubConnected, client))
		// D4: Agent Health (fallback)
		r.add(&r.HubChecks, checkDoctorAgentHealth(hubEP, hubConnected, client))
	}

	// D9: GCP service account mappings (warning only): registered GSAs of
	// the linked project that no Kubernetes broker profile maps.
	r.add(&r.HubChecks, checkDoctorSAMappings(hubEP, hubConnected, client, linkedProjectID))
	return ph
}

// platformHealthSection reports the Platform Health lines and keeps the
// raw summary for JSON output.
func (r *doctorReport) platformHealthSection(ph doctorPlatformHealth) {
	checks := ph.checks
	if ph.summary != nil {
		// The verbose tables are for people; JSON carries the raw summary.
		checks = renderDoctorHealthSummary(ph.summary, r.verbose && !r.json)
	}
	for _, c := range checks {
		r.add(&r.PlatformHealth, c)
	}
	if ph.summary != nil {
		r.HealthSummary = ph.summary.Raw
	}
}

// localSettingsChecks runs the checks that read only this machine: D5
// NFS mounts, D10 Kubernetes block ServiceAccount, D11 server hub endpoint
// deprecation (server hosts only), D6 telemetry, D7 container images and
// D8 broker credentials. They are reported with the
// Hub checks, as before.
func (r *doctorReport) localSettingsChecks() {
	// D5: NFS Mount Status (local check against this host's global
	// settings and mount table; read-only)
	nfsSettings, _, nfsErr := config.LoadGlobalSettings()
	d5 := scionruntime.CheckResult{
		Name:    "nfs-mounts",
		Status:  "warn",
		Message: fmt.Sprintf("Could not load settings to check NFS: %v", nfsErr),
	}
	if nfsErr == nil {
		d5 = checkDoctorNFSMounts(nfsSettings, defaultNFSDoctorProbe())
	}
	r.add(&r.HubChecks, d5)

	// D10: Kubernetes block ServiceAccount (information only): Kubernetes
	// profiles in this host's global settings with no
	// kubernetes_block_service_account, whose GCP identity "block" pods run
	// as the namespace's default ServiceAccount.
	d10 := scionruntime.CheckResult{
		Name:    "k8s-block-service-account",
		Status:  "skip",
		Message: fmt.Sprintf("Could not load settings to check Kubernetes profiles: %v", nfsErr),
	}
	if nfsErr == nil {
		d10 = checkDoctorKubernetesBlockServiceAccount(nfsSettings)
	}
	r.add(&r.HubChecks, d10)

	// D11: Server hub endpoint deprecation (local check). Reported only on a
	// host configured as a server; plain CLI hosts get no line.
	if d11, ok := checkDoctorServerHubEndpointHere(); ok {
		r.add(&r.HubChecks, d11)
	}

	// D6: Telemetry Pipeline (local check)
	r.add(&r.HubChecks, checkDoctorTelemetry())
	// D7: Container Images (local check)
	r.add(&r.HubChecks, checkDoctorContainerImages())
	// D8: Credential Validity (local check)
	r.add(&r.HubChecks, checkDoctorCredentialValidity())
}

// runtimeSection reports the active runtime and its checks: the runtime's
// own diagnostics when it implements Diagnosable (diag non-nil), otherwise
// basic checks for docker, podman and the Apple container CLI.
func (r *doctorReport) runtimeSection(rtName string, diag scionruntime.Diagnosable, opts scionruntime.DiagnosticOpts) {
	r.Runtime = rtName
	r.add(&r.Checks, scionruntime.CheckResult{Name: "runtime", Status: "pass", Message: fmt.Sprintf("Active runtime: %s", rtName)})

	if diag == nil {
		var checks []scionruntime.CheckResult
		switch rtName {
		case "docker", "podman":
			checks = checkDockerOrPodman(rtName)
		case "container":
			checks = []scionruntime.CheckResult{checkContainerCLI()}
		}
		for _, c := range checks {
			r.add(&r.Checks, c)
		}
		return
	}

	r.heading(fmt.Sprintf("Runtime Diagnostics (%s)", rtName))
	report := diag.RunDiagnostics(opts)
	passes, warns, fails := 0, 0, 0
	for _, c := range report.Checks {
		r.add(&r.Checks, c)
		switch c.Status {
		case "pass":
			passes++
		case "warn":
			warns++
		case "fail":
			fails++
		}
	}

	r.text("\n")
	switch {
	case fails > 0:
		r.text(fmt.Sprintf("%s%d checks passed, %d warnings, %d failures%s\n", util.Red, passes, warns, fails, util.Reset))
	case warns > 0:
		r.text(fmt.Sprintf("%s%d checks passed, %d warnings%s\n", util.Yellow, passes, warns, util.Reset))
	default:
		r.text(fmt.Sprintf("%s%d checks passed%s\n", util.Green, passes, util.Reset))
	}
}

// noProject reports that no project was found, so the runtime checks are
// skipped, and finishes the report.
func (r *doctorReport) noProject() error {
	r.add(&r.Checks, scionruntime.CheckResult{
		Name: "project", Status: "warn",
		Message:     "No project found — skipping runtime checks",
		Remediation: "Run 'scion init' to create a project.",
	})
	return r.finish()
}

// stripControlChars removes C0 and C1 control characters and DEL from s,
// keeping newlines, so text from the Hub cannot carry terminal escape
// sequences.
func stripControlChars(s string) string {
	clean := func(r rune) bool {
		return r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f))
	}
	if strings.IndexFunc(s, clean) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if clean(r) {
			return -1
		}
		return r
	}, s)
}

func resolveActiveProjectPath() (string, error) {
	if projectPath != "" {
		return projectPath, nil
	}
	resolved, _, err := config.RequireProjectPath("")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func checkGit() scionruntime.CheckResult {
	path, err := exec.LookPath("git")
	if err != nil {
		return scionruntime.CheckResult{Name: "git", Status: "fail", Message: "git not found in PATH", Remediation: "Install git: https://git-scm.com/downloads"}
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return scionruntime.CheckResult{Name: "git", Status: "warn", Message: fmt.Sprintf("git found at %s but version check failed", path)}
	}
	return scionruntime.CheckResult{Name: "git", Status: "pass", Message: trimNewline(string(out))}
}

func checkTmux() scionruntime.CheckResult {
	_, err := exec.LookPath("tmux")
	if err != nil {
		if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
			return scionruntime.CheckResult{Name: "tmux", Status: "warn", Message: "tmux not found locally (required inside agent containers)"}
		}
		return scionruntime.CheckResult{Name: "tmux", Status: "skip", Message: "tmux check skipped on this platform"}
	}
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		return scionruntime.CheckResult{Name: "tmux", Status: "pass", Message: "tmux found"}
	}
	return scionruntime.CheckResult{Name: "tmux", Status: "pass", Message: trimNewline(string(out))}
}

func checkDockerOrPodman(name string) []scionruntime.CheckResult {
	_, err := exec.LookPath(name)
	if err != nil {
		return []scionruntime.CheckResult{{Name: name, Status: "fail", Message: fmt.Sprintf("%s not found in PATH", name), Remediation: fmt.Sprintf("Install %s", name)}}
	}
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		return []scionruntime.CheckResult{{Name: name, Status: "warn", Message: fmt.Sprintf("%s found but version check failed", name)}}
	}
	checks := []scionruntime.CheckResult{{Name: name, Status: "pass", Message: trimNewline(string(out))}}

	// Check daemon connectivity
	if _, err = exec.Command(name, "info").Output(); err != nil {
		return append(checks, scionruntime.CheckResult{
			Name:        name + "-daemon",
			Status:      "fail",
			Message:     fmt.Sprintf("%s daemon is not running or not accessible", name),
			Remediation: fmt.Sprintf("Start the %s daemon and try again.", name),
		})
	}
	return append(checks, scionruntime.CheckResult{Name: name + "-daemon", Status: "pass", Message: fmt.Sprintf("%s daemon is running", name)})
}

func checkContainerCLI() scionruntime.CheckResult {
	if goruntime.GOOS != "darwin" {
		return scionruntime.CheckResult{Name: "container", Status: "skip", Message: "Apple container CLI is only available on macOS"}
	}
	if _, err := exec.LookPath("container"); err != nil {
		return scionruntime.CheckResult{Name: "container", Status: "fail", Message: "container CLI not found in PATH", Remediation: "Install the container CLI for macOS"}
	}
	return scionruntime.CheckResult{Name: "container", Status: "pass", Message: "container CLI found"}
}

// checkDoctorHubConnectivity performs D1: Hub connectivity check via /healthz.
// Doctor itself calls probeDoctorHubConnectivity; this wrapper is kept as
// the test seam for the D1 result alone.
func checkDoctorHubConnectivity(hubEP string, client hubclient.Client) scionruntime.CheckResult {
	res, _ := probeDoctorHubConnectivity(hubEP, client)
	return res
}

// probeDoctorHubConnectivity is checkDoctorHubConnectivity that also
// reports whether the Hub answered at all. A Hub can answer and still fail
// D1 (its /healthz reports unhealthy, or returns a non-200 status); it is
// then reachable for other requests.
func probeDoctorHubConnectivity(hubEP string, client hubclient.Client) (res scionruntime.CheckResult, answered bool) {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Prefer authenticated Hub client health check (handles bearer tokens and proxy fallbacks)
	if client != nil {
		healthResp, err := client.Health(ctx)
		if err != nil {
			return scionruntime.CheckResult{
				Name:        "hub-connectivity",
				Status:      "fail",
				Message:     fmt.Sprintf("Cannot reach Hub at %s: %v", hubEP, hubclient.HintProxyError(err)),
				Remediation: "Verify the Hub is running and the endpoint is correct",
			}, false
		}
		probe := healthProbeResponse{Status: healthResp.Status, Checks: healthResp.Checks}
		if len(healthResp.Hub) > 0 {
			var hub healthProbeComponent
			if json.Unmarshal(healthResp.Hub, &hub) == nil {
				probe.Hub = &hub
			}
		}
		if len(healthResp.Broker) > 0 {
			var broker healthProbeComponent
			if json.Unmarshal(healthResp.Broker, &broker) == nil {
				probe.Broker = &broker
			}
		}
		return doctorHubHealthResult(hubEP, probe), true
	}

	cleanEP := strings.TrimRight(hubEP, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cleanEP+"/healthz", nil)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Invalid Hub endpoint: %v", err),
			Remediation: "Check the Hub endpoint URL",
		}, false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Cannot reach Hub at %s: %v", hubEP, err),
			Remediation: "Verify the Hub is running and the endpoint is correct",
		}, false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Hub returned HTTP %d", resp.StatusCode),
			Remediation: "Check Hub server logs",
		}, true
	}

	var healthResp healthProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		return scionruntime.CheckResult{
			Name:    "hub-connectivity",
			Status:  "warn",
			Message: "Hub reachable but health response could not be parsed",
		}, true
	}

	return doctorHubHealthResult(hubEP, healthResp), true
}

// doctorHubHealthResult maps a parsed /healthz response to the D1 check
// result, using the same severity semantics as the hub (ptone/scion#1094):
// healthy passes; degraded (up, a non-critical check failing) warns; unhealthy
// (a critical check such as the database failing) fails; any other status
// warns with the status verbatim. Non-healthy checks are named in the message.
func doctorHubHealthResult(hubEP string, health healthProbeResponse) scionruntime.CheckResult {
	res := scionruntime.CheckResult{Name: "hub-connectivity"}
	checks := ""
	if names := nonHealthyChecks(health); len(names) > 0 {
		checks = " (" + strings.Join(names, "; ") + ")"
	}
	switch health.Status {
	case probeStatusHealthy:
		res.Status = "pass"
		res.Message = fmt.Sprintf("Hub at %s is healthy", hubEP)
	case probeStatusDegraded:
		res.Status = "warn"
		res.Message = fmt.Sprintf("Hub at %s is degraded%s", hubEP, checks)
		res.Remediation = "Check Hub server logs (non-critical check failing)"
	case probeStatusUnhealthy:
		res.Status = "fail"
		res.Message = fmt.Sprintf("Hub at %s is unhealthy%s", hubEP, checks)
		res.Remediation = "Check Hub server logs (critical check failing)"
	default:
		res.Status = "warn"
		res.Message = fmt.Sprintf("Hub at %s reported status %q%s", hubEP, health.Status, checks)
	}
	return res
}

// checkDoctorHubAuth performs D2: Hub authentication check.
func checkDoctorHubAuth(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "hub-auth",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "hub-auth",
			Status:  "skip",
			Message: "Skipped (Hub connectivity check failed)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:        "hub-auth",
			Status:      "fail",
			Message:     "Failed to create authenticated Hub client",
			Remediation: "Run 'scion hub auth login' to authenticate",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.Auth().Me(ctx)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "hub-auth",
			Status:      "fail",
			Message:     fmt.Sprintf("Authentication failed: %v", err),
			Remediation: "Run 'scion hub auth login' to authenticate",
		}
	}

	return scionruntime.CheckResult{
		Name:    "hub-auth",
		Status:  "pass",
		Message: "Authenticated with Hub",
	}
}

// checkDoctorBrokerConnectivity performs D3: Broker connectivity check.
func checkDoctorBrokerConnectivity(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "broker-connectivity",
			Status:  "skip",
			Message: "Skipped (Hub unreachable)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:    "broker-connectivity",
			Status:  "skip",
			Message: "Skipped (Hub client not available)",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.RuntimeBrokers().List(ctx, nil)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to query brokers: %v", err),
			Remediation: "Check Hub server logs",
		}
	}

	if len(resp.Brokers) == 0 {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "fail",
			Message:     "No runtime brokers registered",
			Remediation: "Register a broker with 'scion server start'",
		}
	}

	onlineCount := 0
	for _, broker := range resp.Brokers {
		if broker.Status == "online" {
			onlineCount++
		}
	}

	if onlineCount == 0 {
		return scionruntime.CheckResult{
			Name:        "broker-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("%d broker(s) registered but none online", len(resp.Brokers)),
			Remediation: "Start a broker or check broker connectivity",
		}
	}

	return scionruntime.CheckResult{
		Name:    "broker-connectivity",
		Status:  "pass",
		Message: fmt.Sprintf("%d/%d broker(s) online", onlineCount, len(resp.Brokers)),
	}
}

// checkDoctorSAMappings performs D9: it reports, as a warning, each GCP
// service account registered in the linked project that no Kubernetes
// broker profile of the project maps in kubernetes_service_account_mappings
// (ptone/scion#3329 phase 2). The Hub computes the warnings; this check
// never fails, since an unmapped account only matters if it is assigned on
// a Kubernetes profile.
func checkDoctorSAMappings(hubEP string, hubConnected bool, client hubclient.Client, projectID string) scionruntime.CheckResult {
	const name = "gcp-sa-mappings"
	switch {
	case hubEP == "":
		return scionruntime.CheckResult{Name: name, Status: "skip", Message: "No Hub endpoint configured"}
	case !hubConnected:
		return scionruntime.CheckResult{Name: name, Status: "skip", Message: "Skipped (Hub unreachable)"}
	case client == nil:
		return scionruntime.CheckResult{Name: name, Status: "skip", Message: "Skipped (Hub client not available)"}
	case projectID == "":
		return scionruntime.CheckResult{Name: name, Status: "skip", Message: "Skipped (project not linked to Hub)"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sas, warnings, err := client.GCPServiceAccounts().ListWithWarnings(ctx, hubclient.ListForProject(projectID))
	if err != nil {
		return scionruntime.CheckResult{
			Name:    name,
			Status:  "warn",
			Message: fmt.Sprintf("Could not list the project's GCP service accounts: %v", err),
		}
	}
	if len(warnings) > 0 {
		return scionruntime.CheckResult{
			Name:   name,
			Status: "warn",
			Message: fmt.Sprintf("%d of %d registered GCP service account(s) not mapped on any Kubernetes broker profile:\n    %s",
				len(warnings), len(sas), strings.Join(warnings, "\n    ")),
			Remediation: "Add the service account to kubernetes_service_account_mappings in the broker's settings (and provision the bound Kubernetes ServiceAccount); the broker reports the change on its next heartbeat. Or assign a mapped service account on Kubernetes profiles",
		}
	}
	return scionruntime.CheckResult{
		Name:    name,
		Status:  "pass",
		Message: fmt.Sprintf("No unmapped GCP service accounts reported (%d registered)", len(sas)),
	}
}

// checkDoctorKubernetesBlockServiceAccount performs D10: it reports, as
// information, the Kubernetes profiles in vs (the local broker's global
// settings file; the Hub database overlay is not read) that have no
// kubernetes_block_service_account (ptone/scion#4034). A GCP identity
// "block" pod under such a profile runs as the namespace's default
// ServiceAccount, so its zero privilege depends on the namespace admin. It
// never warns or fails: running block pods as the namespace default is a
// supported choice.
func checkDoctorKubernetesBlockServiceAccount(vs *config.VersionedSettings) scionruntime.CheckResult {
	const name = "k8s-block-service-account"
	missing := vs.KubernetesProfilesWithoutBlockServiceAccount()
	if len(missing) == 0 {
		return scionruntime.CheckResult{
			Name:    name,
			Status:  "pass",
			Message: "Every Kubernetes profile in the local broker settings names a block ServiceAccount, or none is defined",
		}
	}
	return scionruntime.CheckResult{
		Name:   name,
		Status: "info",
		Message: fmt.Sprintf("%d Kubernetes profile(s) with no kubernetes_block_service_account in the local broker settings (~/.scion/settings.yaml; a Hub database overlay is not read); GCP identity \"block\" agents on them run as the namespace's default ServiceAccount: %s",
			len(missing), strings.Join(missing, ", ")),
		Remediation: "Optional: provision a dedicated Kubernetes ServiceAccount with no Workload Identity annotation and no IAM grants, and set kubernetes_block_service_account on the profile or its runtime entry",
	}
}

// checkDoctorAgentHealth performs D4, the agent check used when the health
// summary cannot be read. It covers the agents the caller can list (one
// page).
func checkDoctorAgentHealth(hubEP string, hubConnected bool, client hubclient.Client) scionruntime.CheckResult {
	if hubEP == "" {
		return scionruntime.CheckResult{
			Name:        "agent-health",
			Status:      "skip",
			Message:     "No Hub endpoint configured",
			Remediation: "Set SCION_HUB_ENDPOINT or use --hub flag",
		}
	}
	if !hubConnected {
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "skip",
			Message: "Skipped (Hub unreachable)",
		}
	}
	if client == nil {
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "skip",
			Message: "Skipped (Hub client not available)",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.Agents().List(ctx, nil)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "agent-health",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to query agents: %v", err),
			Remediation: "Check Hub server logs",
		}
	}

	if len(resp.Agents) == 0 {
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "pass",
			Message: "No agents registered",
		}
	}

	// The same policy as the Hub's health summary: agents in phase stopped
	// are not considered, and stalled agents are not a health signal. The
	// counts here are exclusive: an agent counts once, as errored (phase
	// error) before crashed before offline (activity). The summary's
	// problem groups can overlap instead (an agent in phase error with
	// activity crashed is in both groups).
	considered, errored, crashed, offline := 0, 0, 0, 0
	for _, agent := range resp.Agents {
		if agent.Phase == string(state.PhaseStopped) {
			continue
		}
		considered++
		switch {
		case agent.Phase == string(state.PhaseError):
			errored++
		case agent.Activity == string(state.ActivityCrashed):
			crashed++
		case agent.Activity == string(state.ActivityOffline):
			offline++
		}
	}

	if unhealthy := errored + crashed + offline; unhealthy > 0 {
		var parts []string
		if errored > 0 {
			parts = append(parts, fmt.Sprintf("%d in error", errored))
		}
		if crashed > 0 {
			parts = append(parts, fmt.Sprintf("%d crashed", crashed))
		}
		if offline > 0 {
			parts = append(parts, fmt.Sprintf("%d offline", offline))
		}
		return scionruntime.CheckResult{
			Name:    "agent-health",
			Status:  "warn",
			Message: fmt.Sprintf("%d of %d agent(s) not stopped need attention: %s", unhealthy, considered, strings.Join(parts, ", ")),
		}
	}

	return scionruntime.CheckResult{
		Name:    "agent-health",
		Status:  "pass",
		Message: fmt.Sprintf("No agent in error, crashed or offline (%d agent(s), %d not stopped)", len(resp.Agents), considered),
	}
}

// checkDoctorTelemetry performs D6: Telemetry pipeline check (local).
func checkDoctorTelemetry() scionruntime.CheckResult {
	enabled := os.Getenv("SCION_TELEMETRY_ENABLED")
	if enabled == "" || enabled == "false" || enabled == "0" {
		return scionruntime.CheckResult{
			Name:    "telemetry",
			Status:  "pass",
			Message: "Telemetry not enabled",
		}
	}

	endpoint := os.Getenv("SCION_OTEL_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	conn, err := net.DialTimeout("tcp", endpoint, 3*time.Second)
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "telemetry",
			Status:      "fail",
			Message:     fmt.Sprintf("Telemetry endpoint %s unreachable: %v", endpoint, err),
			Remediation: "Start the OpenTelemetry collector or check SCION_OTEL_ENDPOINT",
		}
	}
	_ = conn.Close()

	return scionruntime.CheckResult{
		Name:    "telemetry",
		Status:  "pass",
		Message: fmt.Sprintf("Telemetry endpoint %s is reachable", endpoint),
	}
}

// checkDoctorContainerImages performs D7: Container images check (local).
// Note: This is a simplified check that scans for any images with "scion" in
// the name. The design specifies checking per-configured-harness images, but
// that requires loading the project's harness config which adds complexity.
// A future enhancement could load harness configs and verify each image.
func checkDoctorContainerImages() scionruntime.CheckResult {
	// Find a container CLI (docker or podman)
	var containerCLI string
	for _, cli := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(cli); err == nil {
			containerCLI = cli
			break
		}
	}

	if containerCLI == "" {
		return scionruntime.CheckResult{
			Name:    "container-images",
			Status:  "skip",
			Message: "No container runtime CLI found (docker/podman)",
		}
	}

	imgCtx, imgCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer imgCancel()
	out, err := exec.CommandContext(imgCtx, containerCLI, "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return scionruntime.CheckResult{
			Name:    "container-images",
			Status:  "skip",
			Message: fmt.Sprintf("Could not list %s images: %v", containerCLI, err),
		}
	}

	imageOutput := strings.TrimSpace(string(out))
	if imageOutput == "" {
		return scionruntime.CheckResult{
			Name:        "container-images",
			Status:      "warn",
			Message:     fmt.Sprintf("No scion container images found via %s", containerCLI),
			Remediation: "Pull scion images or check image_registry configuration",
		}
	}

	images := strings.Split(imageOutput, "\n")
	scionImageCount := 0
	for _, img := range images {
		if strings.Contains(img, "scion") {
			scionImageCount++
		}
	}

	if scionImageCount == 0 {
		return scionruntime.CheckResult{
			Name:        "container-images",
			Status:      "warn",
			Message:     fmt.Sprintf("No scion container images found via %s", containerCLI),
			Remediation: "Pull scion images or check image_registry configuration",
		}
	}

	return scionruntime.CheckResult{
		Name:    "container-images",
		Status:  "pass",
		Message: fmt.Sprintf("%d scion image(s) found via %s", scionImageCount, containerCLI),
	}
}

// checkDoctorCredentialValidity performs D8: Broker credential validity check (local).
func checkDoctorCredentialValidity() scionruntime.CheckResult {
	store := brokercredentials.NewStore("")
	if !store.Exists() {
		return scionruntime.CheckResult{
			Name:    "broker-credentials",
			Status:  "skip",
			Message: "No broker credentials found (not a broker host)",
		}
	}

	creds, err := store.Load()
	if err != nil {
		return scionruntime.CheckResult{
			Name:        "broker-credentials",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to load broker credentials: %v", err),
			Remediation: "Re-register the broker with 'scion server start'",
		}
	}

	if creds.SecretKey == "" {
		return scionruntime.CheckResult{
			Name:        "broker-credentials",
			Status:      "fail",
			Message:     "Broker HMAC key is empty",
			Remediation: "Re-register the broker with 'scion server start'",
		}
	}

	return scionruntime.CheckResult{
		Name:    "broker-credentials",
		Status:  "pass",
		Message: "Broker credentials valid",
	}
}

func trimNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

// doctorServerHost is what 'scion doctor' can tell about the server on
// this host without the 'server start' flags.
type doctorServerHost struct {
	// configured: the global settings carry a server block, or a server or
	// broker daemon is running.
	configured bool
	// brokerOnly: the running daemon is a standalone broker
	// ('runtime-broker start') and no server daemon runs, so the hub is
	// known to be disabled.
	brokerOnly bool
}

// checkDoctorServerHubEndpointHere runs checkDoctorServerHubEndpoint
// against this host's global server config and server settings.
func checkDoctorServerHubEndpointHere() (scionruntime.CheckResult, bool) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return scionruntime.CheckResult{}, false
	}
	host := detectDoctorServerHost(globalDir)
	if !host.configured {
		return scionruntime.CheckResult{}, false
	}
	cfg, err := config.LoadGlobalConfig("")
	if err != nil {
		return scionruntime.CheckResult{}, false
	}
	// The global settings only: the server daemon runs in the global
	// directory, so the project settings of doctor's current directory are
	// not the server's.
	settings, err := config.LoadSettings(globalDir)
	if err != nil {
		return scionruntime.CheckResult{}, false
	}
	return checkDoctorServerHubEndpoint(cfg, settings, host, defaultServerWebPort())
}

// detectDoctorServerHost reports whether this host is configured as a
// server: a server block in the global settings file, or a running server
// or standalone broker daemon.
func detectDoctorServerHost(globalDir string) doctorServerHost {
	serverRunning, _, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	brokerRunning, _, _ := daemon.StatusComponent(brokerDaemonComponent, globalDir)
	return doctorServerHost{
		configured: serverRunning || brokerRunning || globalSettingsHaveServerBlock(globalDir),
		brokerOnly: brokerRunning && !serverRunning,
	}
}

// globalSettingsHaveServerBlock reports whether the settings file in
// globalDir has a top-level server key. It only reads the file.
func globalSettingsHaveServerBlock(globalDir string) bool {
	path := config.GetSettingsPath(globalDir)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false
	}
	_, ok := raw["server"]
	return ok
}

// defaultServerWebPort is the default of 'server start --web-port'.
func defaultServerWebPort() int {
	if f := serverStartCmd.Flags().Lookup("web-port"); f != nil {
		if p, err := strconv.Atoi(f.DefValue); err == nil {
			return p
		}
	}
	return webPort
}

// checkDoctorServerHubEndpoint performs D11: it reports the server hub
// endpoint deprecation with serverHubEndpointDeprecated, the check server
// startup logs. Doctor does not see the server's flags, so unless the host
// is known to run a standalone broker it evaluates every mode the server
// could run in (workstation, hosted with and without the web frontend,
// broker only, with default ports) and warns only when the server would be
// affected in all of them: it never warns on a guess. For the same reason a
// loopback legacy value is indeterminate unless the host runs a standalone
// broker: a workstation on a port doctor cannot see may have written it as
// its own endpoint. The bool is false when there is nothing to report.
func checkDoctorServerHubEndpoint(cfg *config.GlobalConfig, settings *config.Settings, host doctorServerHost, defaultWebPort int) (scionruntime.CheckResult, bool) {
	if !host.configured {
		return scionruntime.CheckResult{}, false
	}
	brokerOnly := serverEndpointMode{brokerEnabled: true}
	modes := []serverEndpointMode{brokerOnly}
	if !host.brokerOnly {
		modes = append(modes,
			serverEndpointMode{hubEnabled: true, brokerEnabled: true, webEnabled: true, webPort: defaultWebPort},
			serverEndpointMode{hubEnabled: true, brokerEnabled: true, webEnabled: true, webPort: defaultWebPort, hosted: true},
			serverEndpointMode{hubEnabled: true, hosted: true},
		)
	}
	legacy := settings.GetHubEndpoint()
	if !host.brokerOnly && isLoopbackURL(legacy) {
		return scionruntime.CheckResult{}, false
	}
	for _, m := range modes {
		if !serverHubEndpointDeprecated(cfg, m, legacy) {
			return scionruntime.CheckResult{}, false
		}
	}
	return scionruntime.CheckResult{
		Name:    "server-hub-endpoint",
		Status:  "warn",
		Message: serverHubEndpointDeprecationWarning,
	}, true
}
