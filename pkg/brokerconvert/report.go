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

package brokerconvert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// The report of --apply, --status and --rollback. Like the plan, it holds
// keys, names, IDs, states and fixed texts only: no report type has a
// field that can carry a settings value, and Hub or OS error strings are
// never copied into it (sec R2).

// Report modes.
const (
	ModeApply    = "apply"
	ModeStatus   = "status"
	ModeRollback = "rollback"
)

// Refusal codes. Each has a fixed text (RefusalText).
const (
	RefuseLockHeld           = "lock_held"
	RefusePlanNotClean       = "plan_not_clean"
	RefuseHubSections        = "hub_sections_missing"
	RefuseMultipleInstances  = "multiple_instances"
	RefuseCapacitySplit      = "capacity_split"
	RefuseCapacityUnknown    = "capacity_unknown"
	RefuseCapacityDecision   = "capacity_decision_required"
	RefuseCapacityPermission = "capacity_permission"
	RefuseCapacityDrift      = "capacity_changed"
	RefuseAlreadyFlat        = "already_flat"
	RefusePlanChanged        = "plan_changed"
	RefuseOtherLegacy        = "other_legacy_broker"
	RefuseRollingBack        = "rollback_in_progress"
	RefuseNotDrained         = "not_drained"
	RefuseServerRunning      = "server_running"
	RefuseNameConflict       = "name_conflict"
	RefuseLocalPath          = "local_path_link"
	RefuseHubRequest         = "hub_request_failed"
	RefuseRegister           = "register_failed"
	RefuseSettingsWrite      = "settings_write_failed"
	RefuseSettingsChanged    = "settings_changed_since_apply"
	RefuseLocalWrite         = "local_write_failed"
	RefuseNoJournal          = "no_journal"
	RefuseNothingToRollBack  = "nothing_to_roll_back"
	RefuseDefaultUnknown     = "default_instance_unknown"
	RefuseInstanceDrained    = "instance_not_drained"
	RefuseIdentityUnreadable = "identity_unreadable"

	// The co-located path (design section 4.8 and Q7).
	RefuseColocatedUnknown    = "colocated_mode_unknown"
	RefuseHostModeConflict    = "host_mode_conflict"
	RefuseColocatedServerDown = "colocated_server_down"
	RefuseColocatedJournal    = "colocated_journal_state"
	RefuseStoreReplica        = "store_replica_setup"
	RefuseStoreUnknown        = "store_topology_unknown"
	RefuseStoreUnreadable     = "store_unreadable"
	RefuseSingleHubMisplaced  = "single_hub_replica_misplaced"
	RefuseSingleHubRequired   = "single_hub_replica_required"
	RefuseHubEndpointAnswers  = "hub_endpoint_answers"
	RefuseHubEndpointUnprobed = "hub_endpoint_unprobeable"
	RefuseStopRecheckMissing  = "stop_recheck_missing"
	RefuseStopRecheckExpired  = "stop_recheck_expired"
	RefuseCredentialsCleanup  = "credentials_cleanup_failed"
)

