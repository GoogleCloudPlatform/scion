import { defineConfig } from 'vitest/config';
import { resolve } from 'path';

export default defineConfig({
  resolve: {
    alias: {
      '@': resolve(__dirname, 'src'),
    },
  },
  esbuild: {
    target: 'esnext',
  },
  test: {
    environment: 'happy-dom',
    include: ['src/**/*.test.ts'],
    setupFiles: ['./vitest.setup.ts'],
    // Pin the process timezone so tests are deterministic regardless of the
    // CI host's or developer's ambient TZ (tz-refactor task 11). Explicitly
    // overrides any `TZ` already set in the invoking shell, so running with
    // e.g. `TZ=Asia/Tokyo npx vitest run` exercises the exact same pinned
    // zone here — time.ts tests that need a specific zone pass it explicitly
    // to the function under test (`formatInstant`, `parseWallClock`, etc.)
    // rather than relying on this value, which only backs `browserTimeZone()`
    // and anything that falls back to it.
    env: {
      TZ: 'UTC',
    },
    // Belt-and-braces guard for a known intermittent CI failure: a
    // stray console call that lands after a worker starts tearing down can
    // leave the "onUserConsoleLog" RPC pending when the worker's channel
    // closes, which Vitest reports as an unhandled EnvironmentTeardownError
    // even though every test passed. This is raised by the runner when it
    // rejects pending RPC calls on worker teardown, not in the worker's own
    // process — a filter in vitest.setup.ts (which only runs inside the
    // worker) cannot see it, so it has to live here instead.
    //
    // This narrowly matches ONLY that exact error and lets every other
    // unhandled error and rejection fail the run as normal.
    //
    // A non-false return (including falling off the end of this function)
    // lets Vitest report the error and fail the run normally.
    onUnhandledError(error) {
      if (
        error?.name === 'EnvironmentTeardownError' &&
        typeof error?.message === 'string' &&
        error.message.includes('onUserConsoleLog')
      ) {
        return false;
      }
    },
  },
});
