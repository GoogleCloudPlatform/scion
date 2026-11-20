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

package agent

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
)

// homeLayerDir is the directory of a harness-config or template directory
// whose contents seed the agent home.
const homeLayerDir = "home"

// overlayHomeLayer copies the home/ directory of a harness-config or
// template directory onto the agent home, over what is there (entries the
// layer does not hold are kept). It reports whether a home/ directory was
// copied; a missing directory, or a home/ that is not a directory, copies
// nothing.
//
// The source directory is opened with openDir (HarnessConfigDir.OpenDir or
// Template.OpenDir), so no component from the trusted root it was found
// from is followed through a symbolic link, and home/ and everything below
// it is read without following a link either. The copy fails closed: a
// linked source directory or home/, or a symbolic link, special file or
// hard-linked file anywhere below home/, is refused before anything is
// written, with an error that names the entry relative to the source
// directory, never a host path.
func overlayHomeLayer(home *agenthome.Home, openDir func() (*agenthome.Home, error)) (bool, error) {
	src, err := openDir()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case isLinkRefusal(err):
		return false, errors.New("its directory is a symbolic link or sits below one; replace the link with the directory itself")
	case err != nil:
		return false, err
	}
	defer func() { _ = src.Close() }()

	fi, err := src.Lstat(homeLayerDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case fi.Mode()&fs.ModeSymlink != 0:
		return false, errors.New(homeLayerDir + " is a symbolic link; replace it with the directory itself")
	case !fi.IsDir():
		return false, nil
	}

	if err := agenthome.CheckTree(src, homeLayerDir); err != nil {
		var refused *agenthome.RefusedEntryError
		if errors.As(err, &refused) {
			return false, fmt.Errorf("%s/%w", homeLayerDir, refused)
		}
		return false, err
	}
	if err := home.OverlayTreeFrom(src, homeLayerDir, ".", agenthome.Limits{}); err != nil {
		return false, err
	}
	return true, nil
}
