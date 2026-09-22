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
 * Tests for showToast() — verifies that:
 * 1. Toasts are created with correct attributes and DOM structure.
 * 2. The app does NOT register a competing sl-after-hide cleanup listener,
 *    so Shoelace's toast() lifecycle is the sole cleanup owner (#1733).
 * 3. No NotFoundError occurs during toast auto-dismiss.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { showToast } from './toast.js';

/**
 * Instrument sl-alert elements to track:
 * - Whether toast() was called
 * - All sl-after-hide listeners registered
 * - Whether any listener calls alert.remove()
 *
 * Returns a helper to query instrumentation state.
 */
function instrumentAlertCreation() {
  const origCreate = document.createElement.bind(document);
  const alerts: Array<{
    element: HTMLElement;
    toastCalled: boolean;
    afterHideListeners: Array<() => void>;
  }> = [];

  vi.spyOn(document, 'createElement').mockImplementation(
    (tag: string, options?: ElementCreationOptions) => {
      const el = origCreate(tag, options);
      if (tag === 'sl-alert') {
        const record = {
          element: el,
          toastCalled: false,
          afterHideListeners: [] as Array<() => void>,
        };

        // Intercept addEventListener to track sl-after-hide registrations
        const origAddEventListener = el.addEventListener.bind(el);
        vi.spyOn(el, 'addEventListener').mockImplementation(
          (type: string, listener: EventListenerOrEventListenerObject, opts?: unknown) => {
            if (type === 'sl-after-hide' && typeof listener === 'function') {
              record.afterHideListeners.push(listener as () => void);
            }
            origAddEventListener(type, listener, opts as AddEventListenerOptions);
          }
        );

        // Stub toast()
        (el as unknown as Record<string, unknown>).toast = vi.fn(() => {
          record.toastCalled = true;
          return Promise.resolve();
        });

        alerts.push(record);
      }
      return el;
    }
  );

  return {
    get alerts() {
      return alerts;
    },
    get lastAlert() {
      return alerts[alerts.length - 1];
    },
  };
}

describe('showToast', () => {
  afterEach(() => {
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    vi.restoreAllMocks();
  });

  it('creates an sl-alert with correct variant and duration', () => {
    const inst = instrumentAlertCreation();

    showToast('Something went wrong', 'danger');

    const alert = inst.lastAlert;
    expect(alert).toBeDefined();
    expect(
      alert.element.getAttribute('variant') ??
        (alert.element as unknown as Record<string, unknown>).variant
    ).toBe('danger');
  });

  it('calls toast() on the created element', () => {
    const inst = instrumentAlertCreation();

    showToast('Test message', 'success');

    expect(inst.lastAlert.toastCalled).toBe(true);
  });

  it('does NOT register an app-owned sl-after-hide listener (#1733)', () => {
    const inst = instrumentAlertCreation();

    showToast('Test message', 'warning');

    // The app must NOT register any sl-after-hide listener.
    // Shoelace's toast() is the sole cleanup owner.
    expect(inst.lastAlert.afterHideListeners.length).toBe(0);
  });

  it('appends the alert to document.body before calling toast()', () => {
    const inst = instrumentAlertCreation();

    showToast('Test message');

    const alert = inst.lastAlert;
    expect(alert.toastCalled).toBe(true);
    // The element should have been appended to the DOM
    expect(document.body.contains(alert.element)).toBe(true);
  });

  it('includes correct icon for each variant', () => {
    const inst = instrumentAlertCreation();
    const variants = ['danger', 'warning', 'success', 'neutral', 'primary'] as const;
    const expectedIcons: Record<string, string> = {
      danger: 'exclamation-octagon',
      warning: 'exclamation-triangle',
      success: 'check-circle',
      neutral: 'info-circle',
      primary: 'info-circle',
    };

    for (const variant of variants) {
      showToast(`Test ${variant}`, variant);
      const alert = inst.lastAlert;
      const iconEl = alert.element.querySelector('sl-icon');
      expect(iconEl).not.toBeNull();
      expect(iconEl!.getAttribute('name')).toBe(expectedIcons[variant]);
    }
  });

  it('uses custom icon when provided', () => {
    const inst = instrumentAlertCreation();

    showToast('Custom icon', 'success', { icon: 'gear' });

    const iconEl = inst.lastAlert.element.querySelector('sl-icon');
    expect(iconEl!.getAttribute('name')).toBe('gear');
  });

  it('defaults to danger variant', () => {
    const inst = instrumentAlertCreation();

    showToast('Error message');

    expect((inst.lastAlert.element as unknown as Record<string, string>).variant).toBe('danger');
  });
});

// ---------------------------------------------------------------------------
// showToast auto-dismiss — no NotFoundError (#1733)
// ---------------------------------------------------------------------------

describe('showToast auto-dismiss (#1733)', () => {
  afterEach(() => {
    document.querySelectorAll('sl-alert').forEach((el) => el.remove());
    vi.restoreAllMocks();
  });

  it('does not throw NotFoundError when sl-after-hide fires', () => {
    const inst = instrumentAlertCreation();
    const errors: Error[] = [];
    const errorHandler = (e: ErrorEvent) => errors.push(e.error);
    window.addEventListener('error', errorHandler);

    try {
      showToast('Dismiss me', 'warning');

      const alert = inst.lastAlert.element;

      // Simulate what Shoelace's toast() would do: the element is in a
      // toast stack, and on sl-after-hide the stack removes the child.
      // Since we removed the app's competing listener, only Shoelace's
      // listener (which we stubbed via toast()) should fire.
      // Dispatch sl-after-hide to verify no app-side listener throws.
      alert.dispatchEvent(new Event('sl-after-hide'));

      // No errors should have been thrown
      expect(errors.length).toBe(0);
    } finally {
      window.removeEventListener('error', errorHandler);
    }
  });

  it('element remains in DOM after sl-after-hide (cleanup is Shoelace-only)', () => {
    const inst = instrumentAlertCreation();

    showToast('Dismiss me', 'success');

    const alert = inst.lastAlert.element;

    // Before sl-after-hide: element is in DOM
    expect(document.body.contains(alert)).toBe(true);

    // After sl-after-hide: element should STILL be in DOM because
    // the app no longer removes it — Shoelace's toast() is responsible.
    alert.dispatchEvent(new Event('sl-after-hide'));
    expect(document.body.contains(alert)).toBe(true);
  });
});
