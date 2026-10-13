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
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The Platform Health section renders the Hub's health summary (GET
// /api/v1/admin/health/summary, ptone/scion#4727), the data behind the web
// health dashboard. Doctor renders it and does not re-derive anything: the
// headline is the summary's status, the attention lines are the summary's
// ranked attention list with its severities, and every other line is
// information.

const (
	healthSummaryHealthy   = "healthy"
	healthSummaryDegraded  = "degraded"
	healthSummaryUnhealthy = "unhealthy"

	// doctorHealthSummaryCheck names the Platform Health headline line.
	doctorHealthSummaryCheck = "health-summary"

	// doctorHealthSummaryForbidden is the skip line for a caller that may
	// not read the summary.
	doctorHealthSummaryForbidden = "requires hub.health.read"
	// doctorHealthSummaryUnavailable is shown for a 503: the serving Hub
	// instance cannot read its database. The wording matches the web
	// dashboard; the response body is not echoed.
	doctorHealthSummaryUnavailable = "Health data not available (database unreachable from this hub instance)"
)

// doctorPlatformHealth is the outcome of reading the health summary:
// either the summary, or the lines that explain why it is not available.
type doctorPlatformHealth struct {
	summary *hubclient.HealthSummary
	checks  []scionruntime.CheckResult
}

