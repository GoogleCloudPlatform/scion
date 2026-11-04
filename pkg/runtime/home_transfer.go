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
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// The home transfer set: what of the broker-side agent home a runtime
// whose agent home lives on shared storage copies there. Kubernetes
// NFS-home pods (nfsHomeSyncExcludes) and Cloud Run instances
// (cloudRunHomeExcludes) both derive what they leave out from
// homeTransferExcludes, so the two cannot drift apart.

// Paths, relative to the agent home, of the harness bundle's secrets and
// outputs on the broker. They are never transferred into a home on shared
// storage: the secrets are delivered to an in-memory directory
// (SCION_HARNESS_SECRETS_DIR) and the outputs are regenerated in an
// in-memory directory (SCION_HARNESS_OUTPUTS_DIR).
const (
	harnessBundleSecretsRel = ".scion/harness/secrets"
	harnessBundleOutputsRel = ".scion/harness/outputs"
)

// stagedFileKind is where a staged file comes from.
type stagedFileKind int

const (
	// stagedFileSecret is a file-type resolved secret.
	stagedFileSecret stagedFileKind = iota
	// stagedVariablesFile is secrets.json, carrying the variable-type
	// resolved secrets.
	stagedVariablesFile
	// stagedAuthFile is a file of the resolved harness auth.
	stagedAuthFile
)

// stagedFile is one file a runtime delivers to the agent container through
// its secret path rather than through the agent home.
type stagedFile struct {
	Kind stagedFileKind
	// Key names the file within its delivery: the secret name, "secrets.json",
	// or "auth-file-<index into ResolvedAuth.Files>".
	Key string
	// Target is the absolute container path.
	Target string
}

// stagedFiles lists, in a stable order, the files config's resolved
// secrets and auth deliver to the agent container: each file-type secret,
// then secrets.json when there is a variable-type secret and withVariables
// is set, then each auth file with a source.
func stagedFiles(config RunConfig, withVariables bool) []stagedFile {
	containerHome := util.GetHomeDir(config.UnixUsername)
	var out []stagedFile
	hasVariable := false
	for _, s := range config.ResolvedSecrets {
		switch s.Type {
		case "file":
			out = append(out, stagedFile{Kind: stagedFileSecret, Key: s.Name, Target: expandTildeTarget(s.Target, containerHome)})
		case "variable":
			hasVariable = true
		}
	}
	if hasVariable && withVariables {
		out = append(out, stagedFile{Kind: stagedVariablesFile, Key: "secrets.json", Target: path.Join(containerHome, ".scion", "secrets.json")})
	}
	if config.ResolvedAuth != nil {
		for i, f := range config.ResolvedAuth.Files {
			if f.SourcePath == "" {
				continue
			}
			out = append(out, stagedFile{Kind: stagedAuthFile, Key: fmt.Sprintf("auth-file-%d", i), Target: expandTildeTarget(f.ContainerPath, containerHome)})
		}
	}
	return out
}

// homeTransferExcludes returns what a transfer of config's agent home onto
// shared storage leaves out, as paths relative to the home: the home
// targets of the staged files (stagedFiles, which reach the container by
// their own path), then the harness bundle's secrets and outputs.
// withVariables is whether the runtime delivers secrets.json.
func homeTransferExcludes(config RunConfig, withVariables bool) []string {
	home := util.GetHomeDir(config.UnixUsername)
	var out []string
	for _, f := range stagedFiles(config, withVariables) {
		if !placedInK8sHome(f.Target, config.UnixUsername) {
			continue
		}
		out = append(out, strings.TrimPrefix(path.Clean(f.Target), home+"/"))
	}
	return append(out, harnessBundleSecretsRel, harnessBundleOutputsRel)
}

// homeRelExcluded reports whether rel (slash-separated, relative to the
// home) is one of excludes or lies below one of them.
func homeRelExcluded(rel string, excludes []string) bool {
	for _, ex := range excludes {
		if rel == ex || strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}
