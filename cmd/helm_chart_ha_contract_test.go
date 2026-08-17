package cmd

// The scion-hub Helm chart's claims about the hub, re-derived from the hub.
//
// WHY THIS FILE IS IN THE REPOSITORY AND NOT UNDER deploy/helm/. Two reasons,
// and only the second one is interesting.
//
// The mechanical reason: validateHostedHAPreflight, isHADeployment, hostedMode
// and enableHub are unexported, so a driver for them lives in package cmd or
// nowhere.
//
// The reason that matters: the chart's prose carried a list of eight preflight
// gates, written down three times - in templates/_helpers.tpl, in
// templates/NOTES.txt, and as the literal 8 in tests/render-guards.sh. The
// three agreed with each other and with nothing in the hub. gd-p1-rev then
// walked the same preflight and got NINE, because their walk supplied a
// malformed IAP audience where mine supplied a well-formed one, and the format
// check is a separate gate. The guard could not see the disagreement, because
// the guard and the claim shared the constant.
//
// So the list is not written down any more. It is DERIVED, here, by driving the
// real preflight over the settings.yaml the chart actually renders - pulled out
// of the chart's own committed golden files - and written to
// deploy/helm/scion-hub/hack/ha-gates.txt. The chart's shell suite checks its
// prose against that artifact. When Cloud SQL lands and renders
// server.database.url, this walk returns one fewer gate on its own and there is
// no constant for anyone to decrement.
//
// THE WALK'S OUTPUT IS A FUNCTION OF WHAT THE WALK SUPPLIES. That is the whole
// lesson of the eight-versus-nine, so every step below records the value it
// supplied, and an arm that cannot construct the next input says so rather than
// reporting a shorter list. "The probe ran out of moves" and "the hub ran out
// of gates" are different outcomes and a bare count cannot tell them apart.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

var updateChartContract = flag.Bool("update-chart-contract", false,
	"rewrite deploy/helm/scion-hub/hack/ha-gates.txt from this run")

// chartDir is the chart root, relative to this package's directory.
const chartDir = "../deploy/helm/scion-hub"

// gatesArtifact is the derived list the chart's prose is checked against.
const gatesArtifact = chartDir + "/hack/ha-gates.txt"

// audienceWellFormed is a Cloud Run IAP audience in the shape
// isSupportedIAPAudience accepts. audienceMalformed is the shape an operator
// produces by pasting an OAuth client ID into the field, which is the mistake
// the format gate exists to catch.
const (
	audienceWellFormed = "/projects/1/locations/us-central1/services/hub"
	audienceMalformed  = "my-iap-audience"
)

// supplier grants exactly the thing one gate asked for, and reports what it
// granted. A supplier that returns ok=false has hit the probe's limit, not the
// hub's, and the walk records that distinction rather than silently stopping.
type supplier func(cfg *config.GlobalConfig, msg string) (granted string, ok bool)

