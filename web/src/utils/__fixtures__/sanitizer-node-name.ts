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
 * Lets the sanitizer walk a tree in the test DOM as it does in a browser.
 *
 * DOMPurify reads node names through the `Node.prototype.nodeName` getter.
 * In the test DOM that getter returns '' for elements (each element class
 * has its own), so DOMPurify would unwrap the first element it meets and
 * stop walking. This delegates the base getter to the node's own one.
 *
 * Import it before DOMPurify loads: the renderer loads DOMPurify on first
 * use, so importing this at the top of a test file is enough.
 */

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
