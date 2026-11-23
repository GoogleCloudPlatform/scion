/**
 * Vitest setup — polyfills for Happy DOM compatibility with Shoelace and DOMPurify.
 */

// DOMPurify reads node names through the `Node.prototype.nodeName` getter.
// In Happy DOM that getter returns '' for elements (each element subclass
// defines its own), so DOMPurify unwraps the first element it meets, stops
// walking the tree, leaves disallowed elements/attributes in place, and skips
// hooks. Delegate the base getter to the node's own subclass getter so every
// unit test exercises real DOMPurify sanitization.
if (typeof Node !== 'undefined') {
  const base = Object.getOwnPropertyDescriptor(Node.prototype, 'nodeName');

  Object.defineProperty(Node.prototype, 'nodeName', {
    configurable: true,
    get(this: Node): string {
      for (
        let proto: object | null = Object.getPrototypeOf(this) as object | null;
        proto && proto !== Node.prototype;
        proto = Object.getPrototypeOf(proto) as object | null
      ) {
        const own = Object.getOwnPropertyDescriptor(proto, 'nodeName');
        if (own?.get) return own.get.call(this) as string;
      }
      return (base?.get?.call(this) as string | undefined) ?? '';
    },
  });
}

// Happy DOM's NodeIterator delegates directly to TreeWalker without WHATWG
// pre-removing steps. When DOMPurify removes a disallowed element (e.g.
// <script>) during its walk, the iterator's currentNode becomes detached and
// nextNode() terminates early, leaving every subsequent sibling unsanitized.
// Wrap createNodeIterator on the owning Document prototype (in Happy DOM,
// window.Document.prototype is a per-window subclass separate from the base
// Document prototype shared by document and template.content.ownerDocument)
// so nextNode() resumes from the previous in-tree node whenever the last
// yielded node has been removed from root's subtree.
if (typeof document !== 'undefined') {
  let docProto: object | null = Object.getPrototypeOf(document) as object | null;
  while (docProto && !Object.prototype.hasOwnProperty.call(docProto, 'createNodeIterator')) {
    docProto = Object.getPrototypeOf(docProto) as object | null;
  }
  const origCreateNodeIterator = docProto
    ? (docProto as Document).createNodeIterator
    : undefined;
  const patchedFlag = Symbol.for('scion.vitest.liveNodeIterator');

  if (
    docProto &&
    typeof origCreateNodeIterator === 'function' &&
    !(origCreateNodeIterator as unknown as Record<symbol, boolean>)[patchedFlag]
  ) {
    const isInSubtree = (root: Node, node: Node | null): boolean => {
      for (let cur: Node | null = node; cur; cur = cur.parentNode) {
        if (cur === root) return true;
      }
      return false;
    };

    // Descend through childNodes rather than firstChild/lastChild: Happy DOM's
    // HTMLTemplateElement.firstChild/lastChild return nodes inside
    // template.content (whose parentNode is the detached DocumentFragment),
    // whereas childNodes is empty for <template> as in browsers.
    const nextInTree = (root: Node, node: Node): Node | null => {
      const first = node.childNodes[0];
      if (first) return first;
      for (let cur: Node | null = node; cur && cur !== root; cur = cur.parentNode) {
        if (cur.nextSibling) return cur.nextSibling;
      }
      return null;
    };

    const prevInTree = (root: Node, node: Node): Node => {
      if (node === root) return root;
      if (node.previousSibling) {
        let cur: Node = node.previousSibling;
        while (cur.childNodes.length) {
          cur = cur.childNodes[cur.childNodes.length - 1]!;
        }
        return cur;
      }
      return node.parentNode ?? root;
    };

    const matchesFilter = (
      node: Node,
      whatToShow: number,
      filter: NodeFilter | null | undefined,
    ): boolean => {
      const bit = 1 << (node.nodeType - 1);
      if ((whatToShow & bit) === 0) return false;
      if (!filter) return true;
      const accept = typeof filter === 'function' ? filter(node) : filter.acceptNode(node);
      return accept === NodeFilter.FILTER_ACCEPT;
    };

    const wrappedCreateNodeIterator = function (
      this: Document,
      root: Node,
      whatToShow = 0xffffffff,
      filter: NodeFilter | null = null,
    ): NodeIterator {
      const native = origCreateNodeIterator.call(this, root, whatToShow, filter);
      let started = false;
      let current: Node | null = null;
      let fallbacks: Node[] = [];

      const recordFallbacks = (node: Node): void => {
        const chain: Node[] = [prevInTree(root, node)];
        for (let p = node.parentNode; p; p = p.parentNode) {
          chain.push(p);
          if (p === root) break;
        }
        fallbacks = chain;
      };

      const nextNode = (): Node | null => {
        let candidate: Node | null;
        if (!started) {
          started = true;
          candidate = root;
        } else if (current && isInSubtree(root, current)) {
          candidate = nextInTree(root, current);
        } else {
          const anchor = fallbacks.find((n) => isInSubtree(root, n)) ?? null;
          candidate = anchor ? nextInTree(root, anchor) : null;
        }

        while (candidate) {
          if (matchesFilter(candidate, whatToShow, filter)) {
            current = candidate;
            recordFallbacks(candidate);
            return candidate;
          }
          candidate = nextInTree(root, candidate);
        }
        current = null;
        fallbacks = [];
        return null;
      };

      return {
        get root() {
          return native.root;
        },
        get whatToShow() {
          return native.whatToShow;
        },
        get filter() {
          return native.filter;
        },
        get referenceNode() {
          return current ?? root;
        },
        get pointerBeforeReferenceNode() {
          return !started;
        },
        nextNode,
        previousNode() {
          return native.previousNode();
        },
        detach() {
          // No-op per WHATWG DOM spec.
        },
      } as NodeIterator;
    };
    (wrappedCreateNodeIterator as unknown as Record<symbol, boolean>)[patchedFlag] = true;
    (docProto as Document).createNodeIterator = wrappedCreateNodeIterator;
  }
}

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
