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

package k8s

import (
	"errors"
	"testing"

	"k8s.io/client-go/rest"
)

func TestLiveCurrentContext(t *testing.T) {
	path := writeTestKubeconfig(t, kubeconfigWithServer("https://127.0.0.1:6443"))
	got, err := LiveCurrentContext(path)
	if err != nil || got != "x" {
		t.Errorf("LiveCurrentContext = %q, %v; want x, nil", got, err)
	}
	if _, err := LiveCurrentContext(writeTestKubeconfig(t, "clusters: [unclosed\n")); err == nil {
		t.Error("unparsable kubeconfig: want an error")
	}
}

func TestHostConnection(t *testing.T) {
	path := writeTestKubeconfig(t, kubeconfigWithServer("https://127.0.0.1:6443"))
	ctx, mode, err := HostConnection(path)
	if err != nil || ctx != "x" || mode != ConnKubeconfig {
		t.Errorf("kubeconfig: got %q, %v, %v; want x, kubeconfig, nil", ctx, mode, err)
	}

	failLoad := func(string, string) (*rest.Config, loadedSource, error) {
		return nil, loadedSource{}, errKubeconfigLoad
	}
	ctx, mode, err = hostConnection("", failLoad, func() (*rest.Config, error) { return &rest.Config{}, nil })
	if err != nil || ctx != "" || mode != ConnInCluster {
		t.Errorf("in-cluster: got %q, %v, %v; want \"\", in-cluster, nil", ctx, mode, err)
	}
	_, mode, err = hostConnection("", failLoad, func() (*rest.Config, error) { return nil, errors.New("not in a pod") })
	if err == nil || mode != ConnUnknown {
		t.Errorf("neither: got %v, %v; want unknown and an error", mode, err)
	}
	_, mode, err = hostConnection("/explicit", failLoad, func() (*rest.Config, error) { return &rest.Config{}, nil })
	if err == nil || mode != ConnUnknown {
		t.Errorf("explicit path never falls back to in-cluster: got %v, %v", mode, err)
	}
}