var refusalTexts = map[string]string{
	RefuseLockHeld:           HeldByServerStartText,
	RefusePlanNotClean:       "the plan has blocking findings or needs an operator decision; review 'convert --plan' and resolve them first",
	RefuseHubSections:        "the plan needs this host's Runtime Broker row and the Hub sections; check the Hub login and the registration",
	RefuseMultipleInstances:  "this release applies a plan with exactly one instance; a plan with more instances is not applied yet",
	RefuseCapacitySplit:      "the plan's per-instance agent limits do not match the planned instances or exceed the legacy Runtime Broker's limit",
	RefuseCapacityDecision:   "the legacy Runtime Broker has no agent limit of its own (it inherits the Hub's), so the instance's limit needs an operator decision; set the legacy Runtime Broker's maxAgents first",
	RefuseCapacityPermission: "writing the instance's agent limit needs quota.update on the Hub; ask a Hub admin to run the apply, or to grant it",
	RefuseCapacityDrift:      "the instance's agent limit on the Hub is not the one apply carries over from the legacy Runtime Broker; it was changed since; set it back, or roll back",
	RefuseCapacityUnknown:    "the legacy Runtime Broker's agent limit could not be read from the Hub, so the instance's limit cannot be planned; check that its settings are visible to you",
	RefuseAlreadyFlat:        "server.broker.instances is already set and no apply of this plan is in progress; there is nothing to convert",
	RefusePlanChanged:        "the plan changed since apply started; finish or roll back first",
	RefuseOtherLegacy:        "the journal is for another legacy Runtime Broker ID; finish or roll back that run first",
	RefuseRollingBack:        "a rollback is in progress; finish it with 'convert --rollback' first",
	RefuseNotDrained:         "non-terminal agents are bound to the legacy Runtime Broker; finish or delete them, then run again",
	RefuseServerRunning:      "the server is running on this host (or the legacy Runtime Broker is online with a recent heartbeat); stop it with 'scion server stop', then run again",
	RefuseNameConflict:       "a Runtime Broker with a planned instance's name already exists on the Hub; rename the instance in the reviewed choices, then run again",
	RefuseLocalPath:          "a project links the legacy Runtime Broker through a local path, which a flat Runtime Broker cannot take over; relink it without a path first",
	RefuseHubRequest:         "a Hub request failed; nothing after it was done; run again to resume",
	RefuseRegister:           "registering the instance failed (for example, the Hub's flat Runtime Broker experiment is off, or a name conflict); settings are untouched; fix the cause and run again, or roll back",
	RefuseSettingsWrite:      "writing server.broker.instances failed; the settings file is unchanged; fix the cause and run again",
	RefuseSettingsChanged:    "server.broker.instances changed since apply wrote it; restore what apply wrote, or edit the settings file by hand",
	RefuseLocalWrite:         "a local convert file could not be written; fix the cause and run again",
	RefuseNoJournal:          "no convert run is recorded on this host",
	RefuseNothingToRollBack:  "the recorded convert run is already rolled back",
	RefuseDefaultUnknown:     "a project's default instance cannot be chosen from the plan",
	RefuseInstanceDrained:    "non-terminal agents are bound to a flat instance; finish or delete them, then run the rollback again",
	RefuseIdentityUnreadable: "an instance identity file could not be read; it must be an owner-only regular file holding valid JSON; fix it, then run the rollback again",

	RefuseColocatedUnknown:    "whether this host's server runs the Hub with an embedded Runtime Broker cannot be determined: no saved server or broker launch was found (or it, or the server settings, could not be read); either assert the mode with --host-mode colocated|remote, or start the server ('scion server start') or the Runtime Broker ('scion runtime-broker start') once through the daemon, then run the command again",
	RefuseHostModeConflict:    "the host mode is fixed by the first apply and is never overridden, and the sources disagree; run with the journaled mode, or roll back first",
	RefuseColocatedServerDown: "this step needs this host's Hub: start the server ('scion server start') and run the command again",
	RefuseColocatedJournal:    "the convert journal is in a state this command cannot continue from; see 'convert --status'",
	RefuseStoreReplica:        "this Hub's settings configure more than one Hub process for its store (an HA or replica setup), so another Hub could accept agents while this server is stopped; a co-located conversion is refused (--single-hub-replica does not override this)",
	RefuseStoreUnknown:        "whether more than one Hub process serves this Hub's store cannot be determined from its settings; a co-located conversion is refused",
	RefuseStoreUnreadable:     "the Hub's SQLite store could not be opened or read read-only; a co-located conversion is refused",
	RefuseSingleHubMisplaced:  "--single-hub-replica is accepted only on the stopped phase of a co-located '--apply' whose Hub uses an external database",
	RefuseSingleHubRequired:   "this Hub uses an external database, so this command cannot see whether another Hub process serves it; if exactly one Hub process serves it, say so with --single-hub-replica",
	RefuseHubEndpointAnswers:  "the Hub endpoint still answers with this server stopped, so another Hub process serves this store; a co-located conversion is refused",
	RefuseHubEndpointUnprobed: "the Hub endpoint could not be probed (none configured, or its address does not resolve); a co-located conversion is refused",
	RefuseStopRecheckMissing:  "the journal has no record of the drained re-check before the stop; start the server and run 'convert --apply' again to repeat the running phase",
	RefuseStopRecheckExpired:  "the server was stopped more than 30 minutes after the drained re-check; start the server and run 'convert --apply' again to repeat the running phase",
	RefuseCredentialsCleanup:  "an instance credentials file could not be removed; nothing past it was removed; fix the cause and run again",
}

// RefusalText returns the fixed text for a refusal code.
func RefusalText(code string) string {
	if t, ok := refusalTexts[code]; ok {
		return t
	}
	return "the operation was refused"
}