// gateSupplier builds the supply table. audience is a parameter and not a
// constant precisely because the count depends on it.
func gateSupplier(audience string) supplier {
	// audienceTried guards against a walk that supplies a value the gate
	// rejects and then supplies it again forever.
	audienceTried := false
	return func(cfg *config.GlobalConfig, msg string) (string, bool) {
		switch {
		case strings.Contains(msg, "explicit server.hub.hub_id"):
			cfg.Hub.HubID = "probe-hub"
			return `server.hub.hub_id="probe-hub"`, true
		case strings.Contains(msg, "server.database.driver=postgres"):
			cfg.Database.Driver = "postgres"
			return `server.database.driver="postgres"`, true
		case strings.Contains(msg, "server.database.url"):
			cfg.Database.URL = "postgres://probe/db"
			return `server.database.url="postgres://probe/db"`, true
		case strings.Contains(msg, "server.storage.provider=gcs"):
			cfg.Storage.Provider = "gcs"
			cfg.Storage.Bucket = "probe-bucket"
			return `server.storage.provider="gcs" + bucket`, true
		case strings.Contains(msg, "durable session/signing secret"):
			// resolveSessionSecret reads this by literal os.Getenv, so the
			// grant has to be an environment variable and not a config field.
			_ = os.Setenv("SCION_SERVER_SESSION_SECRET", "probe-secret")
			return `env SCION_SERVER_SESSION_SECRET="probe-secret"`, true
		case strings.Contains(msg, "server.auth.mode=proxy"):
			cfg.Auth.Mode = "proxy"
			return `server.auth.mode="proxy"`, true
		case strings.Contains(msg, "server.auth.proxy.provider=iap"):
			if cfg.Auth.Proxy == nil {
				cfg.Auth.Proxy = &config.ProxyAuthConfig{}
			}
			cfg.Auth.Proxy.Provider = "iap"
			return `server.auth.proxy.provider="iap"`, true
		case strings.Contains(msg, "server.auth.proxy.iap.audience"),
			strings.Contains(msg, "supported IAP audience"):
			if audienceTried {
				// THE PROBE'S LIMIT, NOT THE HUB'S. This arm has exactly one
				// audience to offer and the gate refused it. Re-offering it
				// would spin; substituting a different one would answer a
				// question this arm was not asked.
				return "", false
			}
			audienceTried = true
			if cfg.Auth.Proxy == nil {
				cfg.Auth.Proxy = &config.ProxyAuthConfig{}
			}
			if cfg.Auth.Proxy.IAP == nil {
				cfg.Auth.Proxy.IAP = &config.IAPAuthConfig{}
			}
			cfg.Auth.Proxy.IAP.Audience = audience
			return fmt.Sprintf("server.auth.proxy.iap.audience=%q", audience), true
		case strings.Contains(msg, "server.auth.transport; do not"):
			cfg.Auth.Transport = &config.TransportAuthConfig{}
			return "server.auth.transport={}", true
		case strings.Contains(msg, "server.auth.transport.mode=iap"):
			cfg.Auth.Transport.Mode = "iap"
			return `server.auth.transport.mode="iap"`, true
		case strings.Contains(msg, "server.auth.transport.oidc_audience"):
			cfg.Auth.Transport.OIDCAudience = "probe-client-id"
			return `server.auth.transport.oidc_audience="probe-client-id"`, true
		case strings.Contains(msg, "server.auth.transport.platform_auth_sa"):
			cfg.Auth.Transport.PlatformAuthSA = "probe@example.iam.gserviceaccount.com"
			return `server.auth.transport.platform_auth_sa="probe@..."`, true
		}
		return "", false
	}
}

