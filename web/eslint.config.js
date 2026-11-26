import js from '@eslint/js';
import { defineConfig, globalIgnores } from 'eslint/config';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default defineConfig([
  // src/api/schema.d.ts is generated from backend/api/openapi.yaml (npm run generate:api).
  globalIgnores(['dist', 'playwright-report', 'test-results', 'blob-report', 'src/api/schema.d.ts']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.strictTypeChecked,
      tseslint.configs.stylisticTypeChecked,
    ],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      // AGENTS.md §1.3: never `any`. Already in strictTypeChecked; kept explicit so a
      // preset change cannot silently drop it.
      '@typescript-eslint/no-explicit-any': 'error',
    },
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    extends: [reactHooks.configs.flat.recommended, reactRefresh.configs.vite],
    languageOptions: { globals: globals.browser },
  },
  {
    files: ['*.config.ts', 'config/**/*.ts', 'e2e/**/*.ts'],
    languageOptions: { globals: globals.node },
  },
  {
    // Playwright's fixture runner requires the first parameter to be `{}` when a
    // fixture depends on `testInfo` only.
    files: ['e2e/**/*.ts'],
    rules: { 'no-empty-pattern': 'off' },
  },
]);