// Fixed notes.
const (
	NoteEvidenceKept = "A copy of the settings file as it was before apply is kept in the convert directory, owner-only. It holds the full settings, including harness env values: do not share it."
	NoteSwitch       = "Apply is complete. Start the server ('scion server start'): it then hosts the flat instance(s) instead of the legacy Runtime Broker. Check with 'scion runtime-broker convert --status'."
	NoteNoop         = "Nothing to do: this plan is already applied."
	NoteRolledBack   = "Rollback is complete. The next 'scion server start' serves the legacy Runtime Broker again. The flat identity directories were moved aside, not deleted."
	NoteLegacyLinks  = "The legacy Runtime Broker stays linked to its projects (offline) until it is retired; nothing is re-created on a rollback."
	// NoteNoIdentity is followed by an instance key: its registration
	// started but recorded no Runtime Broker ID, and its identity file
	// holds none, so no row could be named for deregistration.
	NoteNoIdentity = "No Runtime Broker ID is recorded for an instance whose registration started, and its identity file holds none, so no row was deregistered for it. Check the Hub for a row of that name. Instance key:"

	// NoteColocatedAwaitingStop is the outage text of a co-located apply's
	// running phase. It is fixed, and a test holds it verbatim.
	NoteColocatedAwaitingStop = "The legacy Runtime Broker is drained and the flat instance is registered. " +
		"This host runs the Hub and its Runtime Broker in the same server process, so stopping the server takes the whole Hub down: " +
		"every user, Runtime Broker and agent connected to this Hub loses it until the server is started again. " +
		"Plan a maintenance window, then stop the server ('scion server stop') and run 'scion runtime-broker convert --apply' again."
	// NoteColocatedRollbackAwaitingStop is the outage text of a co-located
	// rollback's running phase.
	NoteColocatedRollbackAwaitingStop = "The flat instance row and its links are removed. " +
		"This host runs the Hub and its Runtime Broker in the same server process, so stopping the server takes the whole Hub down: " +
		"every user, Runtime Broker and agent connected to this Hub loses it until the server is started again. " +
		"Plan a maintenance window, then stop the server ('scion server stop') and run 'scion runtime-broker convert --rollback' again."

	NoteColocatedStart          = "The settings now name the flat instance. Start the server ('scion server start'): it serves the flat instance under its registered ID. Then run 'scion runtime-broker convert --apply' again to remove the unused instance credentials file."
	NoteColocatedCleanupWaiting = "The flat instance has not reported online yet; its unused credentials file is kept until it does. Run 'scion runtime-broker convert --apply' again once it shows online in 'convert --status'."
	NoteColocatedDone           = "The conversion is complete: the flat instance serves this host and no unused instance credentials file remains."
	NoteColocatedRollbackStart  = "The flat instance is removed from the settings and its directory is moved aside, without its unused credentials file. Start the server ('scion server start'): it serves the legacy Runtime Broker again."
)

// Refusal is a refused operation. It is an error whose text is fixed.
type Refusal struct {
	Code string `json:"code"`
	Text string `json:"text"`
	// HTTPStatus is the status of a failed Hub request, when there is one.
	HTTPStatus int `json:"httpStatus,omitempty"`
	// Agents are the non-terminal agents that block a drained check.
	Agents []AgentEntry `json:"agents,omitempty"`
	// Projects are project IDs a refusal names (a local path link).
	Projects []string `json:"projects,omitempty"`
	// Keys are instance keys a refusal names.
	Keys []string `json:"keys,omitempty"`
}

func (r *Refusal) Error() string {
	var b strings.Builder
	b.WriteString(r.Text)
	if r.HTTPStatus > 0 {
		fmt.Fprintf(&b, " (HTTP %d)", r.HTTPStatus)
	}
	if len(r.Keys) > 0 {
		fmt.Fprintf(&b, "; instance keys: %s", strings.Join(r.Keys, ", "))
	}
	if len(r.Projects) > 0 {
		fmt.Fprintf(&b, "; projects: %s", strings.Join(r.Projects, ", "))
	}
	for _, a := range r.Agents {
		fmt.Fprintf(&b, "; agent %s (%s) in project %s, phase %s", a.Name, a.ID, a.Project, a.Phase)
	}
	return b.String()
}

func refuse(code string) *Refusal { return &Refusal{Code: code, Text: RefusalText(code)} }

// refuseHub is a Hub request refusal carrying the HTTP status only.
func refuseHub(code string, err error) *Refusal {
	r := refuse(code)
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode > 0 {
		r.HTTPStatus = apiErr.StatusCode
	}
	return r
}

// fixedError is the journal's error text for a failure: the refusal's
// fixed text and, for a Hub request, its status.
func fixedError(r *Refusal) string {
	if r.HTTPStatus > 0 {
		return fmt.Sprintf("%s (HTTP %d)", r.Text, r.HTTPStatus)
	}
	return r.Text
}

