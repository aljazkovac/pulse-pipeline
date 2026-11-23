import { fileURLToPath } from 'node:url';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';
import { apiProxyTarget } from './config/devProxy.ts';

// The repository root holds the shared .env (API_PORT etc.); see .env.example.
const repoRoot = fileURLToPath(new URL('..', import.meta.url));

// ADR-0005 §4: PULSE_PUBLIC_URL is the Vite origin in development, so the port is fixed.
const DEV_PORT = 5173;

export default defineConfig(({ mode }) => {
  // Config-only read of the root .env; nothing here reaches the client bundle
  // (only VITE_-prefixed variables from web/ are exposed via import.meta.env).
  const env = { ...loadEnv(mode, repoRoot, ''), ...process.env };
  const proxy = {
    '/api': {
      target: apiProxyTarget(env),
      // Keep the browser's Host header so the API sees one origin: its session
      // cookies and http.CrossOriginProtection both expect localhost:5173.
      changeOrigin: false,
    },
  };

  return {
    plugins: [react(), tailwindcss()],
    server: { port: DEV_PORT, strictPort: true, proxy },
    preview: { port: DEV_PORT, strictPort: true, proxy },
    test: {
      environment: 'jsdom',
      include: ['src/**/*.test.{ts,tsx}', 'config/**/*.test.ts'],
      setupFiles: ['./src/test/setup.ts'],
      restoreMocks: true,
    },
  };
});