// fetchDoctorPlatformHealth reads the health summary when the Hub answered
// the connectivity check (hubReachable). A 403 is a skip
// (the caller lacks hub.health.read or is not a user identity), a 503 is a
// warning with a fixed message, and any other failure is a warning; in
// every one of those cases the caller falls back to the list-based checks.
func fetchDoctorPlatformHealth(hubEP string, hubReachable bool, client hubclient.Client) doctorPlatformHealth {
	skip := func(msg string) doctorPlatformHealth {
		return doctorPlatformHealth{checks: []scionruntime.CheckResult{{Name: doctorHealthSummaryCheck, Status: "skip", Message: msg}}}
	}
	switch {
	case hubEP == "":
		return skip("No Hub endpoint configured")
	case !hubReachable:
		return skip("Skipped (Hub connectivity check failed)")
	case client == nil:
		return skip("Skipped (Hub client not available)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	summary, err := client.HealthSummary(ctx)
	if err == nil && summary != nil {
		return doctorPlatformHealth{summary: summary}
	}

	var apiErr *apiclient.APIError
	statusCode := 0
	if errors.As(err, &apiErr) {
		statusCode = apiErr.StatusCode
	}
	switch statusCode {
	case http.StatusForbidden:
		return doctorPlatformHealth{checks: []scionruntime.CheckResult{{
			Name:        doctorHealthSummaryCheck,
			Status:      "skip",
			Message:     doctorHealthSummaryForbidden,
			Remediation: "Platform Health needs a hub admin login, or a hub-boundary access token with hub_health:read (see 'scion doctor --help'). The broker and agent checks above cover only what you can list",
		}}}
	case http.StatusServiceUnavailable:
		return doctorPlatformHealth{checks: []scionruntime.CheckResult{{
			Name:        doctorHealthSummaryCheck,
			Status:      "warn",
			Message:     doctorHealthSummaryUnavailable,
			Remediation: "Check the Hub's database connectivity and the Hub server logs",
		}}}
	case http.StatusNotFound:
		return skip("This Hub does not provide a health summary (older Hub version)")
	}
	if err == nil {
		err = errors.New("empty response")
	}
	return doctorPlatformHealth{checks: []scionruntime.CheckResult{{
		Name:   doctorHealthSummaryCheck,
		Status: "warn",
		// The error can carry a proxy's or the Hub's response body.
		Message: singleLine(fmt.Sprintf("Could not read the health summary: %v", err)),
	}}}
}

// renderDoctorHealthSummary turns the summary into Platform Health lines:
// the headline, every attention item in order, then one information line
// per section. verbose adds the per-broker, per-phase, per-integration
// and per-instance detail.
func renderDoctorHealthSummary(summary *hubclient.HealthSummary, verbose bool) []scionruntime.CheckResult {
	s, err := singleLineHealthSummary(summary)
	if err != nil {
		// Without a safe copy only the status is shown; the JSON output
		// still carries the whole summary.
		return []scionruntime.CheckResult{
			healthSummaryHeadline(&hubclient.HealthSummary{Status: singleLine(summary.Status), Attention: summary.Attention}),
			{Name: doctorHealthSummaryCheck, Status: "warn", Message: "Could not display the health summary details; use --format json to see them"},
		}
	}
	out := []scionruntime.CheckResult{healthSummaryHeadline(s)}
	for _, a := range s.Attention {
		status := "warn"
		if a.Severity == "critical" {
			status = "fail"
		}
		out = append(out, scionruntime.CheckResult{Name: "attention", Status: status, Message: a.Message})
	}

	info := func(name, msg string) {
		out = append(out, scionruntime.CheckResult{Name: name, Status: "info", Message: msg})
	}
	info("hub", healthSummaryHubLine(s))
	info("runtime-brokers", healthSummaryBrokersLine(s, verbose))
	info("agents", healthSummaryAgentsLine(s, verbose))
	info("dispatch", healthSummaryDispatchLine(s))
	info("integrations", healthSummaryIntegrationsLine(s, verbose))
	if sa := s.ServiceAccountCheck; sa != nil {
		msg := "The service account assignment check cannot run"
		if sa.Cause != "" {
			msg += ": " + sa.Cause
		}
		if len(sa.Instances) > 0 {
			msg += " (instances: " + strings.Join(sa.Instances, ", ") + ")"
		}
		remedy := sa.Remedy
		if sa.DocsURL != "" {
			if remedy != "" {
				remedy += " "
			}
			remedy += "See " + sa.DocsURL
		}
		out = append(out, scionruntime.CheckResult{Name: "service-account-check", Status: "info", Message: msg, Remediation: remedy})
	}
	if line, ok := healthSummaryTestIdentityLine(s.Hub); ok {
		info("test-identities", line)
	}
	if verbose {
		info("hub-instances", healthSummaryInstancesLine(s))
		served := s.Hub.InstanceID
		if s.HubInstances != nil {
			for _, it := range s.HubInstances.Items {
				if it.Serving {
					served = it.Label
				}
			}
		}
		info("generated", fmt.Sprintf("%s, served by %s", clitime.Format(s.GeneratedAt, clitime.Full), orDash(served)))
	}
	return out
}

// singleLineHealthSummary returns a deep copy of s in which every string
// (names, messages, reasons, map keys and values) is on one line: CR and
// LF become spaces and other control characters are removed. Only the
// renderer then adds line breaks, so a value from a broker or an
// integration cannot start a line that looks like a check. s itself, and
// its raw body, are never changed: when no deep copy can be made it
// returns an error instead.
func singleLineHealthSummary(s *hubclient.HealthSummary) (*hubclient.HealthSummary, error) {
	cp, err := cloneHealthSummary(s)
	if err != nil {
		return nil, err
	}
	// The clone has no Raw (it is not part of the JSON form). Attach it
	// only after the walk, so reflection never visits the raw body byte by
	// byte; it holds no strings to clean.
	singleLineStrings(reflect.ValueOf(cp).Elem())
	cp.Raw = s.Raw
	return cp, nil
}

// cloneHealthSummary deep-copies s through its JSON form. A variable so
// tests can make it fail.
var cloneHealthSummary = func(s *hubclient.HealthSummary) (*hubclient.HealthSummary, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var cp hubclient.HealthSummary
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// singleLineStrings applies singleLine to every settable string reachable
// from v, including the keys and values of string maps.
func singleLineStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			singleLineStrings(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				singleLineStrings(f)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			singleLineStrings(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() || v.Type().Key().Kind() != reflect.String || v.Type().Elem().Kind() != reflect.String {
			return
		}
		clean := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			clean.SetMapIndex(reflect.ValueOf(singleLine(iter.Key().String())).Convert(v.Type().Key()),
				reflect.ValueOf(singleLine(iter.Value().String())).Convert(v.Type().Elem()))
		}
		if v.CanSet() {
			v.Set(clean)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(singleLine(v.String()))
		}
	}
}

// singleLine turns CR and LF into spaces and removes every other control
// character (see stripControlChars).
func singleLine(s string) string {
	return stripControlChars(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s))
}

func healthSummaryHeadline(s *hubclient.HealthSummary) scionruntime.CheckResult {
	res := scionruntime.CheckResult{Name: doctorHealthSummaryCheck}
	attention := ""
	if n := len(s.Attention); n > 0 {
		attention = fmt.Sprintf(" (%d item(s) need attention)", n)
	}
	switch s.Status {
	case healthSummaryHealthy:
		res.Status, res.Message = "pass", "Platform is healthy"+attention
	case healthSummaryDegraded:
		res.Status, res.Message = "warn", "Platform is degraded"+attention
	case healthSummaryUnhealthy:
		res.Status, res.Message = "fail", "Platform is unhealthy"+attention
	default:
		res.Status, res.Message = "warn", fmt.Sprintf("Platform reported status %q%s", s.Status, attention)
	}
	return res
}

func healthSummaryHubLine(s *hubclient.HealthSummary) string {
	h := s.Hub
	if h.Instances == nil {
		return "Hub instance data not available"
	}
	f := h.Instances
	line := fmt.Sprintf("fleet %s: %d live instance(s), %d healthy, %d degraded, %d unhealthy",
		orDash(h.Status), f.Live, f.Healthy, f.Degraded, f.Unhealthy)
	switch h.Version {
	case "":
	case "mixed":
		line += "; versions mixed"
	default:
		line += "; version " + h.Version
	}
	return line
}

func healthSummaryBrokersLine(s *hubclient.HealthSummary, verbose bool) string {
	b := s.Brokers
	if b.NotReported {
		return "Runtime broker data not available"
	}
	online := 0
	for _, it := range b.Items {
		if it.Status == "online" {
			online++
		}
	}
	line := fmt.Sprintf("%d of %d listed online, %d registered", online, len(b.Items), b.Total)
	if b.Truncated {
		line += " (list truncated)"
	}
	if !verbose {
		return line
	}
	for _, it := range b.Items {
		line += "\n    " + healthSummaryBrokerRow(it)
	}
	return line
}

func healthSummaryBrokerRow(b hubclient.HealthSummaryBroker) string {
	name := b.Name
	if name == "" {
		name = b.ID
	}
	parts := []string{name, orDash(b.Status), "version " + orDash(b.Version)}
	if b.Runtime != nil {
		parts = append(parts, "runtime "+orDash(b.Runtime.Type)+"/"+orDash(b.Runtime.Profile))
	}
	if ws := b.WorkspaceStorage; ws != nil {
		storage := "storage " + orDash(ws.Backend)
		if ws.NFSHealthy != nil {
			if *ws.NFSHealthy {
				storage += " (healthy)"
			} else {
				storage += " (unhealthy)"
			}
		}
		parts = append(parts, storage)
	}
	if b.Health != nil {
		self := "self-health " + orDash(b.Health.Status)
		if b.Status != "online" {
			// The last report before the broker stopped being online.
			self = "last reported " + self
		}
		if failing := nonHealthyCheckList(b.Health.Checks); failing != "" {
			self += " (" + failing + ")"
		}
		parts = append(parts, self)
	} else {
		parts = append(parts, "self-health not reported")
	}
	parts = append(parts, fmt.Sprintf("agents running %d, attention %d", b.Agents.Running, b.Agents.Attention))
	if b.LastHeartbeat != nil {
		parts = append(parts, "last heartbeat "+clitime.Format(*b.LastHeartbeat, clitime.Full))
	} else {
		parts = append(parts, "no heartbeat")
	}
	return strings.Join(parts, "; ")
}

func healthSummaryAgentsLine(s *hubclient.HealthSummary, verbose bool) string {
	a := s.Agents
	if a == nil {
		return "Agent data not available"
	}
	line := fmt.Sprintf("%d active of %d; %d in error or crashed of %d not stopped", a.Active, a.Total, a.Errored, a.Considered)
	for _, g := range a.Problems {
		line += fmt.Sprintf("; %s %d", g.Kind, g.Count)
	}
	if verbose && len(a.ByPhase) > 0 {
		phases := make([]string, 0, len(a.ByPhase))
		for _, p := range a.ByPhase {
			phases = append(phases, fmt.Sprintf("%s %d", p.Phase, p.Count))
		}
		line += "\n    by phase: " + strings.Join(phases, ", ")
	}
	return line
}

func healthSummaryDispatchLine(s *hubclient.HealthSummary) string {
	d := s.Dispatch
	if d == nil {
		return "Dispatch data not available"
	}
	return fmt.Sprintf("%d stuck message(s), %d stuck broker dispatch(es), %d failed broker dispatch(es) in the last hour",
		d.StuckMessages, d.StuckBrokerDispatch, d.FailedBrokerDispatch1h)
}

func healthSummaryIntegrationsLine(s *hubclient.HealthSummary, verbose bool) string {
	c := s.IntegrationCounts
	line := fmt.Sprintf("%d total: %d healthy, %d degraded, %d unhealthy, %d unknown", c.Total, c.Healthy, c.Degraded, c.Unhealthy, c.Unknown)
	if !verbose || c.Total == 0 {
		return line
	}
	if !s.IntegrationsDetail {
		return line + "\n    (integration names need hub.integrations.read)"
	}
	for _, it := range s.Integrations {
		row := fmt.Sprintf("%s (%s): %s", it.Name, orDash(it.Platform), orDash(it.Health))
		if it.Connected {
			row += ", connected"
		} else {
			row += ", not connected"
		}
		if it.Version != "" {
			row += ", version " + it.Version
		}
		if it.Reason != "" {
			row += ", " + it.Reason
		}
		line += "\n    " + row
	}
	return line
}

// healthSummaryTestIdentityLine returns the test identity posture line,
// and false when no test identity setting is on.
func healthSummaryTestIdentityLine(h hubclient.HealthSummaryHub) (string, bool) {
	var on []string
	if h.TestIdentitiesEnabled {
		on = append(on, "test identities")
	}
	if h.TestAdminIdentitiesEnabled {
		on = append(on, "test admin identities")
	}
	if h.TestSuperAdminIdentitiesEnabled {
		on = append(on, "test super-admin identities")
	}
	if h.TestIdentityWebSessionsEnabled {
		on = append(on, "test identity web sessions")
	}
	if len(on) == 0 {
		return "", false
	}
	line := "Enabled on this Hub: " + strings.Join(on, ", ")
	if h.TestMode != "" && h.TestMode != "off" {
		line = fmt.Sprintf("Enabled on this Hub (--test-mode=%s): %s", h.TestMode, strings.Join(on, ", "))
	}
	if h.LiveTestAdminIdentities != nil {
		line += fmt.Sprintf(" (%d live admin test identities)", *h.LiveTestAdminIdentities)
	}
	return line, true
}

func healthSummaryInstancesLine(s *hubclient.HealthSummary) string {
	hi := s.HubInstances
	if hi == nil {
		return "Hub instance data not available"
	}
	line := fmt.Sprintf("%d listed, %d live", hi.Total, hi.Live)
	if hi.Truncated {
		line += " (list truncated)"
	}
	for _, it := range hi.Items {
		label := orDash(it.Label)
		if it.Serving {
			label += " (serving)"
		}
		parts := []string{label, it.State, orDash(it.Status), "version " + orDash(it.Version)}
		if it.State == "live" && !it.StartedAt.IsZero() && !hi.AsOf.IsZero() {
			parts = append(parts, "up "+hi.AsOf.Sub(it.StartedAt).Round(time.Second).String())
		}
		if failing := nonHealthyCheckList(it.Checks); failing != "" {
			parts = append(parts, "checks "+failing)
		}
		if db := it.Database; db != nil {
			parts = append(parts, fmt.Sprintf("db pool %d active, %d idle, max %d", db.PoolActive, db.PoolIdle, db.PoolMax))
		}
		line += "\n    " + strings.Join(parts, "; ")
	}
	return line
}

// nonHealthyCheckList lists "name: value" for every check that did not
// pass, sorted by name. A check passes when its value is healthy or
// available (a broker reports its runtime as available), as on the web
// health dashboard.
func nonHealthyCheckList(checks map[string]string) string {
	var out []string
	for name, value := range checks {
		if value != healthSummaryHealthy && value != "available" {
			out = append(out, name+": "+value)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
