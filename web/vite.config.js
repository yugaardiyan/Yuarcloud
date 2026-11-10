import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
import { readFileSync } from 'node:fs'

// The version the Settings footer names. package.json is the single source of
// truth, so a plain `npm run build` on a laptop shows the same number as the
// published image. The publish workflow may still override it with the pushed
// tag through the Dockerfile's APP_VERSION build arg.
const pkgVersion = JSON.parse(
  readFileSync(new URL('./package.json', import.meta.url), 'utf8')
).version
const appVersion = process.env.APP_VERSION || `v${pkgVersion}`

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  // Replaced as a literal at build time. Declared here rather than imported
  // from a module so nothing ships a copy of package.json to the browser.
  define: {
    __APP_VERSION__: JSON.stringify(appVersion),
  },
  build: {
    outDir: 'dist',
    // Off on purpose. The maps were 4.3 MB of this build's 5.5 MB, and the
    // server hands out everything under /assets — so they were not only wasted
    // bytes but a public copy of the source: 159 files with sourcesContent
    // intact, fetched from any instance with a single request. Turn it on
    // locally when a stack trace needs to be read against real code.
    sourcemap: false,
    // Note: intentionally no manualChunks. Splitting react / react-dom into
    // separate chunks reorders module initialisation and throws
    // "TypeError: n is not a function" at render time. Rolldown's automatic
    // chunking is fast enough at this bundle size.
  },
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:3103',
        changeOrigin: true,
      },
    },
  },
})
