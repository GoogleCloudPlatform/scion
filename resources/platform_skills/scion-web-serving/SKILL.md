---
name: scion-web-serving
description: >-
  How to serve a web app or page from inside an agent so a human can open it
  through scion expose: ship one self-contained HTML file, the build settings
  each common tool needs for that, how to hand over the right URL, and how to
  verify it honestly. Use before running scion expose for a page or app a
  person will open.
---

# Serving web apps through scion expose

A person opens your page in their own browser through a link on the Hub. Today
such a page can only use what is inside the page itself: separate scripts,
stylesheets, images and requests back to your app are not served. Build for
that, or point the person to `scion tunnel` (below).

## Steps

1. Build the page as **one self-contained HTML file** (see Build settings).
2. Serve it on `127.0.0.1` on a port of 1024 or above, not 9810 or 18380, as
   `index.html` so the root URL opens it:
   `python3 -m http.server <port> --bind 127.0.0.1 --directory <folder>`
3. Run: `scion expose <port> --label "<what it is>"`
4. Verify it (see Verifying honestly).
5. Give the human the URL **exactly as printed**, including the trailing `/`.
   Do not hand out tokens, and do not build the URL by hand.

## What works when a person opens the link

- Works: one HTML file with its CSS in `<style>`, its JavaScript in `<script>`
  (no `src`), and images and fonts as `data:` URLs.
- Does not work: `<script src>`, `<link rel="stylesheet" href>`, `<img src>`
  pointing at your server, `fetch`, XHR, EventSource or WebSocket calls back
  to your app. This holds for relative URLs (`./app.js`) and for URLs that
  start with `/` alike.
- Does not work: localStorage, sessionStorage, IndexedDB, cookies, web workers
  and service workers.
- Does not work: dev servers with live reload (`vite dev`, `next dev`), and
  anything that needs a live backend.
- Does not work: client-side routing by path (React Router's
  `BrowserRouter`, Vue Router's `createWebHistory` and similar). Use hash
  routing instead (`HashRouter`, `createWebHashHistory`), or no router.
- Data the page needs must be inlined at build time (for example a
  `<script type="application/json">` block or a JS constant).

For a full multi-file app, a backend API or a dev server, the human can use
`scion tunnel` from their machine with the scion CLI, if their Hub has
`scion tunnel` turned on. Suggest:

    scion tunnel <agent> --port <port>

It opens the app on a local port with everything working. See
https://googlecloudplatform.github.io/scion/hosted/user/port-forwarding/#multi-file-apps

## Build settings (single-file output)

- **Vite** (React, Vue, Svelte, plain): add `vite-plugin-singlefile`
  (`npm i -D vite-plugin-singlefile`, then in `vite.config.*`
  `import { viteSingleFile } from 'vite-plugin-singlefile'` and
  `plugins: [..., viteSingleFile()]`) and run `vite build`. `dist/index.html` is the whole
  app: everything your code imports (CSS, images, fonts) is inlined. Serve
  `dist`. Files in `public/` that the page refers to by URL (for example
  `/favicon.svg` or `/data.json`) are NOT inlined and will not load through
  the link: import them from your code instead, or inline them by hand.
- **Plain HTML**: put the CSS in `<style>` and the JS in `<script>`. Turn
  images into data URLs, for example
  `echo "data:image/png;base64,$(base64 -w0 logo.png)"`.
- **Create React App, webpack, Parcel, esbuild**: none of them produce a
  single file without extra setup. The dependable route is to build the app
  with Vite and `vite-plugin-singlefile` (a React, Vue or Svelte app usually
  moves over with little change). For a small app, you can instead paste the
  built JS into a `<script>` and the CSS into a `<style>` in the HTML, turn
  images into data URLs, and check the result as below.
- **Next.js, server-rendered apps, apps with an API**: not supported through
  the link. Use `scion tunnel`, or produce a static single-file page with the
  data inlined.
- **Static servers**: `python3 -m http.server <port> --bind 127.0.0.1
  --directory <folder>` serves the folder as is. Any static server bound to
  `127.0.0.1` works; what matters is the single file it serves.

## Verifying honestly

- A `curl` with your own agent token is NOT proof. It only shows that the
  file is served; it does not behave like a person's browser and will succeed
  for pages that are broken for them.
- Search the built HTML for `src=`, `href=`, `url(`, `fetch(`, `XMLHttpRequest`,
  `EventSource`, `WebSocket`, `import(`, `localStorage`, `sessionStorage`,
  `indexedDB`, `document.cookie`, `new Worker`, `SharedWorker` and
  `serviceWorker`. Anything that points at your server, or uses browser
  storage, cookies or workers, will not work through the link. Ignore
  matches inside minified library code and links to other sites; look for
  URLs that point at your own server.
- Search your SOURCE (not the build) for `BrowserRouter`,
  `createBrowserRouter` and `createWebHistory`: any hit is path-based
  routing, which will not work through the link.
- These two searches are the only check for storage, cookies, workers and
  path-based routing: they work on `127.0.0.1`, so the browser check below
  does not catch them.
- Load the page in a headless browser (for example Playwright Chromium) from
  `http://127.0.0.1:<port>/`, with every request other than the page itself
  blocked and recorded. In Playwright:

  ```js
  const blocked = [];
  await page.route('**/*', route => {
    const req = route.request();
    if (req.isNavigationRequest() && req.frame() === page.mainFrame()) return route.continue();
    blocked.push(req.url());
    return route.abort();
  });
  const errors = [];
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', e => errors.push(String(e)));
  await page.goto('http://127.0.0.1:<port>/');
  // then: check what the page shows, and that blocked and errors are both empty
  ```

  Check that the page renders what it should, that `errors` is empty, and
  that `blocked` is empty. Anything in `blocked` is something the page loads
  from outside itself; inline it.
- Report what you verified and how (which checks, which browser). If you
  could not run a browser, say so plainly instead of reporting the page as
  working.
