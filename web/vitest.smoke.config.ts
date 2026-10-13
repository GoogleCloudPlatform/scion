import { configDefaults, defineConfig } from 'vitest/config';
import base from './vitest.config';

/**
 * On-demand smoke checks (`npm run test:terminal-smoke`): the base test
 * configuration, limited to the `*.smoke.test.ts` files that `npm test`
 * excludes.
 */
export default defineConfig({
  ...base,
  test: {
    ...base.test,
    include: ['src/**/*.smoke.test.ts'],
    exclude: [...configDefaults.exclude],
  },
});
