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

/** Global teardown of the test identity session suite: stops the hub. */

import { stopHub } from '../harness/hub.js';
import { removeStateDir, stateFile } from './state.js';

// eslint-disable-next-line @typescript-eslint/require-await -- Playwright calls this as an async teardown hook; it stays async so it keeps returning a Promise
async function globalTeardown(): Promise<void> {
  stopHub(stateFile());
  removeStateDir();
}

export default globalTeardown;