// TestHelmChartHAGateWalk derives the gate list and compares it to the
// committed artifact. Run with -update-chart-contract to rewrite the artifact.
func TestHelmChartHAGateWalk(t *testing.T) {
	goldens, err := filepath.Glob(chartDir + "/golden/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// THE DENOMINATOR. A glob that silently returns two files would produce a
	// shorter artifact that still round-trips against itself.
	if len(goldens) != 5 {
		t.Fatalf("expected 5 chart goldens, found %d (%v). The walk's corpus is the chart's golden set; if that set changed, this number changes with it in the same commit.",
			len(goldens), goldens)
	}
	sort.Strings(goldens)

	var b strings.Builder
	b.WriteString("# DERIVED, not written. Regenerate with:\n")
	b.WriteString("#   go test ./cmd -run TestHelmChartHAGateWalk -update-chart-contract\n")
	b.WriteString("#\n")
	b.WriteString("# Each arm loads the settings.yaml the chart renders - taken from the\n")
	b.WriteString("# golden named in the header - through the real config.LoadGlobalConfig,\n")
	b.WriteString("# then drives the real validateHostedHAPreflight, satisfying one gate at a\n")
	b.WriteString("# time because the preflight returns on first failure.\n")
	b.WriteString("#\n")
	b.WriteString("# THE COUNT DEPENDS ON WHAT THE ARM SUPPLIES. Each step records its grant.\n")
	b.WriteString("# The two audience arms differ in exactly one value and return different\n")
	b.WriteString("# lists; that is a fact about the hub, not a defect in the walk.\n")
	b.WriteString("#\n")
	b.WriteString("# CORPUS BINDING. This walk reads the committed goldens, so on its own it\n")
	b.WriteString("# measures the goldens and not the chart. The digests below close that gap\n")
	b.WriteString("# from the other end: hack/verify.sh proves the goldens are a current render\n")
	b.WriteString("# and then checks these digests against the files it just proved, so a walk\n")
	b.WriteString("# taken over a stale golden cannot be presented as a walk over the chart.\n")
	b.WriteString("# Neither half is sufficient alone and the shell step names the other.\n")
	b.WriteString("#\n")
	b.WriteString("# GOLDEN DIGESTS (sha256, as walked)\n")
	for _, g := range goldens {
		raw, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(&b, "#   %-24s %s\n", filepath.Base(g), hex.EncodeToString(sum[:]))
	}

	totalRefusals := 0
	for _, g := range goldens {
		settings, err := settingsFromGolden(g)
		if err != nil {
			// A golden with no rendered settings.yaml is a real chart shape
			// (config.existingSecret renders none), and it is a distinct
			// outcome from a walk that found nothing to refuse.
			fmt.Fprintf(&b, "\n===== %s\nNO RENDERED settings.yaml: %v\n", filepath.Base(g), err)
			continue
		}
		for _, arm := range []struct{ label, audience string }{
			{"audience well-formed", audienceWellFormed},
			{"audience malformed", audienceMalformed},
		} {
			fmt.Fprintf(&b, "\n===== %s [%s = %q]\n", filepath.Base(g), arm.label, arm.audience)
			totalRefusals += walkOne(t, &b, settings, gateSupplier(arm.audience))
		}
	}

	// ANTI-VACUITY. Every failure mode above - an extractor that returns
	// nothing, a HOME that resolves to a settings.yaml with no HA shape, a
	// preflight that stops running - produces an artifact full of orderly
	// TERMINATED lines that round-trips against itself forever. The number is
	// not the subject of any claim; it exists so that zero cannot pass.
	if totalRefusals == 0 {
		t.Fatal("VACUOUS: the walk recorded no refusals at all across every golden and both audience arms. Whatever this run measured, it was not the hub's preflight.")
	}

	got := b.String()
	if *updateChartContract {
		if err := os.WriteFile(gatesArtifact, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", gatesArtifact)
		return
	}
	wantBytes, err := os.ReadFile(gatesArtifact)
	if err != nil {
		t.Fatalf("%v\n\nThe chart's prose is checked against this artifact, so its absence is not a passing run. Regenerate it with -update-chart-contract.", err)
	}
	if string(wantBytes) != got {
		t.Errorf("%s is stale. The hub's preflight no longer produces the committed gate list.\n\n--- committed\n%s\n--- derived now\n%s",
			gatesArtifact, string(wantBytes), got)
	}
}

// walkOne drives one arm, writes its steps, and returns the number of refusals
// it saw. The return value feeds the anti-vacuity assertion and is deliberately
// NOT written into the artifact: a count in the artifact is a constant again.
func walkOne(t *testing.T, b *strings.Builder, settings string, sup supplier) int {
	t.Helper()
	// PER-ARM ISOLATION. resolveSessionSecret reads the environment, and an
	// arm that inherits the previous arm's grant comes out one gate short.
	for _, k := range []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "K_SERVICE"} {
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("could not clear %s, so this arm would inherit the previous arm's grant: %v", k, err)
		}
	}

	// HOME IS EMPTIED, AND IT IS NOT BELT-AND-BRACES. LoadGlobalConfig falls
	// back to GetGlobalDir() when its argument yields nothing, so a walk run on
	// a machine with a real ~/.scion/settings.yaml measures that file and
	// reports the result as a fact about the chart. gd-p1-rev lost a run to
	// exactly this and nearly filed a precondition failure against this chart.
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	// The real loader, not yaml.Unmarshal: the yaml tag is hubId and the chart
	// renders hub_id, so a plain unmarshal manufactures a gate-1 failure that
	// does not exist.
	cfg, err := config.LoadGlobalConfig(dir)
	if err != nil {
		fmt.Fprintf(b, "TERMINATED loading the rendered settings.yaml: %v\n", err)
		return 0
	}
	hostedMode = cfg.Mode == "hosted" || cfg.Mode == "production"
	enableHub = true

	fmt.Fprintf(b, "config   mode=%q hub_id=%q driver=%q storage=%q auth.mode=%q\n",
		cfg.Mode, cfg.Hub.HubID, cfg.Database.Driver, cfg.Storage.Provider, cfg.Auth.Mode)
	fmt.Fprintf(b, "routes   isHADeployment=%v hostedHAGuardsRequired=%v\n",
		isHADeployment(cfg), hostedHAGuardsRequired(cfg))
	if !hostedHAGuardsRequired(cfg) {
		fmt.Fprintf(b, "TERMINATED no gates: the preflight does not run for this shape.\n")
		return 0
	}

	n := 0
	// The bound is generous and asserted below, so a preflight that grew a
	// cycle reports the cycle rather than hanging.
	for i := 0; i < 32; i++ {
		perr := validateHostedHAPreflight(cfg)
		if perr == nil {
			fmt.Fprintf(b, "TERMINATED the hub ran out of gates: preflight passes after %d.\n", n)
			return n
		}
		n++
		granted, ok := sup(cfg, perr.Error())
		fmt.Fprintf(b, "%2d  %s\n", n, oneLine(perr.Error()))
		if !ok {
			fmt.Fprintf(b, "TERMINATED the probe ran out of moves at gate %d. This is a LIMIT OF THIS ARM, not a\n", n)
			fmt.Fprintf(b, "           property of the hub: there may be further gates behind this one.\n")
			return n
		}
		fmt.Fprintf(b, "    granted %s\n", granted)
	}
	fmt.Fprintf(b, "TERMINATED the walk hit its 32-step bound, which means the preflight is not making\n")
	fmt.Fprintf(b, "           progress under these grants.\n")
	return n
}