// InstanceLine is one instance in a report.
type InstanceLine struct {
	Key             string    `json:"key"`
	Name            string    `json:"name"`
	BrokerID        string    `json:"brokerId,omitempty"`
	RuntimeTargetID string    `json:"runtimeTargetId,omitempty"`
	State           StepState `json:"state"`
	// Online is the Hub's view of the instance row (--status only).
	Online *bool `json:"online,omitempty"`
}

// ProjectLine is one project in a report.
type ProjectLine struct {
	ProjectID  string    `json:"projectId"`
	Linked     []string  `json:"linked"`
	OldDefault string    `json:"oldDefault,omitempty"`
	NewDefault string    `json:"newDefault,omitempty"`
	State      StepState `json:"state"`
}

// Report is the outcome of --apply, --status or --rollback.
type Report struct {
	Mode           string         `json:"mode"`
	RunID          string         `json:"runId,omitempty"`
	PlanDigest     string         `json:"planDigest,omitempty"`
	LegacyBrokerID string         `json:"legacyBrokerId,omitempty"`
	State          JournalState   `json:"state,omitempty"`
	HostMode       string         `json:"hostMode,omitempty"`
	Instances      []InstanceLine `json:"instances"`
	Settings       StepState      `json:"settings,omitempty"`
	Projects       []ProjectLine  `json:"projects"`
	Switch         StepState      `json:"switch,omitempty"`
	// Switched is true once every instance row reports online (--status).
	Switched bool     `json:"switched,omitempty"`
	Notes    []string `json:"notes"`
	Refusal  *Refusal `json:"refusal,omitempty"`
}

// reportFrom fills a report from the journal (states and IDs only).
func reportFrom(mode string, j *Journal) *Report {
	r := &Report{Mode: mode, Instances: []InstanceLine{}, Projects: []ProjectLine{}, Notes: []string{}}
	if j == nil {
		return r
	}
	r.RunID, r.PlanDigest, r.LegacyBrokerID, r.State = j.RunID, j.PlanDigest, j.LegacyBrokerID, j.State
	r.HostMode = j.HostMode
	for _, it := range j.Register {
		r.Instances = append(r.Instances, InstanceLine{Key: it.Key, Name: it.Name, BrokerID: it.BrokerID, RuntimeTargetID: it.RuntimeTargetID, State: it.State})
	}
	r.Settings = j.Settings.State
	for _, p := range j.Projects {
		r.Projects = append(r.Projects, ProjectLine{ProjectID: p.ProjectID, Linked: append([]string{}, p.Linked...), OldDefault: p.OldDefault, NewDefault: p.NewDefault, State: p.State})
	}
	r.Switch = j.Switch.State
	return r
}

// WriteReportJSON writes r as one indented JSON object.
func WriteReportJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteReportHuman writes r for a person.
func WriteReportHuman(w io.Writer, r *Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "convert --%s\n", r.Mode)
	if r.RunID != "" {
		fmt.Fprintf(&b, "Run %s, state %s\n", r.RunID, r.State)
		fmt.Fprintf(&b, "Legacy Runtime Broker: %s\n", r.LegacyBrokerID)
		fmt.Fprintf(&b, "Plan digest: %s\n", r.PlanDigest)
	}
	if len(r.Instances) > 0 {
		b.WriteString("Step 1, register instances:\n")
		for _, in := range r.Instances {
			fmt.Fprintf(&b, "  %s (%s): %s", in.Key, in.Name, in.State)
			if in.BrokerID != "" {
				fmt.Fprintf(&b, ", Runtime Broker %s", in.BrokerID)
			}
			if in.Online != nil {
				if *in.Online {
					b.WriteString(", online")
				} else {
					b.WriteString(", not online")
				}
			}
			b.WriteString("\n")
		}
	}
	if r.Settings != "" {
		fmt.Fprintf(&b, "Step 2, settings: %s\n", r.Settings)
	}
	if len(r.Projects) > 0 {
		b.WriteString("Step 3, projects:\n")
		for _, p := range r.Projects {
			fmt.Fprintf(&b, "  %s: %s, linked %s", p.ProjectID, p.State, strings.Join(p.Linked, ", "))
			if p.NewDefault != "" {
				fmt.Fprintf(&b, ", default %s -> %s", p.OldDefault, p.NewDefault)
			}
			b.WriteString("\n")
		}
	}
	if r.Switch != "" {
		fmt.Fprintf(&b, "Switch: %s", r.Switch)
		if r.Switched {
			b.WriteString(" (every instance has reported online)")
		}
		b.WriteString("\n")
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "%s\n", n)
	}
	if r.Refusal != nil {
		fmt.Fprintf(&b, "Refused: %s\n", r.Refusal.Error())
	}
	_, err := io.WriteString(w, b.String())
	return err
}
