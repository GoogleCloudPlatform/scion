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
