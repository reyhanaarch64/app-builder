const CACHE_NAME = `app-builder-__CACHE_VERSION__`
const PRECACHE_URLS = __PRECACHE_URLS__

self.addEventListener('install', event => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then(cache => cache.addAll(PRECACHE_URLS))
      .then(() => self.skipWaiting()),
  )
})

self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(
        keys
          .filter(key => key.startsWith('app-builder-') && key !== CACHE_NAME)
          .map(key => caches.delete(key)),
      ))
      .then(() => self.clients.claim()),
  )
})

self.addEventListener('push', event => {
  let payload = {}
  try {
    payload = event.data ? event.data.json() : {}
  } catch {
    payload = { body: event.data?.text() ?? '' }
  }
  const title = typeof payload.title === 'string' ? payload.title : 'App Builder'
  const body = typeof payload.body === 'string' ? payload.body : 'status build diperbarui.'
  const tag = typeof payload.tag === 'string' ? payload.tag : 'build-notification'
  event.waitUntil(self.registration.showNotification(title, {
    body,
    tag,
    icon: '/icons/app-192.png',
    badge: '/icons/app-192.png',
    data: { url: typeof payload.url === 'string' ? payload.url : '/' },
  }))
})

self.addEventListener('notificationclick', event => {
  event.notification.close()
  event.waitUntil((async () => {
    let target = new URL('/', self.location.origin)
    try {
      const requested = new URL(event.notification.data?.url ?? '/', self.location.origin)
      if (requested.origin === self.location.origin && requested.pathname.startsWith('/')) target = requested
    } catch {
      target = new URL('/', self.location.origin)
    }
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
    for (const client of windows) {
      if (new URL(client.url).origin === self.location.origin) {
        await client.navigate(target.href)
        return client.focus()
      }
    }
    return self.clients.openWindow(target.href)
  })())
})

self.addEventListener('fetch', event => {
  const { request } = event
  if (request.method !== 'GET') return

  const url = new URL(request.url)
  if (url.origin !== self.location.origin) return

  if (url.pathname.startsWith('/api/')) {
    event.respondWith(fetch(request))
    return
  }

  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).catch(async () => {
        const cache = await caches.open(CACHE_NAME)
        const cached = await cache.match('/')
        if (cached) return cached
        return Response.error()
      }),
    )
    return
  }

  event.respondWith(
    caches.match(request).then(cached => cached || fetch(request)),
  )
})
