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

package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// effectiveHelpers returns the credential helpers git would use from env's
// GIT_CONFIG_COUNT entries, in order, for any URL: an empty
// credential.helper (any scope) clears the list collected so far, as git
// does when it reads matching credential.*.helper entries in config order.
// The global and system files are not read (the remote layer empties them).
func effectiveHelpers(t *testing.T, env []string) []string {
	t.Helper()
	count := 0
	if vals := envValues(env, "GIT_CONFIG_COUNT"); len(vals) > 0 {
		if len(vals) != 1 {
			t.Errorf("GIT_CONFIG_COUNT values = %q, want exactly one", vals)
		}
		n, err := strconv.Atoi(vals[len(vals)-1])
		if err != nil {
			t.Fatalf("GIT_CONFIG_COUNT = %q", vals[len(vals)-1])
		}
		count = n
	}
	var helpers []string
	for i := 0; i < count; i++ {
		keys := envValues(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		vals := envValues(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		if len(keys) != 1 || len(vals) != 1 {
			t.Fatalf("config entry %d: keys %q values %q, want one each", i, keys, vals)
		}
		k := strings.ToLower(keys[0])
		if !strings.HasPrefix(k, "credential.") || !strings.HasSuffix(k, ".helper") {
			continue
		}
		if vals[0] == "" {
			helpers = nil
			continue
		}
		helpers = append(helpers, keys[0]+"="+vals[0])
	}
	return helpers
}

// checkHostGitRemoteEnv fails t unless env is a remote host-side git
// environment: every remote-layer setting present exactly once with its
// value, every host authentication variable absent, LC_ALL=C kept, and no
// credential helper in effect (token "") or exactly the helper scoped to
// scopedKey carrying token.
func checkHostGitRemoteEnv(t *testing.T, env []string, token, scopedKey string) {
	t.Helper()
	want := map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"HOME":                os.DevNull,
		"XDG_CONFIG_HOME":     os.DevNull,
		"GIT_ASKPASS":         "",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_SSH_COMMAND":     hostGitAnonSSHCommand,
		"GIT_SSH_VARIANT":     "ssh",
		"KRB5CCNAME":          "FILE:" + os.DevNull,
		"KRB5_CLIENT_KTNAME":  "FILE:" + os.DevNull,
		"LC_ALL":              "C",
	}
	for k, v := range want {
		// Values are never printed: env may be the real process environment.
		if got := envValues(env, k); len(got) != 1 || got[0] != v {
			t.Errorf("%s: %d value(s), want exactly [the layer value] (values not printed)", k, len(got))
		}
	}
	for _, k := range []string{
		"GIT_CONFIG_SYSTEM", "SSH_ASKPASS", "GIT_SSH", "SSH_AUTH_SOCK", "GSS_USE_PROXY", "NETRC", "GITHUB_TOKEN",
		"GIT_SSL_CERT", "GIT_SSL_KEY", "GIT_SSL_CERT_PASSWORD_PROTECTED", "GIT_SSL_NO_VERIFY",
	} {
		if got := envValues(env, k); len(got) != 0 {
			t.Errorf("%s is present; want it absent (value not printed)", k)
		}
	}
	helpers := effectiveHelpers(t, env)
	cred := envValues(env, scopedGitCredentialEnv)
	if token == "" {
		if len(helpers) != 0 {
			t.Errorf("credential helpers in effect = %q, want none", helpers)
		}
		if len(cred) != 0 {
			t.Errorf("%s is set; want it absent without a token", scopedGitCredentialEnv)
		}
		return
	}
	if len(helpers) != 1 || helpers[0] != scopedKey+"="+scopedCredentialHelperText {
		t.Errorf("credential helpers in effect = %q, want exactly the one scoped to %s", helpers, scopedKey)
	}
	if len(cred) != 1 || cred[0] != token {
		t.Errorf("%s values: want exactly the token once", scopedGitCredentialEnv)
	}
}

// hostAuthEnv is an environment holding every host authentication
// variable the remote layer overrides or removes, plus trust and transport
// settings it keeps.
func hostAuthEnv() []string {
	return []string{
		"PATH=/bin",
		"HOME=/home/host",
		"XDG_CONFIG_HOME=/home/host/.config",
		"GIT_CONFIG_GLOBAL=/home/host/.gitconfig",
		"GIT_CONFIG_SYSTEM=/etc/gitconfig",
		"GIT_ASKPASS=/usr/bin/askpass",
		"SSH_ASKPASS=/usr/bin/ssh-askpass",
		"GIT_SSH=/usr/bin/host-ssh",
		"GIT_SSH_COMMAND=ssh -i /home/host/.ssh/id",
		"GIT_SSH_VARIANT=plink",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"KRB5CCNAME=FILE:/tmp/krb5cc_host",
		"KRB5_CLIENT_KTNAME=/etc/krb5.keytab",
		"GSS_USE_PROXY=yes",
		"GITHUB_TOKEN=host-github-token",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ALLOW_PROTOCOL=https:ssh:git",
		"GIT_SSL_CAINFO=/etc/ca.pem",
		"GIT_SSL_CAPATH=/etc/ca",
		"GIT_HTTP_USER_AGENT=ua",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_CEILING_DIRECTORIES=/c",
		"https_proxy=http://proxy:3128",
		"no_proxy=localhost",
		"LC_ALL=C",
	}
}

// Without a token the remote layer offers nothing: every override is set
// once, every host authentication variable is gone, no helper is in effect.
// Mutants this catches (each a deleted or changed line of the layer): any
// hostGitRemoteSet entry, any hostGitRemoteDropped entry, GIT_ASKPASS left
// out of the owned keys (two values), the NoCredentialHelperEnv step (the
// GIT_ASKPASS value missing).
func TestHostGitRemoteEnv_NoToken(t *testing.T) {
	env := HostGitRemoteEnv(hostAuthEnv(), "https://git.example.test/org/repo.git", "")
	checkHostGitRemoteEnv(t, env, "", "")
}

// With a token and an https URL exactly the scoped helper is in effect.
// Mutants this catches: dropping the ScopedCredentialHelperEnv step (no
// helper), dropping the NoCredentialHelperEnv step on the token path, and
// keeping an inherited scoped credential (two values).
func TestHostGitRemoteEnv_TokenHTTPS(t *testing.T) {
	base := append(hostAuthEnv(), scopedGitCredentialEnv+"=inherited")
	env := HostGitRemoteEnv(base, "https://git.example.test/org/repo.git", "tok-1")
	checkHostGitRemoteEnv(t, env, "tok-1", "credential.https://git.example.test.helper")
}

// A token with a non-http(s) URL is ignored: the command is anonymous, ssh
// included.
func TestHostGitRemoteEnv_TokenIgnoredForNonHTTP(t *testing.T) {
	for _, u := range []string{"ssh://git@git.example.test/org/repo.git", "git@git.example.test:org/repo.git", "git://git.example.test/r.git"} {
		t.Run(u, func(t *testing.T) {
			checkHostGitRemoteEnv(t, HostGitRemoteEnv(hostAuthEnv(), u, "tok-1"), "", "")
		})
	}
}

// Trust and transport settings, and the caller's own GIT_CONFIG_* entries,
// are kept, ahead of the layer's entries.
func TestHostGitRemoteEnv_KeepsTrustTransportAndCallerConfig(t *testing.T) {
	base := append(hostAuthEnv(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/ws")
	env := HostGitRemoteEnv(base, "https://git.example.test/org/repo.git", "")
	for _, kv := range []string{
		"PATH=/bin", "GIT_ALLOW_PROTOCOL=https:ssh:git", "GIT_SSL_CAINFO=/etc/ca.pem", "GIT_SSL_CAPATH=/etc/ca",
		"GIT_HTTP_USER_AGENT=ua", "GIT_LFS_SKIP_SMUDGE=1", "GIT_CEILING_DIRECTORIES=/c",
		"https_proxy=http://proxy:3128", "no_proxy=localhost",
		"GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/ws",
	} {
		k, v, _ := strings.Cut(kv, "=")
		if got := envValues(env, k); len(got) != 1 || got[0] != v {
			t.Errorf("%s values = %q, want it kept as [%q]", k, got, v)
		}
	}
	if got := envValues(env, "GIT_CONFIG_COUNT"); len(got) != 1 || got[0] != "4" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want [4] (the caller's entry plus the three resets)", got)
	}
	checkHostGitRemoteEnv(t, env, "", "")
}

// Applying the layer to its own result with the same arguments changes
// nothing, with and without a token and with caller config entries; with
// other arguments the earlier layer is replaced, not stacked.
// Mutants this catches: dropping the strip of the earlier layer (entries
// stack), ignoring the recorded start (the caller's entry is stripped or the
// earlier layer kept), dropping lastConfigCountOnly (two counts).
func TestHostGitRemoteEnv_Idempotent(t *testing.T) {
	withCaller := append(hostAuthEnv(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/ws")
	for _, tc := range []struct {
		name  string
		base  []string
		url   string
		token string
	}{
		{"anonymous", hostAuthEnv(), "https://git.example.test/r.git", ""},
		{"token", hostAuthEnv(), "https://git.example.test/r.git", "tok-1"},
		{"ssh", hostAuthEnv(), "ssh://git@git.example.test/r.git", "tok-1"},
		{"caller config, token", withCaller, "https://git.example.test/r.git", "tok-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			once := HostGitRemoteEnv(tc.base, tc.url, tc.token)
			twice := HostGitRemoteEnv(once, tc.url, tc.token)
			if !reflect.DeepEqual(once, twice) {
				t.Errorf("applying the layer again changed it; differing keys: %q", diffEnvSliceKeys(once, twice))
			}
		})
	}
	t.Run("replaced", func(t *testing.T) {
		tokenEnv := HostGitRemoteEnv(withCaller, "https://git.example.test/r.git", "tok-1")
		anon := HostGitRemoteEnv(tokenEnv, "https://git.example.test/r.git", "")
		if want := HostGitRemoteEnv(withCaller, "https://git.example.test/r.git", ""); !reflect.DeepEqual(anon, want) {
			t.Errorf("re-applying without the token: differing keys: %q", diffEnvSliceKeys(anon, want))
		}
		checkHostGitRemoteEnv(t, anon, "", "")
		if got := envValues(anon, "GIT_CONFIG_VALUE_0"); len(got) != 1 || got[0] != "/ws" {
			t.Errorf("the caller's entry is lost: %q", got)
		}
	})
}

// Every variable HostGitEnv keeps is either overridden or removed by the
// remote layer, or listed here as a trust or transport setting it keeps. A
// new kept name fails this test until it is classified.
func TestHostGitRemoteEnv_ClassifiesEveryKeptName(t *testing.T) {
	keptByLayer := map[string]string{
		"GIT_SSL_CAINFO":                  "trust: CA bundle",
		"GIT_SSL_CAPATH":                  "trust: CA directory",
		"GIT_ALLOW_PROTOCOL":              "transport allow-list",
		"GIT_HTTP_USER_AGENT":             "transport: user agent",
		"GIT_CEILING_DIRECTORIES":         "discovery bound",
		"GIT_TEST_ASSUME_DIFFERENT_OWNER": "stricter ownership checks (tests)",
	}
	overridden := map[string]bool{"GIT_ASKPASS": true}
	for _, s := range hostGitRemoteSet {
		overridden[s.key] = true
	}
	for _, k := range hostGitRemoteDropped {
		overridden[k] = true
	}
	for k := range hostGitEnvKept {
		_, kept := keptByLayer[k]
		if kept == overridden[k] {
			t.Errorf("%s: kept by HostGitEnv; classify it as overridden by the remote layer or as a trust/transport setting it keeps (exactly one)", k)
		}
	}
	for k := range keptByLayer {
		if !hostGitEnvKept[k] {
			t.Errorf("%s: classified as kept but HostGitEnv does not keep it; drop the entry", k)
		}
	}
}

// The anonymous ssh command reads no ssh config, offers no identity of any
// kind, reuses no connection, runs no proxy command, checks host keys
// strictly without writing them, and quotes the null device path.
// Mutant this catches: deleting any option from hostGitAnonSSHCommand.
func TestHostGitAnonSSHCommand(t *testing.T) {
	want := "ssh -F " + shellQuote(os.DevNull) +
		" -o BatchMode=yes -o IdentityAgent=none -o IdentitiesOnly=yes -o IdentityFile=none" +
		" -o PubkeyAuthentication=no -o GSSAPIAuthentication=no -o HostbasedAuthentication=no" +
		" -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no" +
		" -o ControlMaster=no -o ControlPath=none -o ProxyCommand=none -o ProxyJump=none" +
		" -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o ClearAllForwardings=yes -o PermitLocalCommand=no"
	if hostGitAnonSSHCommand != want {
		t.Errorf("hostGitAnonSSHCommand =\n%q\nwant\n%q", hostGitAnonSSHCommand, want)
	}
	for _, bad := range []string{"UserKnownHostsFile", "GlobalKnownHostsFile", "StrictHostKeyChecking=no", "accept-new"} {
		if strings.Contains(hostGitAnonSSHCommand, bad) {
			t.Errorf("hostGitAnonSSHCommand contains %q: known_hosts are read as trust data and checked strictly", bad)
		}
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote(a'b) = %s", got)
	}
}

// HostGitRemote never formats or logs its token or URL credentials.
func TestHostGitRemote_Redaction(t *testing.T) {
	const token = "tok-SECRET-123"
	r := NewHostGitRemote("https://user:pw-SECRET@git.example.test/org/repo.git?x=q-SECRET#f-SECRET", token)
	var forms []string
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		forms = append(forms, fmt.Sprintf(f, r))
	}
	forms = append(forms, fmt.Sprintf("%v", HostGitOptions{Network: true, Remote: r}))
	forms = append(forms, fmt.Sprintf("%+v", HostGitOptions{Network: true, Remote: r}))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "remote", r)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "remote", r)
	forms = append(forms, buf.String())
	for _, s := range forms {
		if strings.Contains(s, "SECRET") {
			t.Errorf("formatted remote leaks a credential: %s", s)
		}
	}
	if got, want := r.String(), "HostGitRemote{url:https://git.example.test/org/repo.git, token:set}"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := NewHostGitRemote("git@git.example.test:org/repo.git", "").String(); got != "HostGitRemote{url:[redacted], token:unset}" {
		t.Errorf("scp-style String() = %q", got)
	}
	var nilRemote *HostGitRemote
	if got := nilRemote.String(); got != "HostGitRemote(nil)" {
		t.Errorf("nil String() = %q", got)
	}
}

// A network command needs its remote, and a remote needs a network command;
// both refusals come before anything runs and name no path.
func TestHostGitCommand_RemoteSentinels(t *testing.T) {
	dir := t.TempDir()
	_, err := HostGitCommand(context.Background(), dir, HostGitOptions{Network: true}, "fetch")
	if !errors.Is(err, errHostGitRemoteRequired) {
		t.Errorf("Network without Remote: err = %v, want errHostGitRemoteRequired", err)
	}
	_, err = HostGitCommand(context.Background(), dir, HostGitOptions{Remote: NewHostGitRemote("https://h.example/r.git", "")}, "status")
	if !errors.Is(err, errHostGitRemoteWithoutNetwork) {
		t.Errorf("Remote without Network: err = %v, want errHostGitRemoteWithoutNetwork", err)
	}
	for _, e := range []error{errHostGitRemoteRequired, errHostGitRemoteWithoutNetwork} {
		if strings.Contains(e.Error(), dir) || strings.Contains(e.Error(), string(os.PathSeparator)) {
			t.Errorf("%q names a path", e)
		}
	}
}

// diffEnvSliceKeys returns the sorted keys whose presence, value or position differs between a and b, never a value: test
// failure messages report env differences by key, since an env may be the real process environment.
func diffEnvSliceKeys(a, b []string) []string {
	keys := map[string]bool{}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			for _, kv := range []string{x, y} {
				if k, _, ok := strings.Cut(kv, "="); ok {
					keys[k] = true
				}
			}
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
