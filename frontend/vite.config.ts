import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'
import { readFileSync, readdirSync } from 'node:fs'
import { resolve, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

function offlineAssets(): Plugin {
  const projectRoot = fileURLToPath(new URL('.', import.meta.url))
  const monacoRoot = resolve(projectRoot, 'node_modules/monaco-editor/min/vs')
  const serviceWorkerTemplate = resolve(projectRoot, 'pwa/sw-template.js')
  const monacoFiles: string[] = []

  const walk = (directory: string) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = resolve(directory, entry.name)
      if (entry.isDirectory()) walk(path)
      else if (entry.isFile() && !path.endsWith('.d.ts')) monacoFiles.push(path)
    }
  }

  return {
    name: 'offline-assets',
    apply: 'build' as const,
    generateBundle(_options, bundle) {
      walk(monacoRoot)
      const monacoUrls = monacoFiles.map(path => {
        const relativePath = relative(monacoRoot, path).split(sep).join('/')
        this.emitFile({
          type: 'asset',
          fileName: `monaco/vs/${relativePath}`,
          source: readFileSync(path),
        })
        return `/monaco/vs/${relativePath}`
      })
      const localUrls = Object.keys(bundle).map(path => `/${path}`)
      const precache = [...new Set([
        '/',
        '/manifest.webmanifest',
        '/icons/app-192.png',
        '/icons/app-512.png',
        '/icons/app.svg',
        ...localUrls,
        ...monacoUrls,
      ])]
      const template = readFileSync(serviceWorkerTemplate, 'utf8')
      this.emitFile({
        type: 'asset',
        fileName: 'sw.js',
        source: template.replace('__PRECACHE_URLS__', JSON.stringify(precache))
          .replace('__CACHE_VERSION__', String(Date.now())),
      })
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), offlineAssets()],
})
