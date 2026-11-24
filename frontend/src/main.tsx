import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { loader } from '@monaco-editor/react'
import './index.css'
import App from './App.tsx'

loader.config({ paths: { vs: '/monaco/vs' } })

if ('serviceWorker' in navigator) {
  navigator.serviceWorker.register('/sw.js').catch(error => {
    window.dispatchEvent(new CustomEvent('ide:pwa-error', {
      detail: error instanceof Error ? error.message : 'service worker gagal didaftarkan',
    }))
  })
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