// oneLine collapses a multi-line refusal so one gate is one line. The parity
// check downstream counts lines.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// settingsFromGolden lifts the rendered settings.yaml out of a golden manifest
// set. The goldens are helm template output and the file lives in a Secret's
// stringData block.
func settingsFromGolden(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	const key = "  settings.yaml: |"
	start := -1
	for i, l := range lines {
		if l == key {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no %q block in this golden", strings.TrimSpace(key))
	}
	var out []string
	for _, l := range lines[start:] {
		if l == "" {
			out = append(out, "")
			continue
		}
		if !strings.HasPrefix(l, "    ") {
			break
		}
		out = append(out, strings.TrimPrefix(l, "    "))
	}
	s := strings.Join(out, "\n")
	// NON-VACUITY. An extractor that silently returned "" would make every
	// downstream arm report "no gates" and look like good news.
	if !strings.Contains(s, "\nserver:") && !strings.HasPrefix(s, "server:") {
		return "", fmt.Errorf("extracted %d bytes with no top-level server: key; the extractor is not reading what it thinks it is", len(s))
	}
	return s, nil
}

// TestHelmChartSigningKeyContract drives the three cases values.yaml claims to
// have measured through the real hub.New.
//
// WHY IT IS HERE. The claim was true and the apparatus was not shipped:
// gd-p1-rev found that hub.New occurred exactly once in the chart's 35 files,
// inside the comment claiming to have measured it. A measurement whose
// apparatus is not in the tree is a quotation, and it goes stale with nothing
// able to notice - the same defect hack/run-all-mutations.sh was written to fix
// for the mutation table.
func TestHelmChartSigningKeyContract(t *testing.T) {
	cases := []struct {
		name           string
		requireStable  bool
		sharedSecret   string
		wantErr        bool
		wantErrContain string
	}{
		{
			name:           "required and no shared secret refuses",
			requireStable:  true,
			wantErr:        true,
			wantErrContain: "RequireStableSigningKey is set",
		},
		{
			// THE POSITIVE TWIN. Without it, a hub.New that refused for any
			// reason at all would pass the case above.
			name:          "required with a shared secret starts",
			requireStable: true,
			sharedSecret:  "a-durable-session-secret",
		},
		{
			// The shipped chart default. If this ever refuses, the chart's
			// claim that false is the survivable value is gone.
			name:          "the chart default starts",
			requireStable: false,
		},
	}
	// THE DENOMINATOR, and it fails in both directions: a case deleted here
	// makes the count short, a case added without thought makes it long.
	if len(cases) != 3 {
		t.Fatalf("this contract is three cases, found %d", len(cases))
	}
	ran := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			gdir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(gdir, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "sqlite"
			cfg.Database.URL = filepath.Join(gdir, "hub.db")
			ctx := context.Background()
			s, entClient, err := initStore(ctx, cfg)
			if err != nil {
				t.Fatalf("initStore: %v", err)
			}
			if entClient != nil {
				defer func() { _ = entClient.Close() }()
			}
			_, err = hub.New(hub.ServerConfig{
				HubID:                   "chart-contract-hub",
				RequireStableSigningKey: tc.requireStable,
				SharedSigningSecret:     tc.sharedSecret,
			}, s)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("hub.New returned no error; the chart's claim that this shape cannot start is false")
				}
				if !strings.Contains(err.Error(), tc.wantErrContain) {
					t.Fatalf("hub.New refused for a different reason than the chart claims.\n got: %v\nwant substring: %q", err, tc.wantErrContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("hub.New refused: %v", err)
			}
		})
		ran++
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d cases", ran, len(cases))
	}
}
