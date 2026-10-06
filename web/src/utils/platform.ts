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
 * Detect whether the current device is a Mac (including iPhone/iPad/iPod),
 * for platform-specific keyboard handling such as shortcut labels. Prefers
 * the User-Agent Client Hints API (`navigator.userAgentData`), which is not
 * subject to User-Agent string reduction, and falls back to the deprecated
 * `navigator.platform` where Client Hints is unavailable -- notably Safari,
 * which never implemented it. Guarded for environments with no `navigator`
 * at all.
 */
export function isMacPlatform(): boolean {
  if (typeof navigator === 'undefined') return false;
  const uaDataPlatform = (navigator as Navigator & { userAgentData?: { platform?: string } })
    .userAgentData?.platform;
  if (uaDataPlatform) return /mac/i.test(uaDataPlatform);
  return /Mac|iPhone|iPad|iPod/.test(navigator.platform);
}
