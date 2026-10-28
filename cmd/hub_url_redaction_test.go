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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Hub URLs are shown without their user info.

// hubURLRedactionSecrets are the placeholder user info values the tests
// put in hub URLs; none of them appears in any output.
var hubURLRedactionSecrets = []string{"s3cret", "tok3n"}

func assertNoHubURLSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range hubURLRedactionSecrets {
		assert.NotContains(t, out, s)
	}
}

// errHubURLRedaction reads like a net/http error wrapped by a hub error.
var errHubURLRedaction = errors.New(`hub at https://alice:s3cret@hub.example.com is not responding: Get "https://tok3n@hub.example.com/api/v1/agents": connection refused`)

func TestFormatCLIError_RedactsHubURLUserInfo(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	out := formatCLIError(f, errHubURLRedaction)
	assertNoHubURLSecrets(t, out)
	assert.Contains(t, out, "hub at https://alice:xxxxx@hub.example.com is not responding")
	assert.Contains(t, out, `Get "https://xxxxx@hub.example.com/api/v1/agents"`)
}

func TestReportPlainError_RedactsHubURLUserInfo(t *testing.T) {
	var buf bytes.Buffer
	reportPlainError(&buf, errHubURLRedaction)
	assertNoHubURLSecrets(t, buf.String())
	assert.Contains(t, buf.String(), "https://xxxxx@hub.example.com/api/v1/agents")
}

func TestReportSSHError_RedactsHubURLUserInfo(t *testing.T) {
	var buf bytes.Buffer
	reportSSHError(&buf, errHubURLRedaction)
	assertNoHubURLSecrets(t, buf.String())
	assert.Contains(t, buf.String(), "https://xxxxx@hub.example.com/api/v1/agents")
}

// sshOneLine formats the errors scion ssh and ssh-config print themselves.
func TestSSHOneLine_RedactsHubURLUserInfo(t *testing.T) {
	out := sshOneLine(errHubURLRedaction.Error())
	assertNoHubURLSecrets(t, out)
	assert.Contains(t, out, "https://alice:xxxxx@hub.example.com")
}

func TestPrintUsingHub_RedactsUserInfo(t *testing.T) {
	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })
	outputFormat = ""
	out := captureStderr(t, func() { PrintUsingHub("https://tok3n@hub.example.com") })
	assertNoHubURLSecrets(t, out)
	assert.Equal(t, "Using hub: https://xxxxx@hub.example.com\n", out)
}

// notFoundHubURL starts a server that answers 404 to everything and
// returns its URL with the user info userinfo.
func notFoundHubURL(t *testing.T, userinfo string) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http://", "http://"+userinfo+"@", 1)
}

func TestHubAuthLogin_AuthenticatingLineRedacted(t *testing.T) {
	setupContextTest(t)
	origURL, origNoBrowser, origCtx := hubAuthHubURL, hubAuthNoBrowser, hubAuthLoginCmd.Context()
	t.Cleanup(func() {
		hubAuthHubURL, hubAuthNoBrowser = origURL, origNoBrowser
		hubAuthLoginCmd.SetOut(nil)
		hubAuthLoginCmd.SetErr(nil)
		hubAuthLoginCmd.SetContext(origCtx)
	})
	hubAuthHubURL = notFoundHubURL(t, "tok3n")
	hubAuthNoBrowser = true // the device flow: its first request gets a 404
	var out, errOut bytes.Buffer
	hubAuthLoginCmd.SetOut(&out)
	hubAuthLoginCmd.SetErr(&errOut)
	hubAuthLoginCmd.SetContext(context.Background())

	err := runHubAuthLogin(hubAuthLoginCmd, nil)
	require.Error(t, err, "the test hub has no login endpoints")
	assert.Contains(t, out.String(), "Authenticating with Hub at http://xxxxx@127.0.0.1:")
	assertNoHubURLSecrets(t, out.String()+errOut.String())
}

// The login offer of 'scion use <project>'.
func TestUseLoginToHub_AuthenticatingLineRedacted(t *testing.T) {
	setupContextTest(t)
	origNoBrowser := hubAuthNoBrowser
	t.Cleanup(func() { hubAuthNoBrowser = origNoBrowser })
	hubAuthNoBrowser = true
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetContext(context.Background())
	c.SetOut(&out)
	c.SetErr(&out)

	require.Error(t, useLoginToHub(c, notFoundHubURL(t, "tok3n")), "the test hub has no login endpoints")
	assert.Contains(t, out.String(), "Authenticating with Hub at http://xxxxx@127.0.0.1:")
	assertNoHubURLSecrets(t, out.String())
}

