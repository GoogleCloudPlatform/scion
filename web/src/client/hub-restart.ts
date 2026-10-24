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
 * Waiting for the hub to come back after a restart (Restart Hub, or a
 * binary update that restarts the hub).
 */

import { apiFetch } from './api.js';

/** A hub whose uptime is below this many seconds has freshly restarted. */
export const FRESH_UPTIME_SECONDS = 120;

/** Default time to wait for the hub to come back: 5 minutes. */
export const DEFAULT_RESTART_TIMEOUT_MS = 300_000;

export interface WaitForHubRestartOptions {
  /**
   * When set, the hub only counts as back once /healthz reports this
   * scionVersion (the target of an update).
   */
  expectVersion?: string | undefined;
  /** Overall deadline in milliseconds. Default 300000 (5 minutes). */
  timeoutMs?: number;
  /** Stops the wait (for example when the page is left). */
  signal?: AbortSignal;
  /**
   * Called when the hub answers but does not yet count as restarted. When
   * it resolves true the wait ends at once with ok=false and stopped=true
   * (for example: the run being waited for has already failed).
   */
  stopWhen?: (observed: HubHealthObservation) => Promise<boolean>;
}

/** The last /healthz answer seen while waiting. */
export interface HubHealthObservation {
  version: string | undefined;
  uptime: string | undefined;
  uptimeSeconds: number | null;
}

export type HubRestartResult =
  | { ok: true; observed: HubHealthObservation }
  | {
      ok: false;
      /** The last /healthz answer, or null when the hub never answered. */
      observed: HubHealthObservation | null;
      /** Set when the wait was stopped through the signal, not timed out. */
      aborted?: true;
      /** Set when stopWhen ended the wait. */
      stopped?: true;
    };

const INITIAL_DELAY_MS = 500;
const FIRST_INTERVAL_MS = 1000;
const MAX_INTERVAL_MS = 2000;

/**
 * Parse a Go duration string (e.g. "850ms", "3s", "1m5s", "2h3m") into total
 * seconds. Returns null if the format is unrecognized.
 */
export function parseGoUptimeSeconds(uptime: string | undefined): number | null {
  if (!uptime) return null;
  let total = 0;
  let matched = false;
  // Multi-letter units come first so "850ms" is not read as minutes.
  const re = /(\d+(?:\.\d+)?)(ms|µs|us|ns|h|m|s)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(uptime)) !== null) {
    matched = true;
    const val = parseFloat(m[1]);
    switch (m[2]) {
      case 'h':
        total += val * 3600;
        break;
      case 'm':
        total += val * 60;
        break;
      case 's':
        total += val;
        break;
      case 'ms':
        total += val / 1000;
        break;
      default:
        break; // µs, ns: negligible
    }
  }
  return matched ? total : null;
}

/** Sleeps for ms, or until the signal aborts. */
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise<void>((resolve) => {
    if (signal?.aborted) {
      resolve();
      return;
    }
    const onAbort = (): void => {
      clearTimeout(timer);
      resolve();
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

/**
 * Polls /healthz until the hub answers with a fresh uptime (under
 * {@link FRESH_UPTIME_SECONDS}) and, when expectVersion is given, with that
 * scionVersion. Network errors and non-2xx answers mean the hub is still
 * down and polling continues. Resolves with ok=false on timeout, carrying
 * the last answer so the caller can say what it saw. When the signal
 * aborts, resolves at once with ok=false and aborted=true.
 */
export async function waitForHubRestart(
  options: WaitForHubRestartOptions = {}
): Promise<HubRestartResult> {
  const { expectVersion, timeoutMs = DEFAULT_RESTART_TIMEOUT_MS, signal, stopWhen } = options;
  const deadline = Date.now() + timeoutMs;
  let observed: HubHealthObservation | null = null;

  const aborted = (): HubRestartResult => ({ ok: false, observed, aborted: true });

  await sleep(INITIAL_DELAY_MS, signal);

  let interval = FIRST_INTERVAL_MS;
  while (Date.now() < deadline) {
    if (signal?.aborted) return aborted();
    try {
      const res = await apiFetch('/healthz', {
        authErrors: 'caller',
        ...(signal ? { signal } : {}),
      });
      if (res.ok) {
        const data = (await res.json()) as { uptime?: string; scionVersion?: string };
        const seen: HubHealthObservation = {
          version: data.scionVersion,
          uptime: data.uptime,
          uptimeSeconds: parseGoUptimeSeconds(data.uptime),
        };
        observed = seen;
        const fresh = seen.uptimeSeconds !== null && seen.uptimeSeconds < FRESH_UPTIME_SECONDS;
        const versionOk = !expectVersion || seen.version === expectVersion;
        if (fresh && versionOk) {
          return { ok: true, observed: seen };
        }
        if (stopWhen && (await stopWhen(seen).catch(() => false))) {
          return signal?.aborted ? aborted() : { ok: false, observed: seen, stopped: true };
        }
      }
    } catch {
      // Hub still down: keep polling.
    }
    if (signal?.aborted) return aborted();
    await sleep(interval, signal);
    interval = Math.min(interval * 1.5, MAX_INTERVAL_MS);
  }
  return signal?.aborted ? aborted() : { ok: false, observed };
}
