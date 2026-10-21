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

package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// lookupVerdict is doctor's own verdict on the credential file's value,
// from its read-only agent lookup, classified like the credential check
// in sciontool init: accepted only for a 2xx that is not a redirect and
// whose body is this agent's record; refused only for a 401/403 the hub
// itself answered; unchecked otherwise.
type lookupVerdict int

const (
	lookupNotRun     lookupVerdict = iota // no lookup was made
	lookupAccepted                        // 2xx with this agent's record
	lookupHubRefused                      // 401/403 from the hub
	lookupUnchecked                       // anything else
)

// lookupRecordMaxBytes bounds the agent record doctor decodes.
const lookupRecordMaxBytes = 1 << 20

// classifyLookup returns the verdict for doctor's agent lookup response.
func classifyLookup(resp *http.Response, body []byte, agentID string) lookupVerdict {
	if resp == nil {
		return lookupUnchecked
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if classifyRejection(resp, body) == rejectedByHub {
			return lookupHubRefused
		}
		return lookupUnchecked
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || len(body) > lookupRecordMaxBytes {
		return lookupUnchecked
	}
	var record struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &record) != nil || agentID == "" || record.ID != agentID {
		return lookupUnchecked
	}
	return lookupAccepted
}

// initOutcome tells the later sections (GCP metadata, remediation) what
// the init credential check found.
type initOutcome int

const (
	// initOutcomeNone: nothing for the later sections to add.
	initOutcomeNone initOutcome = iota
	// initOutcomeHint: init's credential is expired or refused by the hub;
	// show how to reload it.
	initOutcomeHint
	// initOutcomeQuiet: a reload is in progress, a proxy refused init's
	// requests, or a refusal's source is not known; point at the init
	// lines and do not suggest reset-auth.
	initOutcomeQuiet
)

// initAdoptionBound is how long after the credential file is written init
// may still be switching to it: the file watch interval, the stability
// re-read, the hub check (up to 5s) and the reset, with margin.
const initAdoptionBound = 20 * time.Second

// initClockSkew is how far in the future a file time may be and still
// count as just written.
const initClockSkew = 2 * time.Second

// initStateStaleAfter is how old the state may be before doctor says so.
// Init updates it on every heartbeat (30s).
const initStateStaleAfter = 2 * time.Minute

// initAuthInput is everything the init credential check looks at.
type initAuthInput struct {
	stateStatus string // hub.CredentialState* read status
	stateReason string
	state       hub.CredentialState
	fileOK      bool // the credential file holds a value
	file        hub.CredentialIdentity
	fileAge     time.Duration // now minus the file's modification time
	lookup      lookupVerdict
	now         time.Time
}

// initAuthResult is the outcome of the init credential check.
type initAuthResult struct {
	lines    []string
	failures int
	outcome  initOutcome
	// proxy is true when a proxy refusal is among those found (outcome
	// quiet): remediation then points at the transport.
	proxy bool
	// window is true when the outcome is quiet because init is switching
	// to a newly written credential file.
	window bool
	// unclear is true when the outcome is quiet because init's credential
	// was refused but not known to be by the hub.
	unclear bool
	// initID and fileID name the two credentials for hints.
	initID, fileID string
	match          bool
}

// credentialName names a credential in doctor output: its jti, or a short
// fingerprint.
func credentialName(id hub.CredentialIdentity) string {
	if id.JTI != "" {
		return "jti=" + id.JTI
	}
	if id.FP != "" {
		return "sha256:" + id.FP
	}
	return "unknown"
}

// refusalSourceText words a set of refusal sources.
func refusalSourceText(hubSeen, proxySeen, unknownSeen bool) string {
	switch {
	case proxySeen && !hubSeen && !unknownSeen:
		return "the proxy"
	case hubSeen && !proxySeen && !unknownSeen:
		return "the hub"
	default:
		return "the hub or proxy"
	}
}

