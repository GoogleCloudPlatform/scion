/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Feature flag that gates every artifact surface in the web UI.
 *
 * It lives in its own module so the app shell and the sidebar navigation can
 * check it without pulling the artifacts API client into the entry bundle;
 * the client loads with the artifact pages that use it.
 */
export const ARTIFACTS_FLAG = 'hub.artifacts';
