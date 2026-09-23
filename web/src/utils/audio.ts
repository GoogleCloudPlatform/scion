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
 * Minimal chime player for chat notifications. Generates a short, subtle
 * two-tone chime using the Web Audio API — no external audio file needed.
 */

const CHIME_GLOBAL_KEY = 'scion-chat-chime';

/** Get/set global chime preference. Default: ON. */
export function isChimeEnabled(): boolean {
  return localStorage.getItem(CHIME_GLOBAL_KEY) !== 'false';
}
export function setChimeEnabled(on: boolean): void {
  if (on) {
    localStorage.removeItem(CHIME_GLOBAL_KEY); // default is on
  } else {
    localStorage.setItem(CHIME_GLOBAL_KEY, 'false');
  }
}

/** Per-project chime key. */
function projectChimeKey(projectId: string): string {
  return `scion-chat-chime-${projectId}`;
}

/** Get/set per-project chime preference. Default: ON. */
export function isProjectChimeEnabled(projectId: string): boolean {
  return localStorage.getItem(projectChimeKey(projectId)) !== 'false';
}
export function setProjectChimeEnabled(projectId: string, on: boolean): void {
  if (on) {
    localStorage.removeItem(projectChimeKey(projectId));
  } else {
    localStorage.setItem(projectChimeKey(projectId), 'false');
  }
}

/** Whether chime should play for a given project (both global and project must be on). */
export function shouldChime(projectId: string): boolean {
  return isChimeEnabled() && isProjectChimeEnabled(projectId);
}

let audioCtx: AudioContext | null = null;

/**
 * Play a subtle two-tone chime. Uses Web Audio API to synthesize a short
 * pleasant notification sound — no external audio file required.
 *
 * The chime is two quick sine tones (E5 → G5), ~150ms total, at low volume.
 * Designed to be noticeable but not intrusive.
 */
export function playChime(): void {
  try {
    if (!audioCtx) {
      if (typeof window === 'undefined') return;
      const AudioContextClass = window.AudioContext || (window as any).webkitAudioContext;
      if (!AudioContextClass) return;
      audioCtx = new AudioContextClass();
    }
    // Resume if suspended (browsers require user gesture to start AudioContext)
    if (audioCtx.state === 'suspended') {
      audioCtx.resume().catch(() => {
        // Resume can reject if the browser still refuses without a user
        // gesture — the chime just silently does not play this time.
      });
    }

    const now = audioCtx.currentTime;
    const gain = audioCtx.createGain();
    gain.connect(audioCtx.destination);
    gain.gain.setValueAtTime(0.08, now); // quiet
    gain.gain.exponentialRampToValueAtTime(0.001, now + 0.3);

    // First tone: E5 (659 Hz), 80ms
    const osc1 = audioCtx.createOscillator();
    osc1.type = 'sine';
    osc1.frequency.setValueAtTime(659, now);
    osc1.connect(gain);
    osc1.start(now);
    osc1.stop(now + 0.08);

    // Second tone: G5 (784 Hz), 80ms, starts 70ms after first
    const osc2 = audioCtx.createOscillator();
    osc2.type = 'sine';
    osc2.frequency.setValueAtTime(784, now + 0.07);
    osc2.connect(gain);
    osc2.onended = () => {
      osc1.disconnect();
      osc2.disconnect();
      gain.disconnect();
    };
    osc2.start(now + 0.07);
    osc2.stop(now + 0.15);
  } catch {
    // Audio not available — silently ignore
  }
}

let lastChimeTime = 0;
const CHIME_COOLDOWN_MS = 2000; // 2 seconds between chimes

/**
 * Play the chime, subject to per-project/global preferences and a cooldown
 * that prevents rapid-fire chimes when several messages land in quick
 * succession (e.g. an agent sending several messages back to back).
 */
export function playChimeThrottled(projectId: string): void {
  if (!shouldChime(projectId)) return;
  const now = Date.now();
  if (now - lastChimeTime < CHIME_COOLDOWN_MS) return;
  lastChimeTime = now;
  playChime();
}