// describeRecord words one outcome record.
func describeRecord(r *hub.OutcomeRecord) string {
	by := ""
	switch r.RefusedBy {
	case hub.RefusedByHub:
		by = " by the hub"
	case hub.RefusedByProxy:
		by = " by the proxy"
	case hub.RefusedByUnknown:
		by = " by the hub or proxy"
	}
	switch r.Outcome {
	case hub.OutcomeSent:
		return fmt.Sprintf("sent (hub answered %d)", r.Status)
	case hub.OutcomeRefused:
		if r.Status == 0 {
			return "refused" + by
		}
		return fmt.Sprintf("refused (%d)%s", r.Status, by)
	case hub.OutcomePaused:
		return "paused after the credential was refused" + by
	case hub.OutcomeRefreshed:
		return "new credential received"
	case hub.OutcomeAccepted:
		return "accepted"
	case hub.OutcomeUnchecked:
		return "no answer from the hub"
	case hub.OutcomeFailed:
		if r.Status > 0 {
			return fmt.Sprintf("failed (status %d)", r.Status)
		}
		return "failed (no answer)"
	default:
		return "unknown"
	}
}

// isRefusal reports whether r records a refusal.
func isRefusal(r *hub.OutcomeRecord) bool {
	return r != nil && (r.Outcome == hub.OutcomeRefused || r.Outcome == hub.OutcomePaused)
}

