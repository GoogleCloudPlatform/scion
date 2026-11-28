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
 * Web upload limits published by the hub in `GET /api/v1/settings/public`
 * (`uploads`) and pre-send size validation helpers.
 */

import { getPublicSettings } from './server-feature-flags.js';

/** Upload limits in bytes (`PublicUploadLimits` in the hub). */
export interface UploadLimits {
  maxAttachmentSize: number;
  maxWorkspaceFileSize: number;
}

/** The hub defaults, used when the hub does not publish its limits. */
export const DEFAULT_UPLOAD_LIMITS: Readonly<UploadLimits> = Object.freeze({
  maxAttachmentSize: 10 * 1024 * 1024,
  maxWorkspaceFileSize: 50 * 1024 * 1024,
});

/**
 * Total single-request body limit for workspace and shared-directory uploads
 * (`maxUploadTotalSize` in pkg/hub/project_workspace_handlers.go).
 */
export const MAX_WORKSPACE_REQUEST_BODY = 100 * 1024 * 1024;

/**
 * Per-file ceiling for legacy single-request workspace and shared-directory
 * uploads (mirrors `config.MaxUploadFileSize` in Go, 100 MiB). Limits above
 * this apply only on the chunked upload path.
 */
export const MAX_LEGACY_WORKSPACE_FILE_SIZE = 100 * 1024 * 1024;

/**
 * Clamp a hub-published workspace per-file limit to the legacy single-request
 * ceiling ({@link MAX_LEGACY_WORKSPACE_FILE_SIZE}, 100 MiB).
 */
export function legacyWorkspaceMaxFileSize(maxWorkspaceFileSize: number): number {
  return Math.min(maxWorkspaceFileSize, MAX_LEGACY_WORKSPACE_FILE_SIZE);
}

function positive(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : fallback;
}

/** The hub's upload limits, or the defaults for a field it does not send. */
export async function getUploadLimits(): Promise<UploadLimits> {
  const settings = await getPublicSettings();
  const u = settings?.uploads;
  return {
    maxAttachmentSize: positive(u?.maxAttachmentSize, DEFAULT_UPLOAD_LIMITS.maxAttachmentSize),
    maxWorkspaceFileSize: positive(
      u?.maxWorkspaceFileSize,
      DEFAULT_UPLOAD_LIMITS.maxWorkspaceFileSize
    ),
  };
}

/** A byte count for a message: "10 MiB", "1 GiB", "1.5 GiB", "900 B". */
export function formatUploadSize(bytes: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  const text = Number.isInteger(value) ? String(value) : value.toFixed(1);
  return `${text} ${units[i]}`;
}

/** The message for a file over the per-file limit. */
export function fileTooLargeMessage(name: string, limit: number): string {
  return `File "${name}" exceeds the maximum size of ${formatUploadSize(limit)}`;
}

/**
 * Validate workspace / shared-dir upload files against the hub's per-file
 * limit and single-request total body limit. Returns an error message, or
 * null when all files are within the limits.
 */
export function validateWorkspaceUploadFiles(
  files: Iterable<Pick<File, 'name' | 'size'>>,
  maxFileSize: number,
  maxTotalSize: number = MAX_WORKSPACE_REQUEST_BODY
): string | null {
  let total = 0;
  for (const file of files) {
    if (file.size > maxFileSize) {
      return fileTooLargeMessage(file.name, maxFileSize);
    }
    total += file.size;
  }
  if (total > maxTotalSize) {
    return `Total upload exceeds the maximum request size of ${formatUploadSize(maxTotalSize)}`;
  }
  return null;
}
