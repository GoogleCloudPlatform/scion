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
	"fmt"
)

// ANSI Color codes
const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	LGreen = "\033[92m"
	Brown  = "\033[33m" // ANSI yellow often serves as brown
	Yellow = "\033[93m"
	Cyan   = "\033[36m"
	White  = "\033[97m"
	Bold   = "\033[1m"
	Blue   = "\033[34m"
	Black  = "\033[30m"
	BgRed  = "\033[41m"
)

// GetBanner returns the refined ASCII art banner
func GetBanner() string {
	return fmt.Sprintf(`
       %s.--%s(%s★%s)          %s   █████████      █████████    █████      █████████     █████    ████
      %s/                %s  ███░░░░░███    ███░░░░░███  ░░███      ███░░░░░███   ░░█████   ███
     %s/                 %s ░███    ░░░    ███     ░░░    ░███     ███     ░███    ░██████  ███
    %s/                  %s ░░█████████   ░███            ░███    ░███     ░███    ░███░███ ███
%s---%s*%s-----%s(%s◈%s)           %s  ░░░░░░░░███  ░███            ░███    ░███     ░███    ░███░░██████
    %s\                  %s  ███    ░███  ░░███  ░░███    ░███    ░░███   ░███     ░███ ░░█████
     %s\                 %s ░░█████████    ░░█████████    █████    ░░█████████     █████  ░░████
      %s'--%s(%s▲%s)%s           %s  ░░░░░░░░░      ░░░░░░░░░    ░░░░░      ░░░░░░░░░     ░░░░░    ░░░░ %s
`,
		Blue, LGreen, Yellow, LGreen, LGreen,
		Blue, LGreen,
		Blue, LGreen,
		Blue, LGreen,
		Blue, Yellow, Blue, LGreen, Yellow, LGreen, LGreen,
		Blue, LGreen,
		Blue, LGreen,
		Blue, LGreen, Yellow, LGreen, Reset, LGreen, Reset)
}
