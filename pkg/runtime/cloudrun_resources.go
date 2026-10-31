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

package runtime

import (
	"fmt"
	"strconv"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Cloud Run CPU and memory limits, from the platform's vCPU and memory
// table (https://cloud.google.com/run/docs/configuring/services/cpu and
// .../memory-limits). Fractional vCPU below 1 is not offered: it requires
// request-based billing and a concurrency of 1, and an instance serves no
// requests.
const (
	cloudRunDefaultCPU       = 1         // vCPU an instance gets when none is set
	cloudRunDefaultMemoryMiB = 512       // memory an instance gets when none is set
	cloudRunMinMemoryMiB     = 512       // second generation execution environment
	cloudRunMaxMemoryMiB     = 32 * 1024 // 32Gi

	cloudRunMiB = 1 << 20
)

// cloudRunCPUMemoryBands maps each supported vCPU count to the memory range
// (in MiB, inclusive) the platform accepts with it.
var cloudRunCPUMemoryBands = []struct {
	cpu            int64
	minMiB, maxMiB int64
}{
	{1, cloudRunMinMemoryMiB, 4 * 1024},
	{2, cloudRunMinMemoryMiB, 8 * 1024},
	{4, 2 * 1024, 16 * 1024},
	{6, 4 * 1024, 24 * 1024},
	{8, 4 * 1024, 32 * 1024},
}

// cloudRunContainerResources converts the resolved resource spec into the
// Cloud Run container's resource limits. It returns nil when the spec sets
// neither CPU nor memory, so the instance keeps the platform defaults.
//
// Cloud Run has limits only, so each resource takes its limit, or its
// request when no limit is set. On a real start the agent's resolution
// always sets limits.cpu (config.ApplyBuiltinDefaultResources: the
// built-in default of 2, raised to requests.cpu when that is larger), so
// requests.cpu takes effect only above 2; the request fallback for CPU
// applies to specs resolved without those defaults. Values are checked against the Cloud Run
// vCPU and memory table, including the memory range each vCPU count
// allows; a resource left unset is checked at its platform default. CPU is
// sent as a whole vCPU count and memory as "<n>Mi" or "<n>M" (see
// cloudRunMemory), forms the API accepts whatever quantity syntax the spec
// used.
//
// CpuIdle is left unset. It selects request-based CPU allocation on
// services; Cloud Run Instances keep CPU allocated for their whole
// lifetime either way (confirmed on a live instance), so it is not what
// keeps an agent's CPU allocated. resources.disk is not mapped here.
func cloudRunContainerResources(spec *api.ResourceSpec) (*runpb.ResourceRequirements, error) {
	if spec == nil {
		return nil, nil
	}
	cpuValue, cpuField := spec.Limits.CPU, "resources.limits.cpu"
	if cpuValue == "" {
		cpuValue, cpuField = spec.Requests.CPU, "resources.requests.cpu"
	}
	memValue, memField := spec.Limits.Memory, "resources.limits.memory"
	if memValue == "" {
		memValue, memField = spec.Requests.Memory, "resources.requests.memory"
	}
	if cpuValue == "" && memValue == "" {
		return nil, nil
	}

	limits := map[string]string{}
	cpu := int64(cloudRunDefaultCPU)
	if cpuValue != "" {
		q, err := parseResourceSafe(cpuValue, cpuField)
		if err != nil {
			return nil, fmt.Errorf("cloudrun: %w", err)
		}
		cpu, err = cloudRunCPU(q)
		if err != nil {
			return nil, fmt.Errorf("cloudrun: %s %q: %w", cpuField, cpuValue, err)
		}
		limits["cpu"] = strconv.FormatInt(cpu, 10)
	}

	memBytes, memShown := int64(cloudRunDefaultMemoryMiB)*cloudRunMiB, strconv.Itoa(cloudRunDefaultMemoryMiB)+"Mi"
	if memValue != "" {
		q, err := parseResourceSafe(memValue, memField)
		if err != nil {
			return nil, fmt.Errorf("cloudrun: %w", err)
		}
		memBytes, memShown, err = cloudRunMemory(q)
		if err != nil {
			return nil, fmt.Errorf("cloudrun: %s %q: %w", memField, memValue, err)
		}
		limits["memory"] = memShown
	}

	for _, b := range cloudRunCPUMemoryBands {
		if b.cpu != cpu {
			continue
		}
		if memBytes < b.minMiB*cloudRunMiB || memBytes > b.maxMiB*cloudRunMiB {
			return nil, fmt.Errorf("cloudrun: %d vCPU (%s) allows memory %dMi to %dMi, got %s (%s); set resources.limits.cpu or resources.limits.memory",
				cpu, cloudRunResourceSource(cpuValue, cpuField), b.minMiB, b.maxMiB, memShown, cloudRunResourceSource(memValue, memField))
		}
		break
	}

	return &runpb.ResourceRequirements{Limits: limits}, nil
}

// cloudRunCPU returns q as a supported whole vCPU count.
func cloudRunCPU(q resource.Quantity) (int64, error) {
	milli := q.MilliValue()
	if milli%1000 == 0 {
		for _, b := range cloudRunCPUMemoryBands {
			if b.cpu == milli/1000 {
				return b.cpu, nil
			}
		}
	}
	return 0, fmt.Errorf("a Cloud Run instance takes 1, 2, 4, 6 or 8 vCPU")
}

// cloudRunMemory returns q in bytes and in a form the API accepts: whole
// MiB as "<n>Mi", otherwise whole megabytes as "<n>M". Anything finer, or
// outside the Cloud Run memory range, is an error.
func cloudRunMemory(q resource.Quantity) (int64, string, error) {
	maxQ := resource.NewQuantity(cloudRunMaxMemoryMiB*cloudRunMiB, resource.BinarySI)
	if q.Cmp(*maxQ) > 0 {
		return 0, "", fmt.Errorf("memory above the Cloud Run maximum of %dMi", int64(cloudRunMaxMemoryMiB))
	}
	bytes := q.Value()
	if bytes < cloudRunMinMemoryMiB*cloudRunMiB {
		return 0, "", fmt.Errorf("memory below the Cloud Run minimum of %dMi", int64(cloudRunMinMemoryMiB))
	}
	switch {
	case bytes%cloudRunMiB == 0:
		return bytes, strconv.FormatInt(bytes/cloudRunMiB, 10) + "Mi", nil
	case bytes%1_000_000 == 0:
		return bytes, strconv.FormatInt(bytes/1_000_000, 10) + "M", nil
	}
	return 0, "", fmt.Errorf("memory must be a whole number of Mi or M on Cloud Run")
}

// cloudRunResourceSource names where a checked value came from, for errors.
//
// A limits.cpu equal to the built-in default may have been set by the
// agent's resolution rather than by the user (the provenance does not reach
// the runtime), so it is named as either.
func cloudRunResourceSource(value, field string) string {
	if value == "" {
		return "platform default"
	}
	if field == "resources.limits.cpu" && value == config.BuiltinDefaultResources().Limits.CPU {
		return "from resources.limits.cpu or the built-in default " + value
	}
	return "from " + field
}

// logCloudRunResources records the CPU and memory an instance is created
// with, naming a platform default where the spec set none. resources.disk
// has no instance-local volume to size on this runtime yet, so a set disk
// is reported as not applied.
func logCloudRunResources(instanceID string, res *runpb.ResourceRequirements, spec *api.ResourceSpec) {
	cpu := res.GetLimits()["cpu"]
	if cpu == "" {
		cpu = strconv.Itoa(cloudRunDefaultCPU) + " (platform default)"
	}
	memory := res.GetLimits()["memory"]
	if memory == "" {
		memory = strconv.Itoa(cloudRunDefaultMemoryMiB) + "Mi (platform default)"
	}
	attrs := []any{"instance", instanceID, "cpu", cpu, "memory", memory}
	if spec != nil && spec.Disk != "" {
		attrs = append(attrs, "disk_not_applied", spec.Disk)
	}
	runtimeLog().Info("Cloud Run instance resources", attrs...)
}
