import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { loader } from '@monaco-editor/react'
import './index.css'
import App from './App.tsx'

loader.config({ paths: { vs: '/monaco/vs' } })

if ('serviceWorker' in navigator) {
  const serviceWorker = navigator.serviceWorker
  let hasController = Boolean(serviceWorker.controller)
  let isReloading = false

  serviceWorker.addEventListener('controllerchange', () => {
    if (!hasController) {
      hasController = true
      return
    }
    if (isReloading) return
    isReloading = true
    window.location.reload()
  })

  serviceWorker.register('/sw.js', { updateViaCache: 'none' })
    .then(registration => {
      let updateCheckInProgress = false
      const checkForUpdates = () => {
        if (!navigator.onLine || document.visibilityState !== 'visible' || updateCheckInProgress) return
        updateCheckInProgress = true
        void registration.update()
          .catch(error => console.error('gagal memeriksa update PWA:', error))
          .finally(() => { updateCheckInProgress = false })
      }

      checkForUpdates()
      window.addEventListener('focus', checkForUpdates)
      window.addEventListener('online', checkForUpdates)
      document.addEventListener('visibilitychange', checkForUpdates)
      window.setInterval(checkForUpdates, 5 * 60 * 1000)
    })
    .catch(error => {
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
