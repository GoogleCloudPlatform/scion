// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The port proxy suite's fixture app: the files in ./fixture, an 8x8 PNG
// built from bytes at /img/dot.png, a JSON answer at /api/ping and a
// WebSocket echo at /ws. It listens on 127.0.0.1 (the port the agent
// registers) and writes {"port": N} to the ready file once listening.
//
// Usage: node serve-fixture.mjs <ready-file>

import { createHash } from 'node:crypto';
import * as fs from 'node:fs';
import * as http from 'node:http';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';
import * as zlib from 'node:zlib';

const FIXTURE_DIR = path.join(path.dirname(fileURLToPath(import.meta.url)), 'fixture');
const WS_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
};

/** CRC-32 (IEEE), table-driven; zlib.crc32 needs Node 20.15 or 22.2. */
const CRC_TABLE = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(buf) {
  let c = 0xffffffff;
  for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

/** An 8x8 RGB PNG of one color, built from bytes. */
function dotPNG() {
  const chunk = (type, data) => {
    const len = Buffer.alloc(4);
    len.writeUInt32BE(data.length);
    const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body));
    return Buffer.concat([len, body, crc]);
  };
  const size = 8;
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // color type: RGB
  const row = Buffer.concat([Buffer.from([0]), Buffer.alloc(size * 3, Buffer.from([200, 30, 30]))]);
  const raw = Buffer.concat(Array.from({ length: size }, () => row));
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

const PNG = dotPNG();

function send(res, status, type, body) {
  res.writeHead(status, { 'Content-Type': type, 'Cache-Control': 'no-store' });
  res.end(body);
}

const server = http.createServer((req, res) => {
  const { pathname } = new URL(req.url ?? '/', 'http://fixture');
  if (pathname === '/api/ping') {
    send(res, 200, 'application/json', JSON.stringify({ pong: true }));
    return;
  }
  if (pathname === '/img/dot.png') {
    send(res, 200, 'image/png', PNG);
    return;
  }
  const name = pathname === '/' ? 'index.html' : pathname.slice(1);
  // Only the flat file names in ./fixture are served.
  if (!/^[a-z]+\.(html|js|css)$/.test(name)) {
    send(res, 404, 'text/plain; charset=utf-8', 'not found\n');
    return;
  }
  const file = path.join(FIXTURE_DIR, name);
  fs.readFile(file, (err, data) => {
    if (err) {
      send(res, 404, 'text/plain; charset=utf-8', 'not found\n');
      return;
    }
    send(res, 200, TYPES[path.extname(name)], data);
  });
});

// A minimal WebSocket echo (RFC 6455): unfragmented text frames up to
// 64 KiB from the client are echoed back; a close frame is answered.
server.on('upgrade', (req, socket) => {
  const { pathname } = new URL(req.url ?? '/', 'http://fixture');
  const key = req.headers['sec-websocket-key'];
  if (pathname !== '/ws' || typeof key !== 'string') {
    socket.end('HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n');
    return;
  }
  const accept = createHash('sha1')
    .update(key + WS_GUID)
    .digest('base64');
  socket.write(
    'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n' +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
  );
  let buf = Buffer.alloc(0);
  socket.on('data', (data) => {
    buf = Buffer.concat([buf, data]);
    for (;;) {
      if (buf.length < 2) return;
      const opcode = buf[0] & 0x0f;
      const masked = (buf[1] & 0x80) !== 0;
      let len = buf[1] & 0x7f;
      let off = 2;
      if (len === 126) {
        if (buf.length < 4) return;
        len = buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        socket.destroy();
        return;
      }
      const maskLen = masked ? 4 : 0;
      if (buf.length < off + maskLen + len) return;
      const mask = masked ? buf.subarray(off, off + 4) : null;
      const payload = Buffer.from(buf.subarray(off + maskLen, off + maskLen + len));
      if (mask) for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4];
      buf = buf.subarray(off + maskLen + len);
      if (opcode === 0x8) {
        socket.end(Buffer.from([0x88, 0x00]));
        return;
      }
      if (opcode === 0x1) {
        const header =
          payload.length < 126
            ? Buffer.from([0x81, payload.length])
            : Buffer.from([0x81, 126, payload.length >> 8, payload.length & 0xff]);
        socket.write(Buffer.concat([header, payload]));
      }
    }
  });
  socket.on('error', () => socket.destroy());
});

const readyFile = process.argv[2];
if (!readyFile) {
  console.error('usage: node serve-fixture.mjs <ready-file>');
  process.exit(2);
}
server.listen(0, '127.0.0.1', () => {
  const { port } = server.address();
  fs.writeFileSync(`${readyFile}.tmp`, JSON.stringify({ port }), { mode: 0o600 });
  fs.renameSync(`${readyFile}.tmp`, readyFile);
  console.info(`[fixture] listening on 127.0.0.1:${port}`);
});
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.once(signal, () => {
    server.close();
    process.exit(0);
  });
}
