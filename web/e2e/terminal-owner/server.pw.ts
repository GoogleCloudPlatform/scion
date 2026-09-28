import { connect } from 'node:net';
import { expect, test } from '@playwright/test';

test('malformed absolute request returns 400 and leaves the server healthy', async ({
  baseURL,
  request,
}) => {
  const endpoint = new URL(baseURL!);
  const response = await new Promise<string>((resolve, reject) => {
    const socket = connect(Number(endpoint.port), endpoint.hostname);
    let received = '';
    socket.setEncoding('utf8');
    socket.setTimeout(2000, () => socket.destroy(new Error('Raw HTTP response timed out')));
    socket.on('connect', () => {
      socket.write('GET http://[ HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n');
    });
    socket.on('data', (chunk) => {
      received += chunk;
    });
    socket.on('end', () => resolve(received));
    socket.on('error', reject);
  });
  expect(response).toMatch(/^HTTP\/1\.1 400 /);
  const healthy = await request.get('/');
  expect(healthy.status()).toBe(200);
  expect(await healthy.text()).toContain('Terminal owner fixture');
});