// evaluateInitCredential decides what doctor reports about the credential
// sciontool init holds. It never prints a credential value.
//
// Rules, in order:
//  1. no trusted state: a WARN, nothing else.
//  2. what init holds and its last heartbeat, refresh and start-up check.
//  3. whether init holds the credential file's credential.
//  4. the verdict: expiry FAILs; else a refusal of init's current
//     credential is a WARN while init is switching to a credential file
//     written in the last 20s that the hub has not refused, and a FAIL
//     otherwise.
//  5. the outcome for later sections: a reload hint only for an expired
//     credential or a hub refusal, never while switching and never when a
//     proxy refused.
func evaluateInitCredential(in initAuthInput) initAuthResult {
	var res initAuthResult
	add := func(format string, args ...any) { res.lines = append(res.lines, fmt.Sprintf(format, args...)) }

	// Rule 1.
	switch in.stateStatus {
	case hub.CredentialStateOK:
	case hub.CredentialStateUntrusted:
		add("[WARN] Init credential state not trusted (%s); init's credential is not checked", in.stateReason)
		return res
	default:
		add("[WARN] Init credential state unavailable (init is not running as root, or could not publish it; see the init log)")
		return res
	}

	st := in.state
	cur := st.Credential
	res.initID = credentialName(cur)
	if in.fileOK {
		res.fileID = credentialName(in.file)
	}

	// Rule 2.
	expiry := "no expiry claim"
	if !cur.Expiry.IsZero() {
		expiry = fmtExpiry(cur.Expiry)
	}
	issued := ""
	if !cur.IssuedAt.IsZero() {
		issued = ", issued " + cur.IssuedAt.UTC().Format(time.RFC3339)
	}
	add("[INFO] Init credential: %s%s, %s", res.initID, issued, expiry)
	matched := func(r *hub.OutcomeRecord) bool { return r != nil && r.FP != "" && r.FP == cur.FP }
	for _, rec := range []struct {
		label string
		r     *hub.OutcomeRecord
	}{
		{"last heartbeat", st.Heartbeat},
		{"last refresh", st.Refresh},
		{"start-up check", st.Check},
	} {
		switch {
		case rec.r == nil:
			if rec.label != "start-up check" {
				add("[INFO] Init %s: none yet", rec.label)
			}
		case rec.r.Outcome == hub.OutcomeRefreshed:
			// Recorded for the credential presented, which the refresh
			// replaced.
			add("[INFO] Init %s: %s at %s", rec.label, describeRecord(rec.r), fmtWhen(rec.r.At))
		case !matched(rec.r):
			add("[INFO] Init %s: %s at %s (for a previous credential)", rec.label, describeRecord(rec.r), fmtWhen(rec.r.At))
		case rec.r.Outcome == hub.OutcomeFailed || rec.r.Outcome == hub.OutcomeUnchecked:
			add("[WARN] Init %s: %s at %s", rec.label, describeRecord(rec.r), fmtWhen(rec.r.At))
		default:
			add("[INFO] Init %s: %s at %s", rec.label, describeRecord(rec.r), fmtWhen(rec.r.At))
		}
	}
	if !st.UpdatedAt.IsZero() && in.now.Sub(st.UpdatedAt) > initStateStaleAfter {
		add("[WARN] Init credential state last updated %s ago (init may be busy or stopped)",
			in.now.Sub(st.UpdatedAt).Truncate(time.Second))
	}

	// Rule 3.
	res.match = in.fileOK && cur.FP != "" && in.file.FP == cur.FP && in.file.JTI == cur.JTI
	inWindowAge := in.fileAge < initAdoptionBound && in.fileAge > -initClockSkew
	window := in.fileOK && !res.match && inWindowAge && in.lookup != lookupHubRefused
	switch {
	case !in.fileOK:
		add("[WARN] Credential file missing; cannot compare it with init's credential")
	case res.match:
		add("[ OK ] Init credential matches the credential file")
		switch in.lookup {
		case lookupAccepted:
			add("[ OK ] Lookup with this credential accepted (read-only)")
		case lookupUnchecked:
			add("[WARN] Lookup with this credential not confirmed by the hub")
		}
	default:
		outdated := ""
		if in.fileAge >= initAdoptionBound && in.lookup == lookupAccepted {
			outdated = "; init holds an outdated credential"
		}
		add("[WARN] Init and the credential file hold different credentials (init: %s, file: %s)%s",
			res.initID, res.fileID, outdated)
	}

	// Rule 4: refusals of init's current credential. A refused start-up
	// check is superseded when the file holds the same credential and the
	// lookup with it is now accepted.
	var hubSeen, proxySeen, unknownSeen bool
	refused := false
	for _, r := range []*hub.OutcomeRecord{st.Heartbeat, st.Refresh, st.Check} {
		if !matched(r) || !isRefusal(r) {
			continue
		}
		if r == st.Check && res.match && in.lookup == lookupAccepted {
			add("[WARN] Init's start-up check of this credential was refused; a lookup with the same credential is now accepted")
			continue
		}
		refused = true
		switch r.RefusedBy {
		case hub.RefusedByHub:
			hubSeen = true
		case hub.RefusedByProxy:
			proxySeen = true
		default:
			unknownSeen = true
		}
	}
	expired := !cur.Expiry.IsZero() && !cur.Expiry.After(in.now)

	switch {
	case expired:
		add("[FAIL] Init credential expired at %s", fmtWhen(cur.Expiry))
		res.failures = 1
		if window {
			res.outcome, res.window = initOutcomeQuiet, true
		} else {
			res.outcome = initOutcomeHint
		}
	case refused && window:
		add("[WARN] Init is switching to the new credential file (written %s ago); run doctor again in about 20s",
			max(in.fileAge, 0).Truncate(time.Second))
		res.outcome, res.window = initOutcomeQuiet, true
	case refused:
		why := ""
		if in.fileOK && !res.match {
			why = ": the init process holds an outdated credential"
		}
		add("[FAIL] Init's requests with its credential are refused (401/403) by %s%s",
			refusalSourceText(hubSeen, proxySeen, unknownSeen), why)
		res.failures = 1
		switch {
		case proxySeen:
			res.outcome, res.proxy = initOutcomeQuiet, true
		case hubSeen || (unknownSeen && in.lookup == lookupHubRefused):
			res.outcome = initOutcomeHint
		default:
			// Refused, but not known to be by the hub: no reload advice.
			res.outcome, res.unclear = initOutcomeQuiet, true
		}
	case res.match && in.lookup == lookupHubRefused:
		// Counted by the agent lookup line in the Authentication section.
		add("[FAIL] Init holds the credential file's credential, which the hub refused (see Authentication above)")
	default:
		add("[ OK ] No refusal recorded for init's credential")
	}
	return res
}

