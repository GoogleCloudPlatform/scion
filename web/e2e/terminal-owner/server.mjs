import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';

// Serve only these fixture assets; never proxy to a Hub or broker.
const assets = new Map([
  ['/', ['index.html', 'text/html']],
  ['/coordinator.js', ['coordinator.js', 'text/javascript']],
]);
const server = createServer(async (request, response) => {
  const asset = assets.get(new URL(request.url, 'http://localhost').pathname);
  if (!asset) {
    response.writeHead(404).end();
    return;
  }
  try {
    const body = await readFile(new URL(asset[0], import.meta.url));
    response.writeHead(200, { 'Content-Type': asset[1], 'Cache-Control': 'no-store' });
    response.end(body);
  } catch {
    response.writeHead(500).end('Fixture asset unavailable');
  }
});
server.listen(Number(process.env.TERMINAL_OWNER_PORT || 4517), '127.0.0.1');
