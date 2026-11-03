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

package hub

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBrokerScionDir: the .scion directory named in the refusal text uses
// the separator of the broker's own path, whatever the hub's OS is.
func TestBrokerScionDir(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`/srv/proj`, `/srv/proj/.scion`},
		{`/srv/proj/`, `/srv/proj/.scion`},
		{`/srv/proj/.scion`, `/srv/proj/.scion`},
		{`/srv/proj/.scion/`, `/srv/proj/.scion`},
		{`/`, `/.scion`},
		{`C:\srv\proj`, `C:\srv\proj\.scion`},
		{`C:\srv\proj\`, `C:\srv\proj\.scion`},
		{`C:\srv\proj\.scion`, `C:\srv\proj\.scion`},
		{`C:/srv/proj`, `C:/srv/proj/.scion`},
		{`\\host\share\proj`, `\\host\share\proj\.scion`},
	} {
		assert.Equal(t, tc.want, brokerScionDir(tc.in), "LocalPath %q", tc.in)
	}
}

// TestProjectPathRefusalText_WindowsLocalPath: a Windows-style LocalPath
// is named with backslashes, once, in the refusal text.
func TestProjectPathRefusalText_WindowsLocalPath(t *testing.T) {
	for _, lp := range []string{`C:\srv\proj`, `C:\srv\proj\.scion`} {
		got := projectPathRefusalText("edge-1", lp)
		assert.Contains(t, got, `create C:\srv\proj\.scion as a directory`, "LocalPath %q", lp)
		assert.False(t, strings.Contains(got, `/.scion`) || strings.Contains(got, `.scion\.scion`), "LocalPath %q: %s", lp, got)
	}
}