// checkInitCredential reads the published state and the credential file,
// prints the init credential lines and returns the result.
// The credential file is the one read for the Authentication section
// (diag.file), so the lookup verdict and the file compared are always the
// same value.
func checkInitCredential(diag *doctorDiag) initAuthResult {
	fmt.Println("\n--- Init Process Credential ---")
	in := initAuthInput{lookup: diag.lookup, now: time.Now()}
	in.state, in.stateStatus, in.stateReason = hub.ReadCredentialState()
	if diag.file.ok {
		in.fileOK = true
		in.file = hub.CredentialIdentityOf(diag.file.token)
		in.fileAge = in.now.Sub(diag.file.modTime)
	} else {
		in.lookup = lookupNotRun
	}
	res := evaluateInitCredential(in)
	for _, l := range res.lines {
		fmt.Println(l)
	}
	return res
}

// printInitReloadHint prints how init's credential is reloaded.
func printInitReloadHint() {
	fmt.Println("[!] Init reloads the credential file by itself once the hub accepts the file's credential.")
	fmt.Println("[!] If this persists, run from the host:  scion reset-auth <agent-name>  (writes a new credential and tells init to reload it)")
	fmt.Println("[!] Or restart the agent from the host:  scion stop <agent-name>  then  scion start <agent-name>")
}

// printInitRemediation prints the remediation for the init credential
// outcome. It returns true if it printed anything.
func printInitRemediation(r initAuthResult) bool {
	switch {
	case r.outcome == initOutcomeHint:
		fmt.Println("[!] The init process's credential is expired or refused, so its own requests to the hub (status, heartbeats, GCP credentials) fail.")
		printInitReloadHint()
	case r.outcome == initOutcomeQuiet && r.window:
		fmt.Println("[!] Init is switching to the new credential file. Run doctor again in about 20s.")
	case r.outcome == initOutcomeQuiet && r.unclear:
		fmt.Println("[!] Init's credential was refused (401/403), but not clearly by the hub. See Init Process Credential above.")
		fmt.Println("[!] Run doctor again; if this persists, check the transport settings above and the hub's logs.")
	case r.outcome == initOutcomeQuiet && r.proxy:
		fmt.Println("[!] Init's requests are refused by the platform proxy (IAP / Cloud Run invoker), so its credential does not reach the hub.")
		fmt.Println("[!] Check the transport credential above, that SCION_TRANSPORT_MODE and the audience match the proxy, " +
			"and the hub's transport minter configuration and logs.")
	default:
		return false
	}
	return true
}

// printGCPInitHint prints the hint for a failed GCP token request,
// following the init credential outcome.
func printGCPInitHint(r initAuthResult) {
	fmt.Println("[!] gcloud auth print-access-token will fail in this state")
	switch r.outcome {
	case initOutcomeHint:
		if r.fileID != "" && !r.match {
			fmt.Printf("[!] The init process holds an outdated credential (%s; the credential file has %s), so it cannot get GCP credentials from the hub.\n",
				r.initID, r.fileID)
		} else {
			fmt.Printf("[!] The init process's credential (%s) is expired or refused, so it cannot get GCP credentials from the hub.\n", r.initID)
		}
		printInitReloadHint()
	case initOutcomeQuiet:
		fmt.Println("[!] See Init Process Credential above.")
	default:
		fmt.Println("[!] Run from the host:  scion reset-auth <agent-name>")
	}
}

// agentIDForInitCheck returns the agent ID; the init credential check runs
// only when it is set.
func agentIDForInitCheck() string {
	return strings.TrimSpace(os.Getenv("SCION_AGENT_ID"))
}
