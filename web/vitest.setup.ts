/**
 * Vitest setup — polyfills for Happy DOM compatibility with Shoelace.
 */

// Happy DOM does not implement Element.prototype.getAnimations or
// Element.prototype.animate, which Shoelace calls during component lifecycle
// (open/close/disable transitions via stopAnimations / animateTo).
// Provide no-op stubs to prevent unhandled rejection errors.
if (typeof Element.prototype.getAnimations !== 'function') {
  Element.prototype.getAnimations = function () {
    return [];
  };
}

if (typeof Element.prototype.animate !== 'function') {
  Element.prototype.animate = function () {
    return {
      finished: Promise.resolve(),
      cancel: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
    } as unknown as Animation;
  };
}

// Suppress known Shoelace/Happy DOM unhandled rejections that occur during
// component lifecycle in the test environment. These are harmless —
// Shoelace components still function correctly for testing purposes.
//
// Vitest catches unhandled rejections at the process level, so we must
// intercept there — the browser-style globalThis.addEventListener is not
// sufficient in a Node-based test runner.
{
  const origListeners = process.rawListeners(
    'unhandledRejection',
  ) as ((reason: unknown, promise: Promise<unknown>) => void)[];
  process.removeAllListeners('unhandledRejection');

  process.on('unhandledRejection', (reason: unknown, promise: Promise<unknown>) => {
    const msg = String(
      (reason as { message?: string })?.message ?? reason ?? '',
    );
    if (
      msg.includes('Cannot read from private field') ||
      msg.includes('getAnimations is not a function')
    ) {
      // Swallow known Shoelace / Happy DOM incompatibility
      return;
    }
    // Forward any other rejection to the original Vitest handler(s)
    for (const fn of origListeners) {
      fn(reason, promise);
    }
  });
}
