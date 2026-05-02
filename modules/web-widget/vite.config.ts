import path from 'node:path'
import preact from '@preact/preset-vite'
import { defineConfig } from 'vite'

// IIFE bundle that mounts a Shadow DOM and runs without Vite's runtime
// helpers. The output goes straight into internal/widget/dist/ where
// embed.go picks it up.
export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: path.resolve(__dirname, '../operator/internal/widget/dist'),
    emptyOutDir: true,
    cssCodeSplit: false,
    sourcemap: false,
    target: 'es2019',
    rollupOptions: {
      input: path.resolve(__dirname, 'src/main.tsx'),
      output: {
        format: 'iife',
        name: 'RoutePrismWidget',
        entryFileNames: 'widget.js',
        assetFileNames: (info) => {
          if (info.name && info.name.endsWith('.css')) return 'widget.css'
          return 'assets/[name]-[hash][extname]'
        },
        inlineDynamicImports: true,
      },
    },
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8082',
    },
  },
})
