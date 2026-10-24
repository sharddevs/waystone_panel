// SPDX-License-Identifier: AGPL-3.0-only
/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The build goes into internal/webui/dist, which the controller embeds
// with -tags ui (scope §6). It is never committed.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    // Never inline a file as a data: URI: the CSP allows fonts and
    // scripts only from 'self', and every file should be a hashed asset.
    assetsInlineLimit: 0,
    // The modulepreload polyfill is an inline script, which the CSP
    // forbids. Every browser Waystone supports has modulepreload.
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
})
