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
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostlimits"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// brokerInotifyWarning returns the broker startup warning for low per-user
// inotify limits, or "" when there is nothing to say. hostUserAgents is
// brokerAgentsShareHostUser for this broker. A workstation server (hosted
// false) gets no warning: scion doctor reports the limits there. The check
// only reads; it is judged against the Hub's default per-broker capacity
// (hostlimits.DefaultExpectedAgents).
func brokerInotifyWarning(hosted, hostUserAgents bool, r hostlimits.Reader) string {
	if !hosted || !hostUserAgents {
		return ""
	}
	return hostlimits.CheckInotify(r, 0).Warning()
}

// brokerAgentsShareHostUser reports whether a broker's agents run on this
// host as the broker's user (SCION_HOST_UID, no user-namespace remap), so
// that their file watchers count against that user's inotify limits. A
// flat host (instances set) has such agents when any instance has a docker
// runtime target. A legacy broker has them unless its default runtime is
// Kubernetes or Cloud Run, whose agents run elsewhere; defaultRuntime may
// be "" when it is not known.
func brokerAgentsShareHostUser(instances []config.V1RuntimeBrokerInstanceConfig, defaultRuntime string) bool {
	if len(instances) > 0 {
		for _, inst := range instances {
			if inst.RuntimeTarget != nil && inst.RuntimeTarget.Type == config.RuntimeTargetTypeDocker {
				return true
			}
		}
		return false
	}
	return !runtimebroker.NFSWarnOnlyRuntime(defaultRuntime)
}

// checkDoctorInotifyLimitsHere runs checkDoctorInotifyLimits for this host:
// judged only on a host configured as a server (detectDoctorServerHost)
// whose broker's agents share the host user, read from the global
// settings (flat instances, else the default runtime).
func checkDoctorInotifyLimitsHere() scionruntime.CheckResult {
	notJudged := "not a broker host"
	if globalDir, err := config.GetGlobalDir(); err == nil && detectDoctorServerHost(globalDir).configured {
		instances, _ := config.LoadRuntimeBrokerInstances("")
		defaultRuntime := ""
		if vs, _, err := config.LoadGlobalSettings(); err == nil && vs != nil {
			if _, rtType, err := vs.ResolveRuntime(""); err == nil {
				defaultRuntime = rtType
			}
		}
		notJudged = ""
		if !brokerAgentsShareHostUser(instances, defaultRuntime) {
			notJudged = "this broker's agents do not run as containers on this host"
		}
	}
	return checkDoctorInotifyLimits(notJudged, hostlimits.DefaultReader(), os.Geteuid())
}

// checkDoctorInotifyLimits performs D12: the per-user inotify limits of
// this host and their approximate use by the processes of effective UID
// uid. The values are shown on every Linux host; they are judged (pass or
// warn) only when notJudged is "", otherwise the line is a skip that names
// the reason. Off Linux, or with /proc/sys/fs/inotify unreadable, it is a
// skip.
func checkDoctorInotifyLimits(notJudged string, r hostlimits.Reader, uid int) scionruntime.CheckResult {
	res := hostlimits.CheckInotify(r, 0)
	status, msg, remediation := res.Doctor()
	if res.Applicable {
		if u, ok := hostlimits.CountInotifyUsage(r.Proc, uid); ok {
			msg = fmt.Sprintf("%s; in use by processes of uid %d: %s", msg, uid, u)
		}
		if notJudged != "" {
			status, remediation = hostlimits.StatusSkip, ""
			msg = fmt.Sprintf("Not judged (%s): %s", notJudged, msg)
		}
	}
	return scionruntime.CheckResult{Name: "inotify-limits", Status: status, Message: msg, Remediation: remediation}
}