func TestDescribeHub_RedactsUserInfo(t *testing.T) {
	assert.Equal(t, "https://xxxxx@hub.example.com", describeHub("", "https://tok3n@hub.example.com"))
	assert.Equal(t, "'prod' (https://alice:xxxxx@hub.example.com)", describeHub("prod", "https://alice:s3cret@hub.example.com"))
}

// sshEndpointURL quotes the endpoint in its errors without user info.
func TestSSHEndpointURL_ErrorsRedactUserInfo(t *testing.T) {
	for _, ep := range []string{
		"https://alice s3cret@hub.example.com",
		"https://tok3n@hub.example.com",
		"ftp://tok3n@hub.example.com",
	} {
		_, err := sshEndpointURL(ep)
		require.Error(t, err, ep)
		assertNoHubURLSecrets(t, err.Error())
		assert.Contains(t, err.Error(), "xxxxx@hub.example.com", ep)
	}
}

// Doctor check messages, in text and JSON output, show URLs without user
// info; the hub connectivity messages name the endpoint the same way.
func TestDoctorReportAdd_RedactsURLUserInfo(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var buf bytes.Buffer
		r := &doctorReport{stdout: &buf, json: asJSON}
		r.add(&r.HubChecks, scionruntime.CheckResult{
			Name:        "hub-connectivity",
			Status:      "fail",
			Message:     "Cannot reach Hub: " + errHubURLRedaction.Error(),
			Remediation: "Check https://tok3n@hub.example.com",
		})
		require.Len(t, r.HubChecks, 1)
		got := r.HubChecks[0]
		assertNoHubURLSecrets(t, buf.String()+got.Message+got.Remediation)
		assert.Contains(t, got.Message, "https://xxxxx@hub.example.com/api/v1/agents")
		assert.Equal(t, "Check https://xxxxx@hub.example.com", got.Remediation)
	}
}

// An endpoint containing "@" is never shown by doctor, not even in part
// (see doctorDisplayEndpoint).
func TestDoctorHubHealthResult_RedactsEndpoint(t *testing.T) {
	const ep = "https://alice:s3cret@hub.example.com"
	healthy := doctorHubHealthResult(ep, healthProbeResponse{Status: probeStatusHealthy})
	assert.Equal(t, "Hub at "+doctorEndpointRedacted+" is healthy", healthy.Message)
	unhealthy := doctorHubHealthResult(ep, healthProbeResponse{Status: probeStatusUnhealthy})
	assert.Equal(t, "Hub at "+doctorEndpointRedacted+" is unhealthy", unhealthy.Message)
	assertNoHubURLSecrets(t, healthy.Message+unhealthy.Message)
}

// The connectivity check, with a hub that cannot be reached: an endpoint
// containing "@" is not shown, and the error quoting it is replaced by a
// fixed note (see doctorDisplayEndpoint and doctorHubError).
func TestDoctorHubConnectivity_UnreachableRedacted(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	ep := strings.Replace(srv.URL, "http://", "http://tok3n@", 1)
	srv.Close()
	res := checkDoctorHubConnectivity(ep, nil)
	require.Equal(t, "fail", res.Status)
	assert.Equal(t, "Cannot reach Hub at "+doctorEndpointRedacted+": "+doctorHubErrorHidden, res.Message)
	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf}
	r.add(&r.HubChecks, res)
	assertNoHubURLSecrets(t, buf.String()+r.HubChecks[0].Message)
}

// A hub URL with user info is never saved in the hub list: 'hub link' (and
// the link recorder of a command run against the current hub) records no
// entry for it, and the warning shows the URL without the user info.
func TestRecordLinkEntry_URLWithPasswordNotSaved(t *testing.T) {
	recordLinkEntryNotSaved(t, "https://user:s3cret@hub.example.com")
}

func TestRecordLinkEntry_URLWithUserNameNotSaved(t *testing.T) {
	recordLinkEntryNotSaved(t, "https://tok3n@hub.example.com")
}

// recordLinkEntryNotSaved records a link to ep and checks that nothing is
// saved. It does not call t.Helper, so a failure names its own line.
func recordLinkEntryNotSaved(t *testing.T, ep string) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")
	dotScion := makeLinkedDirProject(t, "")
	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)

	var out, errOut bytes.Buffer
	recordLinkEntry(&out, &errOut, ep, "id-1", "foo", "Foo", dotScion)
	assert.Empty(t, out.String(), ep)
	assert.Contains(t, errOut.String(), "Warning: could not record the link in the hub list: invalid hub URL", ep)
	assertNoHubURLSecrets(t, errOut.String())

	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "context.yaml is not written")
	assertNoHubURLSecrets(t, string(after))
	entry, _ := readTestContext(t).LinkEntryFor(config.LinkPathForProject(dotScion))
	assert.Nil(t, entry, ep)
}

