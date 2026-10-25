import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { defineConfig } from 'vite'
import path from 'node:path'

const api = `http://localhost:${process.env.AOC_UI_PORT ?? '4100'}`

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': path.resolve(import.meta.dirname, 'src') } },
  server: {
    port: 5173,
    proxy: { '/api': { target: api, changeOrigin: true } },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
