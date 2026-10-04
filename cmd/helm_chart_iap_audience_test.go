package cmd

// The scion-hub chart's two copies of isSupportedIAPAudience, checked against
// the hub function itself.
//
// The chart refuses an HA render whose IAP audience the hub's preflight would
// refuse, and it does so in two places that cannot call Go:
//
//   - templates/_helpers.tpl, define "scion-hub.iapAudiencePattern": a plain
//     pattern matched on the NORMALISED audience,
//     trim(regexReplaceAll "/+$" (trim aud) "").
//   - values.schema.json: a pattern matched on the audience AS WRITTEN, which
//     is the template's pattern widened by exactly what the normalisation
//     removes.
//
// Both are compared here, value by value, with the hub's own decision:
// isSupportedIAPAudience(strings.TrimRight(strings.TrimSpace(aud), "/")),
// which is what validateHostedHAPreflight computes. If the hub loosens or
// tightens the check, a row below disagrees and this test fails, rather than
// the chart over- or under-refusing silently.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// hubAcceptsIAPAudience is validateHostedHAPreflight's audience decision.
func hubAcceptsIAPAudience(aud string) bool {
	if strings.TrimSpace(aud) == "" {
		return false
	}
	return isSupportedIAPAudience(strings.TrimRight(strings.TrimSpace(aud), "/"))
}

// chartNormaliseAudience mirrors the template's normalisation. Sprig's trim is
// strings.TrimSpace, and regexReplaceAll "/+$" with no multi-line flag removes
// exactly the trailing slashes, which is strings.TrimRight(s, "/").
func chartNormaliseAudience(aud string) string {
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(aud), "/"))
}

var helperAudiencePatternRE = regexp.MustCompile(
	`(?s)\{\{- define "scion-hub\.iapAudiencePattern" -\}\}\n([^\n]*)\n\{\{- end \}\}`)

func helperAudiencePattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(chartDir + "/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	m := helperAudiencePatternRE.FindAllSubmatch(raw, -1)
	if len(m) != 1 {
		t.Fatalf("expected exactly one scion-hub.iapAudiencePattern define in _helpers.tpl, found %d", len(m))
	}
	re, err := regexp.Compile(string(m[0][1]))
	if err != nil {
		t.Fatalf("scion-hub.iapAudiencePattern does not compile as a Go regexp: %v", err)
	}
	return re
}

// schemaAudiencePatterns returns every "pattern" set on a property named
// audience anywhere in values.schema.json.
func schemaAudiencePatterns(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(chartDir + "/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, child := range n {
				if k == "audience" {
					if m, ok := child.(map[string]any); ok {
						if p, ok := m["pattern"].(string); ok {
							out = append(out, p)
						}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

func TestHelmChartIAPAudiencePattern(t *testing.T) {
	helper := helperAudiencePattern(t)
	patterns := schemaAudiencePatterns(t)
	if len(patterns) != 1 {
		t.Fatalf("expected exactly one audience pattern in values.schema.json, found %d: %q", len(patterns), patterns)
	}
	// helm 3.16's schema validator compiles patterns with Go's regexp, so
	// compiling it here is the same dialect.
	schema, err := regexp.Compile(patterns[0])
	if err != nil {
		t.Fatalf("the schema's audience pattern does not compile as a Go regexp: %v", err)
	}

	cases := []struct {
		name, aud string
	}{
		// The two forms.
		{"cloud run", "/projects/123/locations/us-central1/services/hub"},
		{"backend service", "/projects/123/global/backendServices/456"},
		// All-zero placeholders, accepted by the hub with a warning.
		{"placeholder backend service", "/projects/000000000000/global/backendServices/0"},
		{"placeholder cloud run", "/projects/0/locations/0/services/0"},
		// Empty segments.
		{"empty project", "/projects//global/backendServices/456"},
		{"empty region", "/projects/123/locations//services/hub"},
		{"empty service", "/projects/123/locations/us-central1/services/"},
		{"empty backend id", "/projects/123/global/backendServices/"},
		{"empty backend id, slashes", "/projects/123/global/backendServices//"},
		{"whitespace-only last segment", "/projects/123/global/backendServices/ /"},
		{"whitespace-only middle segment", "/projects/ /global/backendServices/456"},
		// Extra and missing segments.
		{"extra segment", "/projects/123/global/backendServices/456/extra"},
		{"extra segment cloud run", "/projects/123/locations/r/services/hub/x"},
		{"missing leading slash", "projects/123/global/backendServices/456"},
		{"double leading slash", "//projects/123/global/backendServices/456"},
		{"missing segment", "/projects/123/backendServices/456"},
		{"wrong literal", "/projects/123/global/backendservices/456"},
		{"mixed forms", "/projects/123/locations/r/backendServices/456"},
		{"oauth client id", "123-abc.apps.googleusercontent.com"},
		{"bare word", "my-iap-audience"},
		// Trailing slashes.
		{"one trailing slash", "/projects/123/global/backendServices/456/"},
		{"many trailing slashes", "/projects/123/locations/r/services/hub///"},
		{"slash, space, slash", "/projects/123/global/backendServices/456/ /"},
		{"space before slash", "/projects/123/global/backendServices/456 /"},
		// Surrounding whitespace, ASCII and not.
		{"surrounding spaces", "  /projects/123/global/backendServices/456  "},
		{"tabs and newlines", "\t/projects/123/global/backendServices/456\n"},
		{"spaces and slashes", " /projects/123/locations/r/services/hub// "},
		{"vertical tab", "\v/projects/123/global/backendServices/456\v"},
		{"NEL", "\u0085/projects/123/global/backendServices/456"},
		{"NBSP", "/projects/123/global/backendServices/456\u00a0"},
		{"ideographic space and slash", "\u3000/projects/123/locations/r/services/hub/\u3000"},
		{"line separator", "/projects/123/global/backendServices/456\u2028/"},
		{"interior space in id", "/projects/123/global/backendServices/4 56"},
		{"leading space in id", "/projects/123/global/backendServices/ 456"},
		{"non-breaking last segment only", "/projects/123/global/backendServices/\u00a0"},
		// Degenerate.
		{"whitespace only", "   "},
		{"slash only", "/"},
	}
	for _, c := range cases {
		hub := hubAcceptsIAPAudience(c.aud)
		if got := helper.MatchString(chartNormaliseAudience(c.aud)); got != hub {
			t.Errorf("%s (%q): _helpers.tpl iapAudiencePattern on the normalised value says %v, the hub says %v", c.name, c.aud, got, hub)
		}
		if got := schema.MatchString(c.aud); got != hub {
			t.Errorf("%s (%q): values.schema.json pattern on the raw value says %v, the hub says %v", c.name, c.aud, got, hub)
		}
	}

	// THE ONE DELIBERATE DIFFERENCE. The schema passes the empty string,
	// because a missing audience is refused by a separate rule with its own
	// message; the hub refuses it too, under a different gate.
	if !schema.MatchString("") {
		t.Error("the schema's audience pattern refuses the empty string; the empty audience is meant to reach the required-audience rule and its message")
	}
	if hubAcceptsIAPAudience("") {
		t.Error("the hub accepts an empty IAP audience; the chart's empty-audience refusal no longer matches it")
	}

	// ANTI-VACUITY: the table must contain both outcomes.
	var accepted, refused int
	for _, c := range cases {
		if hubAcceptsIAPAudience(c.aud) {
			accepted++
		} else {
			refused++
		}
	}
	if accepted < 5 || refused < 5 {
		t.Fatalf("the table has %d accepted and %d refused audiences; it no longer exercises both sides of the check", accepted, refused)
	}
}