// Every error sink masks the whole user info of each URL form.
func TestErrorSinks_UserInfoForms(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	sinks := map[string]func(error) string{
		"formatCLIError": func(e error) string { return formatCLIError(f, e) },
		"reportPlainError": func(e error) string {
			var b bytes.Buffer
			reportPlainError(&b, e)
			return b.String()
		},
		"reportSSHError": func(e error) string {
			var b bytes.Buffer
			reportSSHError(&b, e)
			return b.String()
		},
		"sshOneLine": func(e error) string { return sshOneLine(e.Error()) },
		"doctor": func(e error) string {
			var b bytes.Buffer
			r := &doctorReport{stdout: &b}
			r.add(&r.HubChecks, scionruntime.CheckResult{Name: "hub-connectivity", Status: "fail", Message: e.Error()})
			return b.String() + r.HubChecks[0].Message
		},
	}
	errs := map[string]error{
		"form 1": fmt.Errorf("invalid hub URL %q", `https://alice:s3"cret@hub.example.com`),
		"form 2": fmt.Errorf("invalid hub URL %+q", "https://tok3né@hub.example.com"),
		"form 3": errors.New("dial https://alice:s3cret@q@hub.example.com/api: refused"),
		"form 4": errors.New("hub at https://al'ice:s3cret@q@hub.example.com is not responding"),
		"form 5": errors.New("dial https://alice:s3cret@q@hub.example.com:port/api: refused"),
	}
	for sink, show := range sinks {
		for name, e := range errs {
			out := show(e)
			assertNoHubURLSecrets(t, out)
			assert.NotContains(t, out, "q@hub", "%s, %s: part of the password is shown: %q", sink, name, out)
			assert.Contains(t, out, "xxxxx@hub.example.com", "%s, %s: %q", sink, name, out)
		}
	}
}

// An error that names the --hub value is shown without its user info.
func TestErrorSinks_HubValueInError(t *testing.T) {
	// Built at run time from its parts: a URL that does not parse.
	password := "s3cr@tp4ss"
	_, perr := url.Parse("http://user:" + password + "@hub.example.com:" + "99999x")
	require.Error(t, perr)
	e := fmt.Errorf("invalid hub endpoint: %w", perr)
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	var plain, ssh bytes.Buffer
	reportPlainError(&plain, e)
	reportSSHError(&ssh, e)
	for sink, out := range map[string]string{
		"formatCLIError":   formatCLIError(f, e),
		"reportPlainError": plain.String(),
		"reportSSHError":   ssh.String(),
		"sshOneLine":       sshOneLine(e.Error()),
	} {
		assert.Contains(t, out, "http://xxxxx@hub.example.com:99999x", sink)
		for i := 0; i+4 <= len(password); i++ {
			assert.NotContains(t, out, password[i:i+4], "%s shows part of the password", sink)
		}
	}
}

// 'hub link --format json' shows the endpoint without user info.
func TestHubLinkJSONResult_RedactsEndpoint(t *testing.T) {
	r := hubLinkJSONResult("demo", "local-id", "hub-id", "https://user:s3cret@hub.example.com")
	assert.Equal(t, "https://user:xxxxx@hub.example.com", r.Details["endpoint"])
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assertNoHubURLSecrets(t, string(b))
}

// redactEndpointIn shows the user info of any URL in a text masked.
func TestRedactEndpointIn_UserInfoForms(t *testing.T) {
	got := redactEndpointIn("dial http://al'ice:pa55word@hub.example.com/x: refused", "http://other:x@127.0.0.1:99999x")
	assert.Equal(t, "dial http://xxxxx@hub.example.com/x: refused", got)
}

// An endpoint that is not a valid URL is named without its user info, and
// the error still wraps the parse error.
func TestSSHEndpointURL_InvalidURLShownWithoutUserInfo(t *testing.T) {
	_, err := sshEndpointURL("https://user:s3cret@hub.example.com:99999x")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cret")
	assert.Contains(t, err.Error(), "https://xxxxx@hub.example.com:99999x")
	var ue *url.Error
	assert.True(t, errors.As(err, &ue), "the parse error stays in the chain")
}

func TestRecordLinkEntry_InvalidURLWarningWithoutUserInfo(t *testing.T) {
	recordLinkEntryNotSaved(t, "https://user:s3cret@hub.example.com:99999x")
}

func TestRegisterHubInList_InvalidURLWarningWithoutUserInfo(t *testing.T) {
	setupContextTest(t)
	var out, errOut bytes.Buffer
	registerHubInList(&out, &errOut, "https://user:s3cret@hub.example.com:99999x")
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Warning: could not add https://xxxxx@hub.example.com:99999x to the hub list")
	assertNoHubURLSecrets(t, errOut.String())
}
