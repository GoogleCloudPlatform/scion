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

package telemetrycontract

// TrajectoryNoticeVersion is the canonical version identifier stamped on
// run_attribution.notice_version when Track 2 trajectory capture is effective
// for a run (experiment tokenomics.trajectory_capture enabled and effective
// policy is not "off"). Bump whenever the user-visible notice semantics change.
const TrajectoryNoticeVersion = "v1"

// IsTrajectoryCaptureEffective reports whether policy results in trajectory
// data or metadata being captured for a run ("full" or "metadata_only").
// Returns false for "off" and unknown/empty values.
func IsTrajectoryCaptureEffective(policy string) bool {
	switch policy {
	case TrajectoryCapturePolicyFull, TrajectoryCapturePolicyMetadataOnly:
		return true
	default:
		return false
	}
}

// TrajectoryCaptureNotice returns the canonical user-visible notice line for
// status, shared across the CLI, Hub, web UI, and sciontool. Returns "" when
// status.Policy is empty or unrecognized.
func TrajectoryCaptureNotice(status TrajectoryCapturePolicyStatus) string {
	switch status.Policy {
	case TrajectoryCapturePolicyFull:
		if status.EnforcedByHub {
			return "Session transcripts for this agent are captured verbatim to the Hub audit trail (policy: full, enforced by Hub). Access is restricted and logged."
		}
		return "Session transcripts for this agent are captured verbatim to the Hub audit trail (policy: full). Access is restricted and logged."
	case TrajectoryCapturePolicyMetadataOnly:
		if status.EnforcedByHub {
			return "Session metadata for this agent is recorded to the Hub audit trail without raw transcripts (policy: metadata_only, enforced by Hub)."
		}
		return "Session metadata for this agent is recorded to the Hub audit trail without raw transcripts (policy: metadata_only)."
	case TrajectoryCapturePolicyOff:
		if status.EnforcedByHub {
			return "Trajectory capture is off for this agent (policy: off, enforced by Hub)."
		}
		return "Trajectory capture is off for this agent (policy: off)."
	default:
		return ""
	}
}
