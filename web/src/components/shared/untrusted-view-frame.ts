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
 * The untrusted view frame for HTML bundle artifacts: the one definition
 * of the frame and its untrusted-content bar, used by the artifact page
 * (artifact-detail.ts) and the chat preview (chat-file-preview.ts), so the
 * two can never drift apart.
 *
 * The frame loads a view minted for the version (its URL is on the view's
 * own origin, with its own Content-Security-Policy). Scripts run, but never
 * with allow-same-origin, so the content cannot reach Scion's origin,
 * cookies or storage; the referrer is never sent.
 */

import { html, css } from 'lit';
import type { TemplateResult } from 'lit';

/**
 * The frame's sandbox. Never add allow-same-origin: combined with
 * allow-scripts it would let the content remove its own sandbox.
 */
export const UNTRUSTED_VIEW_SANDBOX = 'allow-scripts';

/** What the frame shows: the view URL, a frame title and the publisher's label. */
export interface UntrustedViewFrameOptions {
  /** The minted view URL. */
  url: string;
  /** The artifact's title; the frame is titled "<title> (sandboxed)". */
  title: string;
  /** The publisher as shown to the viewer (principalLabel). */
  owner: string;
}

/**
 * Renders the untrusted view frame: a `.untrusted` box holding the
 * `.untrusted-bar` and the sandboxed iframe. Hosts include
 * untrustedViewFrameStyles in their styles and may set
 * `--untrusted-frame-height` (default 70vh).
 */
export function renderUntrustedViewFrame(opts: UntrustedViewFrameOptions): TemplateResult {
  return html`
    <div class="untrusted">
      <div class="untrusted-bar">
        <sl-icon name="shield-lock"></sl-icon>
        Content published by ${opts.owner} · runs sandboxed, isolated from Scion
      </div>
      <iframe
        title=${`${opts.title} (sandboxed)`}
        sandbox=${UNTRUSTED_VIEW_SANDBOX}
        referrerpolicy="no-referrer"
        src=${opts.url}
      ></iframe>
    </div>
  `;
}

/** Styles for renderUntrustedViewFrame, for the host's shadow root. */
export const untrustedViewFrameStyles = css`
  .untrusted {
    border: 2px solid var(--sl-color-warning-500, #f59e0b);
    border-radius: var(--scion-radius, 0.5rem);
    overflow: hidden;
    background: var(--scion-surface, #ffffff);
    display: flex;
    flex-direction: column;
  }
  .untrusted-bar {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    padding: 0.3rem 0.75rem;
    font-size: 0.75rem;
    color: var(--sl-color-warning-800, #92400e);
    background: var(--sl-color-warning-50, #fffbeb);
    border-bottom: 1px solid var(--sl-color-warning-300, #fcd34d);
  }
  .untrusted iframe {
    display: block;
    width: 100%;
    height: var(--untrusted-frame-height, 70vh);
    border: none;
    background: #ffffff;
  }
  .untrusted:fullscreen iframe {
    height: 100%;
    flex: 1;
  }
`;
