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
 * Shared confirmation dialog.
 *
 * Provides an imperative `showConfirm()` function that replaces the native
 * `confirm()` call with a Shoelace-based modal dialog. Uses `sl-dialog`
 * internally, consistent with `scion-quick-message-dialog`.
 *
 * Usage:
 *   import { showConfirm } from '../shared/confirm-dialog.js';
 *
 *   // Simple — mirrors confirm():
 *   if (!(await showConfirm('Delete this agent?'))) return;
 *
 *   // With the altKey bypass pattern:
 *   if (!event?.altKey && !(await showConfirm('Delete this agent?'))) return;
 */

export interface ConfirmOptions {
  /** Dialog title. Default: "Confirm". */
  title?: string;
  /** Label for the confirm button. Default: "Confirm". */
  confirmText?: string;
  /** Label for the cancel button. Default: "Cancel". */
  cancelText?: string;
  /** Variant for the confirm button. Default: 'danger' (destructive actions). */
  variant?: 'primary' | 'danger';
}

/**
 * Show a confirmation dialog and return a promise that resolves to `true`
 * (confirmed) or `false` (cancelled / dismissed).
 */
export function showConfirm(
  message: string,
  options?: ConfirmOptions,
): Promise<boolean> {
  const title = options?.title ?? 'Confirm';
  const confirmText = options?.confirmText ?? 'Confirm';
  const cancelText = options?.cancelText ?? 'Cancel';
  const variant = options?.variant ?? 'danger';

  return new Promise<boolean>((resolve) => {
    const dialog = document.createElement('sl-dialog');
    dialog.label = title;

    // Intercept all close requests so cleanup() owns the close exclusively.
    dialog.addEventListener('sl-request-close', (e: Event) => {
      e.preventDefault();
      const detail = (e as CustomEvent<{ source: string }>).detail;
      if (detail?.source === 'overlay') {
        // Prevent closing on overlay click — user must choose a button.
        return;
      }
      // Escape key or close button → treat as cancel
      cleanup(false);
    });

    // Build the message content. Newlines are preserved via CSS white-space.
    const body = document.createElement('div');
    body.style.whiteSpace = 'pre-line';
    body.textContent = message;
    dialog.appendChild(body);

    // Cancel button
    const cancelBtn = document.createElement('sl-button');
    cancelBtn.slot = 'footer';
    cancelBtn.setAttribute('variant', 'default');
    cancelBtn.textContent = cancelText;
    cancelBtn.addEventListener('click', () => cleanup(false));
    dialog.appendChild(cancelBtn);

    // Confirm button
    const confirmBtn = document.createElement('sl-button');
    confirmBtn.slot = 'footer';
    confirmBtn.setAttribute('variant', variant);
    confirmBtn.textContent = confirmText;
    confirmBtn.style.marginInlineStart = '0.5rem';
    confirmBtn.addEventListener('click', () => cleanup(true));
    dialog.appendChild(confirmBtn);

    // Enter key confirms the dialog (standard UX pattern).
    // Guard against intercepting Enter inside form fields.
    dialog.addEventListener('keydown', (e: KeyboardEvent) => {
      if (e.key === 'Enter') {
        const tag = (e.target as HTMLElement)?.tagName?.toLowerCase();
        if (tag === 'textarea' || tag === 'input' || tag === 'sl-textarea' || tag === 'sl-input' || tag === 'button' || tag === 'sl-button') {
          return;
        }
        e.preventDefault();
        cleanup(true);
      }
    });

    let resolved = false;
    function cleanup(result: boolean) {
      if (resolved) return;
      resolved = true;
      dialog.open = false;
      // Wait for the close animation before removing from DOM.
      dialog.addEventListener(
        'sl-after-hide',
        () => {
          dialog.remove();
          resolve(result);
        },
        { once: true },
      );
    }

    document.body.appendChild(dialog);
    // Open after appending so Shoelace can initialise.
    requestAnimationFrame(() => {
      dialog.open = true;
    });
  });
}
