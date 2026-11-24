import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import Editor from '@monaco-editor/react'
import Dexie, { type Table } from 'dexie'
import JSZip from 'jszip'
import {
  Archive, Ban, Bell, Box, ChevronRight, Copy, Download, FileCode2, FilePlus2, FolderPlus, HardDrive,
  LogOut, Menu, PackagePlus, Play, RefreshCw, Search, Smartphone, Trash2, Upload, WifiOff,
  X,
} from 'lucide-react'
import './App.css'

type Project = {
  id: string
  name: string
  packageName: string
  language: 'kotlin' | 'java'
  templateId: string
  minSdk: number
  targetSdk: number
  createdAt: number
  updatedAt: number
}

type ProjectFile = {
  id: string
  projectId: string
  path: string
  type: 'file' | 'directory'
  content?: string
  binary?: Blob
  sha256?: string
  size: number
  createdAt: number
  updatedAt: number
}

type Build = {
  id: string
  projectId: string
  status: 'preparing' | 'assembling' | 'queued' | 'building' | 'success' | 'failed' | 'canceled'
  variant: string
  createdAt: string
  error?: string
  artifact?: string
}

type APKArtifact = {
  buildId: string
  projectId: string
  variant: string
  createdAt: string
  filename: string
  size: number
  available: boolean
}

type StorageStats = {
  workspaceBytes: number
  artifactBytes: number
  blobBytes: number
  databaseBytes: number
  gradleCacheBytes: number
  buildHistory: number
  activeBuilds: number
}

type CleanupOptions = {
  workspaces: boolean
  artifacts: boolean
  history: boolean
  gradleCache: boolean
}

type AuthStatus = { mode: 'public' | 'private'; authenticated: boolean }
type NotificationSettings = {
  recipient: string
  notifySuccess: boolean
  notifyFailure: boolean
  smtpConfigured: boolean
}
type PushSubscriptionStatus = { subscribed: boolean }
type InstallPromptEvent = Event & {
  prompt: () => Promise<void>
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed'; platform: string }>
}

type ManifestFile = {
  path: string
  kind: 'text' | 'binary'
  size: number
  mimeType: string
  sha256: string
  content?: string
}

type PrepareResponse = { build: Build; missingBlobs: string[]; error?: string }

type ProjectEditorState = {
  openFileIds: string[]
  activeFileId: string | null
  expanded: string[]
}

const editorStateKey = (projectId: string) => `android-web-ide.editorState.${projectId}`
const accessModeKey = 'android-web-ide.accessMode'
const offlineUnlockKey = 'android-web-ide.offlineUnlock'
const defaultExpandedPaths = ['app', 'app/src', 'app/src/main']

function readProjectEditorState(projectId: string): ProjectEditorState | null {
  try {
    const saved = localStorage.getItem(editorStateKey(projectId))
    if (!saved) return null
    const state = JSON.parse(saved) as Partial<ProjectEditorState>
    if (!Array.isArray(state.openFileIds) ||
        !state.openFileIds.every(id => typeof id === 'string') ||
        !(state.activeFileId === null || typeof state.activeFileId === 'string') ||
        !Array.isArray(state.expanded) ||
        !state.expanded.every(path => typeof path === 'string')) return null
    return {
      openFileIds: state.openFileIds,
      activeFileId: state.activeFileId,
      expanded: state.expanded,
    }
  } catch {
    return null
  }
}

class IDEDatabase extends Dexie {
  projects!: Table<Project, string>
  files!: Table<ProjectFile, string>
  constructor() {
    super('AndroidWebIDE')
    this.version(1).stores({
      projects: 'id, name, updatedAt',
      files: 'id, projectId, path, updatedAt',
    })
  }
}

const db = new IDEDatabase()

const uid = () => crypto.randomUUID()
const packagePath = (packageName: string) => packageName.replace(/\./g, '/')

function emptyTemplate(project: Project): ProjectFile[] {
  const now = Date.now()
  const sourcePath = project.language === 'kotlin'
    ? `app/src/main/java/${packagePath(project.packageName)}/MainActivity.kt`
    : `app/src/main/java/${packagePath(project.packageName)}/MainActivity.java`
  const kt = `package ${project.packageName}\n\nimport android.os.Bundle\nimport androidx.appcompat.app.AppCompatActivity\n\nclass MainActivity : AppCompatActivity() {\n    override fun onCreate(savedInstanceState: Bundle?) {\n        super.onCreate(savedInstanceState)\n        setContentView(R.layout.activity_main)\n    }\n}\n`
  const java = `package ${project.packageName};\n\nimport android.os.Bundle;\nimport androidx.appcompat.app.AppCompatActivity;\n\npublic class MainActivity extends AppCompatActivity {\n    @Override\n    protected void onCreate(Bundle savedInstanceState) {\n        super.onCreate(savedInstanceState);\n        setContentView(R.layout.activity_main);\n    }\n}\n`
  const textFiles: Record<string, string> = {
    'settings.gradle.kts': `pluginManagement {\n    repositories {\n        google()\n        mavenCentral()\n        gradlePluginPortal()\n    }\n}\ndependencyResolutionManagement {\n    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)\n    repositories {\n        google()\n        mavenCentral()\n    }\n}\nrootProject.name = "${project.name}\"\ninclude(":app")\n`,
    'build.gradle.kts': `plugins {\n    id("com.android.application") version "8.7.3" apply false\n    ${project.language === 'kotlin' ? 'id("org.jetbrains.kotlin.android") version "2.0.21" apply false' : ''}\n}\n`,
    'gradle.properties': `org.gradle.jvmargs=-Xmx2048m\nandroid.useAndroidX=true\nkotlin.code.style=official\n`,
    'app/build.gradle.kts': `plugins {\n    id("com.android.application")${project.language === 'kotlin' ? '\n    id("org.jetbrains.kotlin.android")' : ''}\n}\n\nandroid {\n    namespace = "${project.packageName}"\n    compileSdk = ${project.targetSdk}\n\n    compileOptions {\n        sourceCompatibility = JavaVersion.VERSION_17\n        targetCompatibility = JavaVersion.VERSION_17\n    }\n${project.language === 'kotlin' ? '\n    kotlinOptions {\n        jvmTarget = "17"\n    }\n' : ''}\n    defaultConfig {\n        applicationId = "${project.packageName}"\n        minSdk = ${project.minSdk}\n        targetSdk = ${project.targetSdk}\n        versionCode = 1\n        versionName = "1.0"\n    }\n\n    buildTypes {\n        release {\n            isMinifyEnabled = false\n        }\n    }\n}\n\ndependencies {\n    implementation("androidx.core:core-ktx:1.13.1")\n    implementation("androidx.appcompat:appcompat:1.7.0")\n    implementation("com.google.android.material:material:1.12.0")\n    implementation("androidx.constraintlayout:constraintlayout:2.1.4")\n}\n\nconfigurations.all {\n    exclude(group = "org.jetbrains.kotlin", module = "kotlin-stdlib-jdk7")\n    exclude(group = "org.jetbrains.kotlin", module = "kotlin-stdlib-jdk8")\n}\n`,
    'app/src/main/AndroidManifest.xml': `<?xml version="1.0" encoding="utf-8"?>\n<manifest xmlns:android="http://schemas.android.com/apk/res/android">\n    <application\n        android:allowBackup="true"\n        android:label="@string/app_name"\n        android:theme="@style/Theme.MyApp">\n        <activity android:name=".MainActivity" android:exported="true">\n            <intent-filter>\n                <action android:name="android.intent.action.MAIN"/>\n                <category android:name="android.intent.category.LAUNCHER"/>\n            </intent-filter>\n        </activity>\n    </application>\n</manifest>\n`,
    'app/src/main/res/layout/activity_main.xml': `<?xml version="1.0" encoding="utf-8"?>\n<androidx.constraintlayout.widget.ConstraintLayout xmlns:android="http://schemas.android.com/apk/res/android" xmlns:app="http://schemas.android.com/apk/res-auto" android:layout_width="match_parent" android:layout_height="match_parent">\n    <TextView android:id="@+id/hello_text" android:layout_width="wrap_content" android:layout_height="wrap_content" android:text="@string/hello_world" android:textSize="24sp" app:layout_constraintBottom_toBottomOf="parent" app:layout_constraintEnd_toEndOf="parent" app:layout_constraintStart_toStartOf="parent" app:layout_constraintTop_toTopOf="parent"/>\n</androidx.constraintlayout.widget.ConstraintLayout>\n`,
    'app/src/main/res/values/strings.xml': `<resources>\n    <string name="app_name">${project.name}</string>\n    <string name="hello_world">Hello, World!</string>\n</resources>\n`,
    'app/src/main/res/values/themes.xml': `<resources>\n    <style name="Theme.MyApp" parent="Theme.Material3.DayNight.NoActionBar"/>\n</resources>\n`,
    [sourcePath]: project.language === 'kotlin' ? kt : java,
  }
  return Object.entries(textFiles).map(([path, content]) => ({
    id: uid(), projectId: project.id, path, type: 'file', content,
    size: new Blob([content]).size, createdAt: now, updatedAt: now,
  }))
}

type TreeNode = { name: string; path: string; type: 'file' | 'directory'; children: TreeNode[] }

function buildTree(files: ProjectFile[]): TreeNode[] {
  const root: TreeNode[] = []
  const dirs = new Map<string, TreeNode>()
  for (const file of files) {
    const parts = file.path.split('/')
    let parent = root
    let current = ''
    for (let i = 0; i < parts.length; i++) {
      const name = parts[i]
      current = current ? `${current}/${name}` : name
      if (i === parts.length - 1) {
        parent.push({ name, path: file.path, type: 'file', children: [] })
      } else {
        let node = dirs.get(current)
        if (!node) {
          node = { name, path: current, type: 'directory', children: [] }
          dirs.set(current, node)
          parent.push(node)
        }
        parent = node.children
      }
    }
  }
  const sort = (nodes: TreeNode[]) => nodes.sort((a, b) =>
    a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'directory' ? -1 : 1)
  for (const node of root) if (node.type === 'directory') sort(node.children)
  sort(root)
  return root
}

function validateBuildProject(project: Project, files: ProjectFile[]): string | null {
  if (!/^([a-zA-Z_]\w*\.)+[a-zA-Z_]\w*$/.test(project.packageName)) return 'Package name tidak valid'
  if (!Number.isInteger(project.minSdk) || !Number.isInteger(project.targetSdk) ||
      project.minSdk < 1 || project.targetSdk < project.minSdk) return 'Rentang Min SDK dan Target SDK tidak valid'
  if (files.length === 0) return 'Project belum memiliki file'
  const paths = new Set<string>()
  for (const file of files) {
    if (!file.path || file.path.startsWith('/') || file.path.includes('\\') ||
        file.path.split('/').some(part => !part || part === '.' || part === '..')) {
      return `Path file tidak valid: ${file.path}`
    }
    if (paths.has(file.path)) return `Path file duplikat: ${file.path}`
    paths.add(file.path)
  }
  const settings = files.find(file => /(^|\/)settings\.gradle(\.kts)?$/.test(file.path))
  if (!settings) return 'settings.gradle atau settings.gradle.kts tidak ditemukan'
  const root = settings.path.includes('/') ? `${settings.path.slice(0, settings.path.lastIndexOf('/') + 1)}` : ''
  if (!paths.has(`${root}app/build.gradle`) && !paths.has(`${root}app/build.gradle.kts`)) {
    return 'Module app/build.gradle atau app/build.gradle.kts tidak ditemukan'
  }
  if (!paths.has(`${root}app/src/main/AndroidManifest.xml`)) {
    return 'app/src/main/AndroidManifest.xml tidak ditemukan'
  }
  if (files.length > 20000) return 'Project memiliki lebih dari 20.000 file'
  return null
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value.toFixed(1)} ${units[unit]}`
}

function decodeVapidPublicKey(key: string): ArrayBuffer {
  const base64 = key.replace(/-/g, '+').replace(/_/g, '/')
  const raw = atob(base64.padEnd(Math.ceil(base64.length / 4) * 4, '='))
  const bytes = new Uint8Array(new ArrayBuffer(raw.length))
  for (let index = 0; index < raw.length; index++) bytes[index] = raw.charCodeAt(index)
  return bytes.buffer
}

function throwIfBuildAborted(signal: AbortSignal) {
  if (!signal.aborted) return
  const error = new Error('Build dibatalkan')
  error.name = 'AbortError'
  throw error
}

async function createBuildManifest(files: ProjectFile[], signal: AbortSignal): Promise<ManifestFile[]> {
  const encoder = new TextEncoder()
  const manifest: ManifestFile[] = []
  for (const file of files) {
    throwIfBuildAborted(signal)
    if (file.type !== 'file') continue
    const kind = file.binary ? 'binary' : 'text'
    const content = file.content ?? ''
    const size = file.binary?.size ?? encoder.encode(content).byteLength
    const cachedHash = file.sha256 === ''
      ? undefined
      : file.sha256 ?? (await db.files.get(file.id))?.sha256
    throwIfBuildAborted(signal)
    let hash = cachedHash
    if (!hash) {
      const bytes = file.binary
        ? new Uint8Array(await file.binary.arrayBuffer())
        : encoder.encode(content)
      throwIfBuildAborted(signal)
      hash = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)))
        .map(value => value.toString(16).padStart(2, '0')).join('')
    }
    throwIfBuildAborted(signal)
    const updated = file.sha256 ? file : { ...file, sha256: hash }
    if (!cachedHash) await db.files.put(updated)
    manifest.push({
      path: file.path,
      kind,
      size,
      mimeType: kind === 'binary' ? file.binary?.type || 'application/octet-stream' : 'text/plain; charset=utf-8',
      sha256: hash,
      ...(kind === 'text' ? { content } : {}),
    })
  }
  return manifest
}

async function ensureGeneratedJvmTargets(project: Project, files: ProjectFile[]): Promise<ProjectFile[]> {
  if (project.templateId !== 'empty-app') return files
  const moduleFile = files.find(file => /(^|\/)app\/build\.gradle\.kts$/.test(file.path))
  if (!moduleFile?.content || moduleFile.content.includes('sourceCompatibility') ||
      !moduleFile.content.includes('android {') || !moduleFile.content.includes('id("com.android.application")')) {
    return files
  }
  const compileSdk = /^([ \t]*)compileSdk\s*=\s*\d+\s*$/m.exec(moduleFile.content)
  if (!compileSdk) return files
  const indent = compileSdk[1]
  const kotlinOptions = project.language === 'kotlin'
    ? `\n\n${indent}kotlinOptions {\n${indent}    jvmTarget = "17"\n${indent}}`
    : ''
  const compatibility = `\n\n${indent}compileOptions {\n${indent}    sourceCompatibility = JavaVersion.VERSION_17\n${indent}    targetCompatibility = JavaVersion.VERSION_17\n${indent}}${kotlinOptions}`
  const updated = {
    ...moduleFile,
    content: moduleFile.content.replace(compileSdk[0], `${compileSdk[0]}${compatibility}`),
    sha256: '',
    updatedAt: Date.now(),
  }
  updated.size = new Blob([updated.content]).size
  await db.files.put(updated)
  return files.map(file => file.id === updated.id ? updated : file)
}

function App() {
  const [projects, setProjects] = useState<Project[]>([])
  const [activeProjectId, setActiveProjectId] = useState<string | null>(null)
  const [files, setFiles] = useState<ProjectFile[]>([])
  const [openTabs, setOpenTabs] = useState<ProjectFile[]>([])
  const [activeFileId, setActiveFileId] = useState<string | null>(null)
  const [editorStateProjectId, setEditorStateProjectId] = useState<string | null>(null)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [buildOpen, setBuildOpen] = useState(false)
  const [buildStatus, setBuildStatus] = useState<Build['status'] | 'idle'>('idle')
  const [buildCanceling, setBuildCanceling] = useState(false)
  const [logs, setLogs] = useState<string[]>([])
  const [buildId, setBuildId] = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [search, setSearch] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set(defaultExpandedPaths))
  const [toast, setToast] = useState('')
  const [storageOpen, setStorageOpen] = useState(false)
  const [storageLoading, setStorageLoading] = useState(false)
  const [storageStats, setStorageStats] = useState<StorageStats | null>(null)
  const [artifactsOpen, setArtifactsOpen] = useState(false)
  const [artifacts, setArtifacts] = useState<APKArtifact[]>([])
  const [artifactsLoading, setArtifactsLoading] = useState(false)
  const [artifactDeletingId, setArtifactDeletingId] = useState<string | null>(null)
  const [artifactsError, setArtifactsError] = useState('')
  const [notificationsOpen, setNotificationsOpen] = useState(false)
  const [notificationSettings, setNotificationSettings] = useState<NotificationSettings | null>(null)
  const [notificationLoading, setNotificationLoading] = useState(false)
  const [notificationSaving, setNotificationSaving] = useState(false)
  const [notificationTesting, setNotificationTesting] = useState(false)
  const [notificationError, setNotificationError] = useState('')
  const [pushSubscribed, setPushSubscribed] = useState(false)
  const [pushPermission, setPushPermission] = useState<NotificationPermission>('default')
  const [pushBusy, setPushBusy] = useState(false)
  const [authStatus, setAuthStatus] = useState<AuthStatus | null>(null)
  const [loginPassword, setLoginPassword] = useState('')
  const [loginError, setLoginError] = useState('')
  const [loginLoading, setLoginLoading] = useState(false)
  const [isOnline, setIsOnline] = useState(navigator.onLine)
  const [serverAvailable, setServerAvailable] = useState(true)
  const [offlineUnlocked, setOfflineUnlocked] = useState(() => localStorage.getItem(offlineUnlockKey) === 'true')
  const [installPrompt, setInstallPrompt] = useState<InstallPromptEvent | null>(null)
  const [isInstalled, setIsInstalled] = useState(false)
  const [cleanupOptions, setCleanupOptions] = useState<CleanupOptions>({
    workspaces: true, artifacts: true, history: false, gradleCache: false,
  })
  const autoSaveTimers = useRef(new Map<string, number>())
  const buildEvents = useRef<EventSource | null>(null)
  const buildAbortController = useRef<AbortController | null>(null)
  const buildPrepareInFlight = useRef(false)
  const buildCancelRequested = useRef(false)
  const pendingBuildId = useRef<string | null>(null)
  const [buildHistory, setBuildHistory] = useState<Build[]>([])
  const [restoreProjectId] = useState(() => localStorage.getItem('android-web-ide.activeProjectId'))
  const cachedAccessMode = localStorage.getItem(accessModeKey)
  const authMode = authStatus?.mode ?? (cachedAccessMode === 'public' ? 'public' : 'private')
  const onlineAccess = isOnline && serverAvailable
  const authenticated = authStatus?.mode === 'public' || authStatus?.authenticated === true ||
    (!serverAvailable && authMode === 'public') ||
    (!serverAvailable && authMode === 'private' && offlineUnlocked)
  const activeProject = projects.find(p => p.id === activeProjectId) ?? null
  const activeFile = files.find(f => f.id === activeFileId) ?? null

  useEffect(() => {
    let disposed = false
    fetch('/api/auth/status')
      .then(async response => {
        if (!response.ok) throw new Error(await response.text())
        return response.json() as Promise<AuthStatus>
      })
      .then(status => {
        if (disposed) return
        setAuthStatus(status)
        setServerAvailable(true)
        setIsOnline(navigator.onLine)
        localStorage.setItem(accessModeKey, status.mode)
        if (status.mode === 'private' && status.authenticated) {
          localStorage.setItem(offlineUnlockKey, 'true')
          setOfflineUnlocked(true)
        }
      })
      .catch(error => {
        if (disposed) return
        setServerAvailable(false)
        setIsOnline(navigator.onLine)
        setLoginError(error instanceof Error ? error.message : 'gagal menghubungi server')
      })
    return () => { disposed = true }
  }, [])

  useEffect(() => {
    const handleOffline = () => {
      setIsOnline(false)
      setServerAvailable(false)
      setStorageOpen(false)
      buildEvents.current?.close()
      buildEvents.current = null
    }
    const handleOnline = () => {
      setIsOnline(true)
      fetch('/api/auth/status')
        .then(async response => {
          if (!response.ok) throw new Error(await response.text())
          return response.json() as Promise<AuthStatus>
        })
        .then(status => {
          setServerAvailable(true)
          setAuthStatus(status)
          localStorage.setItem(accessModeKey, status.mode)
          if (status.mode === 'private' && status.authenticated) {
            localStorage.setItem(offlineUnlockKey, 'true')
            setOfflineUnlocked(true)
          }
          setLoginError('')
        })
        .catch(error => {
          setServerAvailable(false)
          setStorageOpen(false)
          setLoginError(error instanceof Error ? error.message : 'gagal menghubungi server')
        })
    }
    const handleInstallPrompt = (event: Event) => {
      event.preventDefault()
      setInstallPrompt(event as InstallPromptEvent)
    }
    const handleInstalled = () => {
      setIsInstalled(true)
      setInstallPrompt(null)
    }
    const handlePwaError = (event: Event) => {
      const error = (event as CustomEvent<string>).detail
      setToast(`PWA gagal disiapkan: ${error}`)
    }
    setIsInstalled(window.matchMedia('(display-mode: standalone)').matches ||
      ('standalone' in navigator && navigator.standalone === true))
    window.addEventListener('offline', handleOffline)
    window.addEventListener('online', handleOnline)
    window.addEventListener('beforeinstallprompt', handleInstallPrompt)
    window.addEventListener('appinstalled', handleInstalled)
    window.addEventListener('ide:pwa-error', handlePwaError)
    return () => {
      window.removeEventListener('offline', handleOffline)
      window.removeEventListener('online', handleOnline)
      window.removeEventListener('beforeinstallprompt', handleInstallPrompt)
      window.removeEventListener('appinstalled', handleInstalled)
      window.removeEventListener('ide:pwa-error', handlePwaError)
    }
  }, [])

  useEffect(() => {
    if (!onlineAccess || authStatus?.mode !== 'private' || !authStatus.authenticated) return
    const timer = window.setInterval(() => {
      fetch('/api/auth/status')
        .then(response => response.ok ? response.json() as Promise<AuthStatus> : null)
        .then(status => {
          if (status && !status.authenticated) {
            setAuthStatus(status)
            setLoginError('session berakhir, masuk lagi untuk melanjutkan')
          }
        })
        .catch(() => {
          setServerAvailable(false)
        })
    }, 30_000)
    return () => window.clearInterval(timer)
  }, [authStatus, onlineAccess])

  useEffect(() => {
    if (!authenticated) return
    db.projects.orderBy('updatedAt').reverse().toArray().then(items => {
      setProjects(items)
      if (restoreProjectId && items.some(project => project.id === restoreProjectId)) setActiveProjectId(restoreProjectId)
    })
  }, [authenticated, restoreProjectId])

  useEffect(() => {
    if (activeProjectId) localStorage.setItem('android-web-ide.activeProjectId', activeProjectId)
    else localStorage.removeItem('android-web-ide.activeProjectId')
  }, [activeProjectId])

  useEffect(() => {
    if (!activeProjectId) {
      setFiles([])
      setOpenTabs([])
      setActiveFileId(null)
      setEditorStateProjectId(null)
      return
    }
    let disposed = false
    setFiles([])
    setEditorStateProjectId(null)
    db.files.where('projectId').equals(activeProjectId).toArray().then(projectFiles => {
      if (disposed) return
      const state = readProjectEditorState(activeProjectId)
      const filesById = new Map(projectFiles.map(file => [file.id, file] as const))
      const restoredTabs = [...new Set(state?.openFileIds ?? [])]
        .map(id => filesById.get(id))
        .filter((file): file is ProjectFile => file !== undefined)
      const restoredActiveFileId = state?.activeFileId && restoredTabs.some(file => file.id === state.activeFileId)
        ? state.activeFileId
        : restoredTabs.at(-1)?.id ?? null
      setFiles(projectFiles)
      setOpenTabs(restoredTabs)
      setActiveFileId(restoredActiveFileId)
      setExpanded(new Set(state?.expanded ?? defaultExpandedPaths))
      setEditorStateProjectId(activeProjectId)
    }).catch(error => {
      if (!disposed) setToast(error instanceof Error ? error.message : 'gagal memuat file project')
    })
    return () => { disposed = true }
  }, [activeProjectId])

  useEffect(() => {
    if (!activeProjectId || editorStateProjectId !== activeProjectId) return
    try {
      localStorage.setItem(editorStateKey(activeProjectId), JSON.stringify({
        openFileIds: openTabs.map(file => file.id),
        activeFileId,
        expanded: [...expanded],
      } satisfies ProjectEditorState))
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'gagal menyimpan state editor')
    }
  }, [activeProjectId, editorStateProjectId, openTabs, activeFileId, expanded])

  useEffect(() => {
    buildEvents.current?.close()
    buildEvents.current = null
    if (!onlineAccess || !authenticated || !activeProjectId) {
      setBuildHistory([])
      setBuildId(null)
      setBuildStatus('idle')
      setLogs([])
      setBuildOpen(false)
      return
    }
    let disposed = false
    fetch(`/api/builds?projectId=${encodeURIComponent(activeProjectId)}`)
      .then(response => {
        if (!response.ok) throw new Error('gagal memuat riwayat build')
        return response.json() as Promise<Build[]>
      })
      .then(builds => {
        if (disposed) return
        setBuildHistory(builds)
        const latest = builds[0]
        if (latest) {
          setBuildOpen(true)
          followBuild(latest)
        } else {
          setBuildId(null)
          setBuildStatus('idle')
          setLogs([])
          setBuildOpen(false)
        }
      })
      .catch(error => {
        if (!disposed) setToast(error instanceof Error ? error.message : 'gagal memuat riwayat build')
      })
    return () => {
      disposed = true
      buildEvents.current?.close()
      buildEvents.current = null
    }
  }, [activeProjectId, authenticated, onlineAccess])

  useEffect(() => {
    if (!toast) return
    const timer = window.setTimeout(() => setToast(''), 2800)
    return () => window.clearTimeout(timer)
  }, [toast])

  const tree = useMemo(() => buildTree(
    search ? files.filter(f => f.path.toLowerCase().includes(search.toLowerCase())) : files
  ), [files, search])

  async function submitLogin(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setLoginLoading(true)
    setLoginError('')
    try {
      const response = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: loginPassword }),
      })
      const body = await response.json() as { error?: string; authenticated?: boolean }
      if (!response.ok) throw new Error(body.error || 'login gagal')
      setAuthStatus({ mode: 'private', authenticated: true })
      setServerAvailable(true)
      setIsOnline(true)
      localStorage.setItem(accessModeKey, 'private')
      localStorage.setItem(offlineUnlockKey, 'true')
      setOfflineUnlocked(true)
      setLoginPassword('')
    } catch (error) {
      setLoginError(error instanceof Error ? error.message : 'login gagal')
    } finally {
      setLoginLoading(false)
    }
  }

  async function logout() {
    const shouldNotifyServer = onlineAccess
    localStorage.removeItem(offlineUnlockKey)
    setOfflineUnlocked(false)
    buildEvents.current?.close()
    setAuthStatus({ mode: 'private', authenticated: false })
    setActiveProjectId(null)
    setProjects([])
    setFiles([])
    setBuildHistory([])
    setBuildId(null)
    setBuildStatus('idle')
    setLogs([])
    setBuildOpen(false)
    if (shouldNotifyServer) {
      try {
        const response = await fetch('/api/auth/logout', { method: 'POST' })
        if (!response.ok) throw new Error(await response.text() || 'logout server gagal')
      } catch (error) {
        setToast(error instanceof Error ? error.message : 'logout server gagal')
      }
    }
  }

  async function refreshStorageStats() {
    if (!onlineAccess) {
      setToast('Kelola storage build butuh koneksi ke server')
      return
    }
    setStorageLoading(true)
    try {
      const response = await fetch('/api/maintenance/storage')
      if (!response.ok) throw new Error(await response.text())
      setStorageStats(await response.json() as StorageStats)
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'gagal membaca penggunaan storage')
    } finally {
      setStorageLoading(false)
    }
  }

  function openStorage() {
    if (!onlineAccess) {
      setToast('Kelola storage build butuh koneksi ke server')
      return
    }
    setStorageOpen(true)
  }

  async function loadArtifacts() {
    if (!onlineAccess) {
      setArtifactsError('daftar APK butuh koneksi ke server')
      return
    }
    setArtifactsLoading(true)
    setArtifactsError('')
    try {
      const response = await fetch('/api/artifacts')
      const body = await response.text()
      if (!response.ok) throw new Error(body || `gagal memuat daftar APK (${response.status})`)
      setArtifacts(JSON.parse(body) as APKArtifact[])
    } catch (error) {
      setArtifactsError(error instanceof Error ? error.message : 'gagal memuat daftar APK')
    } finally {
      setArtifactsLoading(false)
    }
  }

  function openArtifacts() {
    setArtifactsOpen(true)
    void loadArtifacts()
  }

  async function deleteArtifact(artifact: APKArtifact) {
    const projectName = projects.find(project => project.id === artifact.projectId)?.name ?? artifact.projectId
    if (!confirm(`hapus ${artifact.filename} dari project "${projectName}"? riwayat build tetap disimpan.`)) return
    setArtifactDeletingId(artifact.buildId)
    setArtifactsError('')
    try {
      const response = await fetch(`/api/artifacts/${encodeURIComponent(artifact.buildId)}`, { method: 'DELETE' })
      const body = await response.text()
      if (!response.ok) throw new Error(body || `gagal menghapus APK (${response.status})`)
      setArtifacts(prev => prev.filter(item => item.buildId !== artifact.buildId))
      setToast('APK dihapus; riwayat build tetap disimpan')
    } catch (error) {
      setArtifactsError(error instanceof Error ? error.message : 'gagal menghapus APK')
    } finally {
      setArtifactDeletingId(null)
    }
  }

  async function cleanStorage() {
    if (!onlineAccess) {
      setToast('Pembersihan storage build butuh koneksi ke server')
      return
    }
    if (!Object.values(cleanupOptions).some(Boolean)) {
      setToast('Pilih data yang mau dibersihkan')
      return
    }
    const confirmed = confirm(
      `Lanjut bersihkan storage?${cleanupOptions.history ? '\nRiwayat, log, dan binary cache build akan dihapus.' : ''}${cleanupOptions.artifacts || cleanupOptions.history ? '\nAPK hasil build yang dipilih akan dihapus.' : ''}${cleanupOptions.gradleCache ? '\nCache Gradle dihapus dan perlu diunduh ulang saat build berikutnya.' : ''}`,
    )
    if (!confirmed) return
    setStorageLoading(true)
    try {
      const response = await fetch('/api/maintenance/cleanup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(cleanupOptions),
      })
      const body = await response.text()
      let data: { error?: string; removed?: Record<string, number>; storage?: StorageStats }
      try { data = JSON.parse(body) as typeof data }
      catch { data = { error: body } }
      if (!response.ok) throw new Error(data.error || `cleanup gagal (${response.status})`)
      setStorageStats(data.storage ?? null)
      setToast(`storage dibersihkan: ${formatBytes(Object.values(data.removed ?? {}).reduce((sum, size) => sum + size, 0))} dibebaskan`)
      if (cleanupOptions.history) {
        setBuildHistory([])
        setBuildId(null)
        setBuildStatus('idle')
        setLogs([])
        setBuildOpen(false)
      } else if (activeProjectId) {
        const historyResponse = await fetch(`/api/builds?projectId=${encodeURIComponent(activeProjectId)}`)
        if (historyResponse.ok) setBuildHistory(await historyResponse.json() as Build[])
      }
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'cleanup gagal')
    } finally {
      setStorageLoading(false)
    }
  }

  async function createProject(data: Omit<Project, 'id' | 'createdAt' | 'updatedAt' | 'templateId'>) {
    const now = Date.now()
    const project: Project = { ...data, id: uid(), templateId: 'empty-app', createdAt: now, updatedAt: now }
    const generated = emptyTemplate(project)
    await db.projects.add(project)
    await db.files.bulkAdd(generated)
    setProjects(prev => [project, ...prev])
    setActiveProjectId(project.id)
    setShowCreate(false)
    setToast('Project berhasil dibuat')
  }

  async function saveFile(file: ProjectFile, content: string) {
    const updated = { ...file, content, sha256: undefined, size: new Blob([content]).size, updatedAt: Date.now() }
    await db.files.put(updated)
    setFiles(prev => prev.map(f => f.id === file.id ? updated : f))
    setOpenTabs(prev => prev.map(f => f.id === file.id ? updated : f))
  }

  function editFile(content: string | undefined) {
    if (!activeFile || content === undefined) return
    setFiles(prev => prev.map(f => f.id === activeFile.id ? { ...f, content, sha256: '' } : f))
    void db.files.update(activeFile.id, { sha256: undefined }).catch(error => {
      setToast(error instanceof Error ? error.message : 'gagal menginvalidasi hash file')
    })
    const existingTimer = autoSaveTimers.current.get(activeFile.id)
    if (existingTimer) window.clearTimeout(existingTimer)
    const timer = window.setTimeout(() => saveFile(activeFile, content), 800)
    autoSaveTimers.current.set(activeFile.id, timer)
  }

  async function openFile(file: ProjectFile) {
    if (!openTabs.some(f => f.id === file.id)) setOpenTabs(prev => [...prev, file])
    setActiveFileId(file.id)
    setDrawerOpen(false)
  }

  async function deleteProject(project: Project) {
    if (!confirm(`Hapus project "${project.name}"?`)) return
    await db.files.where('projectId').equals(project.id).delete()
    await db.projects.delete(project.id)
    localStorage.removeItem(editorStateKey(project.id))
    setProjects(prev => prev.filter(p => p.id !== project.id))
    if (activeProjectId === project.id) setActiveProjectId(null)
  }

  async function exportProject() {
    if (!activeProject) return
    const zip = new JSZip()
    for (const file of files) {
      if (file.binary) zip.file(file.path, file.binary)
      else zip.file(file.path, file.content ?? '')
    }
    const blob = await zip.generateAsync({ type: 'blob' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${activeProject.name.replace(/\s+/g, '-')}.zip`
    a.click()
    URL.revokeObjectURL(url)
  }

  async function importZip(file: File) {
    try {
      const zip = await JSZip.loadAsync(file)
      const projectId = uid()
      const now = Date.now()
      let projectName = file.name.replace(/\.zip$/i, '') || 'Imported App'
      const imported: ProjectFile[] = []
      for (const [path, entry] of Object.entries(zip.files)) {
        const clean = path.replace(/^\/+/, '')
        if (!clean || entry.dir || /(^|\/)(\.git|\.idea|build|\.gradle)(\/|$)/.test(clean) ||
            /(^|\/)(local\.properties|.*\.iml)$/.test(clean)) continue
        const lower = clean.toLowerCase()
        const text = /\.(kt|java|xml|gradle|kts|properties|json|txt|md|pro)$/i.test(lower)
        const content = text ? await entry.async('string') : undefined
        const binary = text ? undefined : await entry.async('blob')
        const mimeType = binary?.type || 'application/octet-stream'
        void mimeType
        imported.push({ id: uid(), projectId, path: clean, type: 'file', content, binary,
          size: binary?.size ?? new Blob([content ?? '']).size, createdAt: now, updatedAt: now })
      }
      if (!imported.some(f => /(^|\/)(settings\.gradle(\.kts)?|build\.gradle(\.kts)?)$/.test(f.path))) {
        throw new Error('ZIP tidak memiliki settings.gradle/build.gradle yang dikenali')
      }
      const manifest = imported.find(f => f.path.endsWith('/AndroidManifest.xml') || f.path === 'AndroidManifest.xml')
      const manifestMatch = manifest?.content?.match(/<manifest[^>]*package=["']([^"']+)/)
      const project: Project = {
        id: projectId, name: projectName,
        packageName: manifestMatch?.[1] || 'com.example.imported',
        language: imported.some(f => f.path.endsWith('.kt')) ? 'kotlin' : 'java',
        templateId: 'imported', minSdk: 24, targetSdk: 34, createdAt: now, updatedAt: now,
      }
      await db.projects.add(project)
      await db.files.bulkAdd(imported)
      setProjects(prev => [project, ...prev])
      setActiveProjectId(projectId)
      setToast('Project ZIP berhasil diimpor')
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'Gagal mengimpor ZIP')
    }
  }

  async function createFile() {
    if (!activeProject) return
    const path = prompt('Path file baru (contoh app/src/main/java/Main.kt)')
    if (!path) return
    const clean = path.replace(/^\/+/, '')
    if (clean.includes('..') || clean.includes('\\')) { setToast('Path file tidak valid'); return }
    if (files.some(f => f.path === clean)) { setToast('File sudah ada'); return }
    let content = ''
    if (clean.endsWith('.xml') && clean.includes('/res/layout/')) content = '<?xml version="1.0" encoding="utf-8"?>\n<LinearLayout xmlns:android="http://schemas.android.com/apk/res/android" android:layout_width="match_parent" android:layout_height="match_parent"/>\n'
    const filename = clean.split('/').pop() || 'NewClass'
    if (clean.endsWith('.kt')) content = `package ${activeProject.packageName}\n\nclass ${filename.replace(/\.kt$/, '')}\n`
    if (clean.endsWith('.java')) content = `package ${activeProject.packageName};\n\npublic class ${filename.replace(/\.java$/, '')} {\n}\n`
    const now = Date.now()
    const file: ProjectFile = { id: uid(), projectId: activeProject.id, path: clean, type: 'file',
      content, size: new Blob([content]).size, createdAt: now, updatedAt: now }
    await db.files.add(file)
    setFiles(prev => [...prev, file])
    openFile(file)
  }

  async function deleteFile(file: ProjectFile) {
    if (!confirm(`Hapus "${file.path}"?`)) return
    await db.files.delete(file.id)
    setFiles(prev => prev.filter(f => f.id !== file.id))
    setOpenTabs(prev => prev.filter(f => f.id !== file.id))
    if (activeFileId === file.id) setActiveFileId(null)
  }

  function followBuild(build: Build) {
    buildEvents.current?.close()
    setBuildId(build.id)
    setBuildStatus(build.status)
    setLogs([])
    setBuildOpen(true)
    const events = new EventSource(`/api/builds/${build.id}/logs`)
    buildEvents.current = events
    events.onmessage = event => {
      const log = JSON.parse(event.data) as { message: string }
      setLogs(prev => [...prev, log.message])
      setBuildStatus(prev => prev === 'queued' ? 'building' : prev)
    }
    events.addEventListener('end', (event: MessageEvent<string>) => {
      const status = event.data as Build['status']
      setBuildStatus(status === 'success' || status === 'failed' || status === 'canceled' ? status : 'failed')
      setBuildCanceling(false)
      events.close()
      if (buildEvents.current === events) buildEvents.current = null
      fetch(`/api/builds?projectId=${encodeURIComponent(build.projectId)}`)
        .then(response => response.ok ? response.json() as Promise<Build[]> : [])
        .then(setBuildHistory)
        .catch(() => undefined)
    })
  }

  async function copyLogs() {
    try {
      await navigator.clipboard.writeText(logs.join('\n'))
      setToast('log berhasil disalin')
    } catch (error) {
      setToast(error instanceof Error ? `gagal menyalin log: ${error.message}` : 'gagal menyalin log')
    }
  }

  async function cancelActiveBuild() {
    if (buildCanceling) return
    const controller = buildAbortController.current
    const currentBuildId = pendingBuildId.current ?? buildId
    if (!currentBuildId) {
      if (!controller) return
      if (buildPrepareInFlight.current) {
        buildCancelRequested.current = true
        setBuildCanceling(true)
        setLogs(prev => [...prev, 'Pembatalan menunggu reservasi build dari server...'])
        return
      }
      setBuildCanceling(true)
      controller.abort()
      setBuildStatus('canceled')
      setLogs(prev => [...prev, 'Build dibatalkan sebelum dikirim ke worker.'])
      return
    }
    if (!onlineAccess) {
      setToast('pembatalan build butuh koneksi ke server')
      return
    }
    setBuildCanceling(true)
    let waitForBuildEnd = false
    try {
      const response = await fetch(`/api/builds/${encodeURIComponent(currentBuildId)}`, { method: 'DELETE' })
      if (!response.ok) {
        const message = await response.text()
        throw new Error(message || `gagal membatalkan build (${response.status})`)
      }
      if (controller) {
        controller.abort()
        setBuildStatus('canceled')
        setLogs(prev => [...prev, 'Build dibatalkan.'])
        setToast('build dibatalkan')
      } else {
        waitForBuildEnd = buildEvents.current !== null
        if (!waitForBuildEnd) setBuildStatus('canceled')
        setLogs(prev => [...prev, 'Permintaan pembatalan build dikirim, menunggu worker berhenti...'])
        setToast('permintaan pembatalan build dikirim')
      }
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'gagal membatalkan build')
    } finally {
      if (!waitForBuildEnd) setBuildCanceling(false)
    }
  }

  async function startBuild() {
    if (!onlineAccess) {
      setToast('Build APK butuh koneksi ke server')
      return
    }
    if (!activeProject) return
    if (buildStatus === 'preparing' || buildStatus === 'assembling' ||
        buildStatus === 'queued' || buildStatus === 'building') {
      setBuildOpen(true)
      return
    }

    const buildFiles = await ensureGeneratedJvmTargets(activeProject, files)
    if (buildFiles !== files) setFiles(buildFiles)
    const invalidReason = validateBuildProject(activeProject, buildFiles)
    if (invalidReason) {
      setToast(invalidReason)
      return
    }
    setBuildOpen(true)
    setBuildId(null)
    setBuildCanceling(false)
    buildPrepareInFlight.current = false
    buildCancelRequested.current = false
    pendingBuildId.current = null
    setBuildStatus('preparing')
    setLogs(['Menghitung SHA-256 dan menyiapkan manifest...'])
    const controller = new AbortController()
    buildAbortController.current = controller
    let reservation: Build | null = null
    let finalizeStarted = false
    try {
      const manifestFiles = await createBuildManifest(buildFiles, controller.signal)
      const totalBytes = manifestFiles.reduce((sum, file) => sum + file.size, 0)
      if (totalBytes > 250 * 1024 * 1024) throw new Error('Ukuran project melebihi 250 MB')
      buildPrepareInFlight.current = true
      const response = await fetch('/api/builds/prepare', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        signal: controller.signal,
        body: JSON.stringify({
          projectId: activeProject.id,
          idempotencyKey: crypto.randomUUID(),
          files: manifestFiles,
        }),
      })
      const body = await response.text()
      buildPrepareInFlight.current = false
      let data: PrepareResponse
      try { data = JSON.parse(body) as PrepareResponse }
      catch { data = { error: body || `request gagal (${response.status})`, build: {} as Build, missingBlobs: [] } }
      if (response.status === 409 && data.build?.id) {
        buildCancelRequested.current = false
        setToast('Project ini masih punya build aktif')
        followBuild(data.build)
        return
      }
      if (!response.ok) throw new Error(data.error || body || `Gagal menyiapkan build (${response.status})`)
      if (!data.build?.id) throw new Error('server tidak mengembalikan reservasi build')
      const buildReservation = data.build
      reservation = buildReservation
      pendingBuildId.current = buildReservation.id
      if (buildReservation.status !== 'preparing') {
        buildCancelRequested.current = false
        setBuildHistory(prev => [buildReservation, ...prev.filter(item => item.id !== buildReservation.id)])
        followBuild(buildReservation)
        return
      }
      setBuildId(buildReservation.id)
      setBuildStatus('preparing')
      if (buildCancelRequested.current) {
        const cancelResponse = await fetch(`/api/builds/${encodeURIComponent(buildReservation.id)}`, { method: 'DELETE' })
        if (!cancelResponse.ok) {
          const message = await cancelResponse.text()
          throw new Error(message || `gagal membatalkan build (${cancelResponse.status})`)
        }
        controller.abort()
        buildCancelRequested.current = false
        setBuildStatus('canceled')
        setLogs(prev => [...prev, 'Build dibatalkan.'])
        setToast('build dibatalkan')
        return
      }
      const binaryByPath = new Map(buildFiles.map(file => [file.path, file] as const))
      const binaryByHash = new Map(manifestFiles
        .filter(file => file.kind === 'binary')
        .flatMap(file => {
          const binary = binaryByPath.get(file.path)?.binary
          return binary ? [[file.sha256, binary] as const] : []
        }))
      for (const [index, hash] of data.missingBlobs.entries()) {
        const binary = binaryByHash.get(hash)
        if (!binary) throw new Error(`binary ${hash} tidak ditemukan di IndexedDB`)
        setLogs(prev => [...prev, `Mengupload binary ${index + 1}/${data.missingBlobs.length}...`])
        const uploadResponse = await fetch(`/api/builds/${buildReservation.id}/blobs/${hash}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/octet-stream' },
          signal: controller.signal,
          body: binary,
        })
        if (!uploadResponse.ok) throw new Error(await uploadResponse.text() || `Upload binary gagal (${uploadResponse.status})`)
      }
      setBuildStatus('assembling')
      setLogs(prev => [...prev, 'Binary lengkap, merakit workspace build...'])
      finalizeStarted = true
      const finalizeResponse = await fetch(`/api/builds/${buildReservation.id}/finalize`, {
        method: 'POST',
        signal: controller.signal,
      })
      const finalizeBody = await finalizeResponse.text()
      let build: Build
      try { build = JSON.parse(finalizeBody) as Build }
      catch { throw new Error(finalizeBody || `Finalisasi build gagal (${finalizeResponse.status})`) }
      if (!finalizeResponse.ok) throw new Error((build as Build & { error?: string }).error || `Finalisasi build gagal (${finalizeResponse.status})`)
      setBuildHistory(prev => [build, ...prev.filter(item => item.id !== build.id)])
      followBuild(build)
    } catch (error) {
      buildPrepareInFlight.current = false
      if (reservation && !finalizeStarted) {
        await fetch(`/api/builds/${reservation.id}`, { method: 'DELETE' }).catch(() => undefined)
      }
      if (controller.signal.aborted || (buildCancelRequested.current && !reservation)) {
        setBuildStatus('canceled')
        setLogs(prev => prev.some(line => line.includes('Build dibatalkan'))
          ? prev
          : [...prev, 'Build dibatalkan.'])
      } else {
        setBuildStatus('failed')
        setLogs(prev => [...prev, error instanceof Error ? error.message : 'Gagal upload build'])
      }
    } finally {
      if (buildAbortController.current === controller) buildAbortController.current = null
      buildPrepareInFlight.current = false
      buildCancelRequested.current = false
      pendingBuildId.current = null
      setBuildCanceling(false)
    }
  }

  async function installApp() {
    if (!installPrompt) {
      setToast('Buka menu browser lalu pilih "Install app" atau "Add to Home Screen"')
      return
    }
    try {
      await installPrompt.prompt()
      const choice = await installPrompt.userChoice
      if (choice.outcome === 'accepted') setToast('Android Web IDE berhasil dipasang')
      setInstallPrompt(null)
    } catch (error) {
      setToast(error instanceof Error ? `gagal memasang app: ${error.message}` : 'gagal memasang app')
    }
  }

  async function openNotifications() {
    setNotificationsOpen(true)
    setNotificationLoading(true)
    setNotificationError('')
    try {
      const [settingsResponse, pushResponse] = await Promise.all([
        fetch('/api/notifications/settings'),
        fetch('/api/notifications/push/subscription'),
      ])
      const settings = await settingsResponse.json() as NotificationSettings & { error?: string }
      if (!settingsResponse.ok) throw new Error(settings.error || 'gagal membaca pengaturan email')
      const pushStatus = await pushResponse.json() as PushSubscriptionStatus & { error?: string }
      if (!pushResponse.ok) throw new Error(pushStatus.error || 'gagal membaca status browser notification')
      setNotificationSettings(settings)
      const supported = 'Notification' in window && 'PushManager' in window && 'serviceWorker' in navigator
      setPushPermission(supported ? Notification.permission : 'denied')
      if (supported) {
        const registration = await navigator.serviceWorker.ready
        let subscription = await registration.pushManager.getSubscription()
        if (subscription && !pushStatus.subscribed) {
          const saveResponse = await fetch('/api/notifications/push/subscription', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(subscription.toJSON()),
          })
          if (!saveResponse.ok) throw new Error('gagal menyimpan browser notification')
        } else if (!subscription && pushStatus.subscribed) {
          await fetch('/api/notifications/push/subscription', {
            method: 'DELETE',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ endpoint: '' }),
          })
        }
        setPushSubscribed(Boolean(subscription))
      } else {
        setPushSubscribed(false)
      }
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'gagal membaca pengaturan notifikasi')
    } finally {
      setNotificationLoading(false)
    }
  }

  async function enablePushNotifications() {
    if (!('Notification' in window) || !('PushManager' in window) || !('serviceWorker' in navigator)) {
      setNotificationError('browser ini belum mendukung browser push notification')
      return
    }
    setPushBusy(true)
    setNotificationError('')
    try {
      const permission = Notification.permission === 'default'
        ? await Notification.requestPermission()
        : Notification.permission
      setPushPermission(permission)
      if (permission !== 'granted') throw new Error('izin notifikasi browser tidak diberikan')
      const keyResponse = await fetch('/api/notifications/push/key')
      const keyData = await keyResponse.json() as { publicKey?: string; error?: string }
      if (!keyResponse.ok || !keyData.publicKey) throw new Error(keyData.error || 'gagal mengambil kunci browser notification')
      const registration = await navigator.serviceWorker.ready
      const subscription = await registration.pushManager.getSubscription() ??
        await registration.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: decodeVapidPublicKey(keyData.publicKey),
        })
      const response = await fetch('/api/notifications/push/subscription', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(subscription.toJSON()),
      })
      const data = await response.json() as { error?: string }
      if (!response.ok) {
        await subscription.unsubscribe()
        throw new Error(data.error || 'gagal mengaktifkan browser notification')
      }
      setPushSubscribed(true)
      setToast('browser notification aktif')
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'gagal mengaktifkan browser notification')
    } finally {
      setPushBusy(false)
    }
  }

  async function disablePushNotifications() {
    setPushBusy(true)
    setNotificationError('')
    try {
      const registration = await navigator.serviceWorker.ready
      const subscription = await registration.pushManager.getSubscription()
      const response = await fetch('/api/notifications/push/subscription', {
        method: 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ endpoint: subscription?.endpoint ?? '' }),
      })
      const data = await response.json() as { error?: string }
      if (!response.ok) throw new Error(data.error || 'gagal menonaktifkan browser notification')
      if (subscription) await subscription.unsubscribe()
      setPushSubscribed(false)
      setToast('browser notification dinonaktifkan')
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'gagal menonaktifkan browser notification')
    } finally {
      setPushBusy(false)
    }
  }

  async function sendTestPushNotification() {
    setPushBusy(true)
    setNotificationError('')
    try {
      const response = await fetch('/api/notifications/push/test', { method: 'POST' })
      const data = await response.json() as { error?: string }
      if (!response.ok) throw new Error(data.error || 'browser notification test gagal dikirim')
      setToast('browser notification test dikirim')
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'browser notification test gagal dikirim')
    } finally {
      setPushBusy(false)
    }
  }

  async function saveNotificationSettings(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!notificationSettings) return
    setNotificationSaving(true)
    setNotificationError('')
    try {
      const response = await fetch('/api/notifications/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          recipient: notificationSettings.recipient,
          notifySuccess: notificationSettings.notifySuccess,
          notifyFailure: notificationSettings.notifyFailure,
        }),
      })
      const data = await response.json() as NotificationSettings & { error?: string }
      if (!response.ok) throw new Error(data.error || 'gagal menyimpan pengaturan email')
      setNotificationSettings(data)
      setToast('pengaturan email tersimpan')
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'gagal menyimpan pengaturan email')
    } finally {
      setNotificationSaving(false)
    }
  }

  async function sendTestNotification() {
    if (!notificationSettings) return
    setNotificationTesting(true)
    setNotificationError('')
    try {
      const saveResponse = await fetch('/api/notifications/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          recipient: notificationSettings.recipient,
          notifySuccess: notificationSettings.notifySuccess,
          notifyFailure: notificationSettings.notifyFailure,
        }),
      })
      const saved = await saveResponse.json() as NotificationSettings & { error?: string }
      if (!saveResponse.ok) throw new Error(saved.error || 'gagal menyimpan pengaturan email')
      setNotificationSettings(saved)
      const response = await fetch('/api/notifications/test', { method: 'POST' })
      const data = await response.json() as { error?: string }
      if (!response.ok) throw new Error(data.error || 'email test gagal dikirim')
      setToast('email test berhasil dikirim')
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'email test gagal dikirim')
    } finally {
      setNotificationTesting(false)
    }
  }

  function renderTree(nodes: TreeNode[], depth = 0) {
    return nodes.map(node => (
      <div key={node.path}>
        {node.type === 'directory' ? (
          <>
            <button className="tree-row" style={{ paddingLeft: 10 + depth * 14 }}
              onClick={() => setExpanded(prev => {
                const next = new Set(prev)
                if (next.has(node.path)) next.delete(node.path)
                else next.add(node.path)
                return next
              })}>
              <ChevronRight size={14} className={expanded.has(node.path) ? 'chevron-open' : ''}/>
              <span className="folder-icon">▰</span><span>{node.name}</span>
            </button>
            {expanded.has(node.path) && renderTree(node.children, depth + 1)}
          </>
        ) : (
          <button className={`tree-row file-row ${activeFile?.path === node.path ? 'active' : ''}`}
            style={{ paddingLeft: 24 + depth * 14 }}
            onClick={() => {
              const file = files.find(f => f.path === node.path)
              if (file) openFile(file)
            }}>
            <FileCode2 size={15}/><span>{node.name}</span>
            <button className="row-menu" title="Hapus file" onClick={event => {
              event.stopPropagation()
              const file = files.find(f => f.path === node.path)
              if (file) deleteFile(file)
            }}><Trash2 size={12}/></button>
          </button>
        )}
      </div>
    ))
  }

  if (!authStatus && !(!serverAvailable && (authMode === 'public' || (authMode === 'private' && offlineUnlocked)))) {
    return <LoginScreen loading={!loginError && serverAvailable} offline={!onlineAccess} error={loginError} onSubmit={submitLogin} password={loginPassword} onPasswordChange={setLoginPassword} busy={loginLoading}/>
  }
  if (authMode === 'private' && !authenticated) {
    return <LoginScreen offline={!onlineAccess} error={loginError} onSubmit={submitLogin} password={loginPassword} onPasswordChange={setLoginPassword} busy={loginLoading}/>
  }

  return (
    <div className="app-shell">
      <header className="topbar">
        <button className="icon-btn" onClick={() => setDrawerOpen(v => !v)}><Menu size={20}/></button>
        <div className="brand"><div className="brand-mark"><Box size={18}/></div><span>Droid Studio</span></div>
        {activeProject && <div className="project-title">{activeProject.name}</div>}
        {!onlineAccess && <div className="offline-indicator"><WifiOff size={14}/><span>offline · edit lokal</span></div>}
        {!isInstalled && <button className="install-btn" title="Install Android Web IDE" onClick={() => void installApp()}><Smartphone size={16}/><span>Install</span></button>}
        <button className="build-btn" onClick={startBuild} disabled={!onlineAccess || !activeProject || buildCanceling || buildStatus === 'preparing' || buildStatus === 'assembling' || buildStatus === 'queued' || buildStatus === 'building'} title={!onlineAccess ? 'Build APK butuh koneksi ke server' : undefined}>
          <Play size={16} fill="currentColor"/><span>{!onlineAccess ? 'Offline' : buildStatus === 'preparing' || buildStatus === 'assembling' || buildStatus === 'queued' || buildStatus === 'building' ? 'Building...' : 'Build APK'}</span>
        </button>
        <button className="icon-btn" title={onlineAccess ? 'APK tersimpan' : 'Perlu koneksi ke server'} onClick={openArtifacts} disabled={!onlineAccess}><Archive size={18}/></button>
        {authMode === 'private' && <button className="icon-btn" title={onlineAccess ? 'Notifikasi build' : 'Perlu koneksi ke server'} onClick={() => void openNotifications()} disabled={!onlineAccess}><Bell size={18}/></button>}
        <button className="icon-btn storage-btn" title={onlineAccess ? 'Kelola storage build' : 'Perlu koneksi ke server'} onClick={openStorage} disabled={!onlineAccess}><HardDrive size={18}/></button>
        {authMode === 'private' && <button className="icon-btn" title="Logout" onClick={logout}><LogOut size={18}/></button>}
      </header>

      <aside className={`sidebar ${drawerOpen ? 'open' : ''}`}>
        <div className="sidebar-head">
          <div><small>WORKSPACE</small><strong>{activeProject?.name || 'Projects'}</strong></div>
          <button className="icon-btn" onClick={() => setDrawerOpen(false)}><X size={17}/></button>
        </div>
        {activeProject ? (
          <>
            <div className="sidebar-actions">
              <label className="tool-btn" title="Import ZIP"><Upload size={15}/><input type="file" accept=".zip" onChange={e => e.target.files?.[0] && importZip(e.target.files[0])}/></label>
              <button className="tool-btn" title="File baru" onClick={createFile}><FilePlus2 size={15}/></button>
              <button className="tool-btn" title="Export ZIP" onClick={exportProject}><Download size={15}/></button>
              <button className="tool-btn" title="Folder baru"><FolderPlus size={15}/></button>
            </div>
            <div className="search-box"><Search size={14}/><input placeholder="Cari file" value={search} onChange={e => setSearch(e.target.value)}/></div>
            <div className="tree">{renderTree(tree)}</div>
          </>
        ) : (
          <div className="project-list">
            {projects.map(project => (
              <article className="project-card" key={project.id}>
                <button className="project-open" onClick={() => setActiveProjectId(project.id)}>
                  <div className="app-icon">A</div>
                  <div><strong>{project.name}</strong><span>{project.packageName}</span></div>
                </button>
                <button className="icon-btn" onClick={() => deleteProject(project)}><Trash2 size={15}/></button>
              </article>
            ))}
            {projects.length === 0 && <div className="empty-state"><Box size={38}/><strong>Belum ada project</strong><span>Buat project Android pertama.</span></div>}
          </div>
        )}
      </aside>

      <main className="workspace">
        {activeProject ? (
          <>
            <div className="tabs">
              {openTabs.map(file => (
                <button key={file.id} className={`tab ${activeFileId === file.id ? 'active' : ''}`} onClick={() => setActiveFileId(file.id)}>
                  <FileCode2 size={14}/><span>{file.path.split('/').pop()}</span>
                  <X size={13} onClick={e => { e.stopPropagation(); setOpenTabs(prev => prev.filter(f => f.id !== file.id)); if (activeFileId === file.id) setActiveFileId(openTabs.find(f => f.id !== file.id)?.id ?? null) }}/>
                </button>
              ))}
              {openTabs.length === 0 && <div className="tab-placeholder">Pilih file dari explorer</div>}
            </div>
            {activeFile ? (
              <Editor
                height="calc(100vh - 110px)"
                language={activeFile.path.endsWith('.kt') ? 'kotlin' : activeFile.path.endsWith('.java') ? 'java' : activeFile.path.endsWith('.xml') ? 'xml' : activeFile.path.endsWith('.json') ? 'json' : activeFile.path.endsWith('.gradle') || activeFile.path.endsWith('.kts') ? 'gradle' : 'plaintext'}
                value={activeFile.content ?? ''}
                theme="vs-dark"
                onChange={editFile}
                options={{ fontSize: 14, lineNumbers: 'on', minimap: { enabled: false }, wordWrap: 'on', automaticLayout: true, padding: { top: 12 } }}
              />
            ) : <div className="editor-empty"><FileCode2 size={44}/><strong>Editor siap</strong><span>Buka file untuk mulai mengedit.</span></div>}
          </>
        ) : (
          <div className="home">
            <div className="home-head"><div><small>ANDROID PROJECTS</small><h1>Workspace</h1></div>
              <div className="home-actions">
                <button className="tool-btn artifact-dashboard-btn" onClick={openArtifacts} disabled={!onlineAccess}><Archive size={16}/>APK tersimpan</button>
                <button className="primary-btn" onClick={() => setShowCreate(true)}><PackagePlus size={17}/>Project baru</button>
              </div>
            </div>
            <div className="project-grid">
              {projects.map(project => (
                <article className="project-card large" key={project.id}>
                  <button className="project-open" onClick={() => setActiveProjectId(project.id)}>
                    <div className="app-icon large-icon">{project.name[0]?.toUpperCase()}</div>
                    <div><strong>{project.name}</strong><span>{project.language.toUpperCase()} • SDK {project.minSdk}+</span><small>{project.packageName}</small></div>
                  </button>
                  <button className="icon-btn" onClick={() => deleteProject(project)}><Trash2 size={16}/></button>
                </article>
              ))}
              {projects.length === 0 && <button className="create-card" onClick={() => setShowCreate(true)}><PackagePlus size={30}/><strong>Buat project Android</strong><span>Kotlin atau Java, XML Views</span></button>}
            </div>
          </div>
        )}
      </main>

      {showCreate && <CreateProjectModal onClose={() => setShowCreate(false)} onCreate={createProject}/>}
      {buildOpen && (
        <div className="build-sheet">
          <div className="build-head">
            <div><small>BUILD STATUS</small><strong>{buildStatus === 'idle' ? 'Debug APK' : buildStatus}</strong></div>
            <div className="build-head-actions">
              {(buildStatus === 'preparing' || buildStatus === 'assembling' || buildStatus === 'queued' || buildStatus === 'building') &&
                <button className="icon-btn cancel-build-btn" title={buildCanceling ? 'Membatalkan build' : 'Batalkan build'}
                  aria-label="Batalkan build" onClick={() => void cancelActiveBuild()} disabled={buildCanceling}>
                  <Ban size={16}/>
                </button>}
              <button className="icon-btn" title="Salin log" aria-label="Salin log" onClick={copyLogs} disabled={logs.length === 0}><Copy size={16}/></button>
              <button className="icon-btn" title="Tutup" aria-label="Tutup log" onClick={() => setBuildOpen(false)}><X size={18}/></button>
            </div>
          </div>
          {buildHistory.length > 1 && <div className="build-history"><label>Riwayat build<select value={buildId ?? ''} onChange={event => {
            const build = buildHistory.find(item => item.id === event.target.value)
            if (build) followBuild(build)
          }}>{buildHistory.map(build => <option key={build.id} value={build.id}>{build.status} · {new Date(build.createdAt).toLocaleString()}</option>)}</select></label></div>}
          <div className={`status-line ${buildStatus}`}><span></span>{buildStatus === 'preparing' ? 'Menyiapkan manifest dan binary...' : buildStatus === 'assembling' ? 'Merakit workspace...' : buildStatus === 'building' ? 'Building...' : buildStatus === 'success' ? 'Build berhasil' : buildStatus === 'failed' ? 'Build gagal' : buildStatus === 'canceled' ? 'Build dibatalkan' : 'Menunggu worker'}</div>
          <div className="log-view">{logs.map((line, i) => <div key={i}><span>{String(i + 1).padStart(3, '0')}</span>{line}</div>)}</div>
          {buildStatus === 'success' && buildId && buildHistory.find(build => build.id === buildId)?.artifact &&
            <a className="download-btn" href={`/api/builds/${buildId}/artifact`} download><Download size={17}/>Download APK</a>}
        </div>
      )}
      {storageOpen && (
        <StorageDialog
          stats={storageStats}
          loading={storageLoading}
          options={cleanupOptions}
          onOptionChange={(key, checked) => setCleanupOptions(prev => ({ ...prev, [key]: checked }))}
          onRefresh={() => void refreshStorageStats()}
          onCleanup={cleanStorage}
          onClose={() => setStorageOpen(false)}
        />
      )}
      {artifactsOpen && (
        <ArtifactDialog
          artifacts={artifacts}
          projects={projects}
          loading={artifactsLoading}
          deletingId={artifactDeletingId}
          error={artifactsError}
          onRefresh={() => void loadArtifacts()}
          onDelete={artifact => void deleteArtifact(artifact)}
          onClose={() => setArtifactsOpen(false)}
        />
      )}
      {notificationsOpen && (
        <NotificationDialog
          settings={notificationSettings}
          loading={notificationLoading}
          saving={notificationSaving}
          testing={notificationTesting}
          pushBusy={pushBusy}
          pushSupported={'Notification' in window && 'PushManager' in window && 'serviceWorker' in navigator}
          pushSubscribed={pushSubscribed}
          pushPermission={pushPermission}
          error={notificationError}
          onSettingsChange={setNotificationSettings}
          onSave={saveNotificationSettings}
          onTest={() => void sendTestNotification()}
          onPushToggle={() => void (pushSubscribed ? disablePushNotifications() : enablePushNotifications())}
          onPushTest={() => void sendTestPushNotification()}
          onClose={() => setNotificationsOpen(false)}
        />
      )}
      {toast && <div className="toast">{toast}</div>}
    </div>
  )
}

function NotificationDialog({ settings, loading, saving, testing, pushBusy, pushSupported, pushSubscribed,
  pushPermission, error, onSettingsChange, onSave, onTest, onPushToggle, onPushTest, onClose }: {
  settings: NotificationSettings | null
  loading: boolean
  saving: boolean
  testing: boolean
  pushBusy: boolean
  pushSupported: boolean
  pushSubscribed: boolean
  pushPermission: NotificationPermission
  error: string
  onSettingsChange: (settings: NotificationSettings | null) => void
  onSave: (event: FormEvent<HTMLFormElement>) => void
  onTest: () => void
  onPushToggle: () => void
  onPushTest: () => void
  onClose: () => void
}) {
  const busy = loading || saving || testing || pushBusy
  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <form className="modal notification-modal" onMouseDown={event => event.stopPropagation()} onSubmit={onSave}>
        <div className="modal-head">
          <div><small>BUILD NOTIFICATIONS</small><h2>Notifikasi build</h2></div>
          <button type="button" className="icon-btn" onClick={onClose}><X size={18}/></button>
        </div>
        {loading ? <p className="notification-info">memuat pengaturan…</p> : settings && (
          <>
            <div className={`notification-info ${settings.smtpConfigured ? 'configured' : 'not-configured'}`}>
              {settings.smtpConfigured
                ? 'SMTP siap. Kredensial pengirim disimpan di data/IDE.config.'
                : 'SMTP belum dikonfigurasi. Isi bagian smtp di data/IDE.config lalu restart service.'}
            </div>
            <label>Email penerima
              <input type="email" autoComplete="email" maxLength={320} value={settings.recipient}
                onChange={event => onSettingsChange({ ...settings, recipient: event.target.value })}
                placeholder="lu@example.com"/>
            </label>
            <div className="notification-options">
              <label><input type="checkbox" checked={settings.notifySuccess}
                onChange={event => onSettingsChange({ ...settings, notifySuccess: event.target.checked })}/>
                <span><strong>Build berhasil</strong><small>kirim notifikasi saat APK selesai dibuat</small></span></label>
              <label><input type="checkbox" checked={settings.notifyFailure}
                onChange={event => onSettingsChange({ ...settings, notifyFailure: event.target.checked })}/>
                <span><strong>Build gagal</strong><small>kirim notifikasi saat build gagal</small></span></label>
            </div>
            <section className="push-settings">
              <div><strong>Browser push</strong>
                <small>{!pushSupported
                  ? 'browser ini belum mendukung push notification'
                  : pushPermission === 'denied'
                    ? 'izin notifikasi diblokir di pengaturan browser'
                    : pushSubscribed ? 'browser ini siap menerima notifikasi build'
                      : 'notifikasi bisa diterima saat IDE tidak sedang dibuka'}</small>
              </div>
              <div className="push-actions">
                <button type="button" className="tool-btn refresh-storage" onClick={onPushToggle}
                  disabled={busy || !pushSupported || pushPermission === 'denied'}>
                  {pushBusy ? 'memproses…' : pushSubscribed ? 'nonaktifkan' : 'aktifkan'}
                </button>
                <button type="button" className="tool-btn refresh-storage" onClick={onPushTest}
                  disabled={busy || !pushSubscribed}>
                  kirim test
                </button>
              </div>
            </section>
            <div className="notification-info not-configured">
              {settings.smtpConfigured
                ? 'email juga aktif; kredensial pengirim disimpan di data/IDE.config.'
                : 'email opsional: isi smtp di data/IDE.config untuk mengaktifkannya.'}
            </div>
          </>
        )}
        {error && <div className="login-error">{error}</div>}
        <div className="notification-actions">
          <button type="button" className="tool-btn refresh-storage" onClick={onTest}
            disabled={busy || !settings?.smtpConfigured || !settings.recipient}>
            {testing ? 'mengirim…' : 'kirim email test'}
          </button>
          <button type="submit" className="primary-btn" disabled={busy || !settings}>
            {saving ? 'menyimpan…' : 'simpan'}
          </button>
        </div>
      </form>
    </div>
  )
}

function LoginScreen({ loading, offline, error, password, busy, onPasswordChange, onSubmit }: {
  loading?: boolean
  offline?: boolean
  error: string
  password: string
  busy: boolean
  onPasswordChange: (value: string) => void
  onSubmit: (event: FormEvent<HTMLFormElement>) => void
}) {
  return (
    <main className="login-screen">
      <form className="login-card" onSubmit={onSubmit}>
        <div className="brand-mark"><Box size={20}/></div>
        <small>SELF-HOSTED ANDROID IDE</small>
        <h1>{loading ? 'menghubungkan…' : 'private workspace'}</h1>
        <p>{offline ? 'login perlu koneksi. akses offline tersedia setelah lu pernah login di browser ini.' : 'masukin password buat lanjut ke workspace.'}</p>
        <label>password<input type="password" autoComplete="current-password" value={password}
          onChange={event => onPasswordChange(event.target.value)} autoFocus disabled={loading || offline} required/></label>
        {error && <div className="login-error">{error}</div>}
        <button className="primary-btn full" type="submit" disabled={busy || loading || offline || !password}>
          {busy ? 'memeriksa…' : 'masuk'}
        </button>
      </form>
    </main>
  )
}

function StorageDialog({ stats, loading, options, onOptionChange, onRefresh, onCleanup, onClose }: {
  stats: StorageStats | null
  loading: boolean
  options: CleanupOptions
  onOptionChange: (key: keyof CleanupOptions, checked: boolean) => void
  onRefresh: () => void
  onCleanup: () => void
  onClose: () => void
}) {
  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className="modal storage-modal" onMouseDown={event => event.stopPropagation()}>
        <div className="modal-head">
          <div><small>SERVER STORAGE</small><h2>Build cache &amp; data</h2></div>
          <button className="icon-btn" onClick={onClose}><X size={18}/></button>
        </div>
        <div className="storage-stats">
          <div><span>Build workspaces</span><strong>{stats ? formatBytes(stats.workspaceBytes) : '—'}</strong></div>
          <div><span>APK artifacts</span><strong>{stats ? formatBytes(stats.artifactBytes) : '—'}</strong></div>
          <div><span>Binary asset cache</span><strong>{stats ? formatBytes(stats.blobBytes) : '—'}</strong></div>
          <div><span>Gradle cache · Docker volume</span><strong>{stats ? formatBytes(stats.gradleCacheBytes) : '—'}</strong></div>
          <div><span>Database · {stats?.buildHistory ?? 0} build records</span><strong>{stats ? formatBytes(stats.databaseBytes) : '—'}</strong></div>
        </div>
        <p className="storage-paths">lokasi: <code>DATA_DIR/workspaces</code>, <code>DATA_DIR/artifacts</code>, dan volume <code>androidide_gradle</code>. project di IndexedDB browser tidak ikut dihapus.</p>
        <div className="cleanup-options">
          <label><input type="checkbox" checked={options.workspaces} onChange={event => onOptionChange('workspaces', event.target.checked)}/><span><strong>workspace lama</strong><small>hapus source ZIP dan output build yang tertinggal</small></span></label>
          <label><input type="checkbox" checked={options.artifacts} onChange={event => onOptionChange('artifacts', event.target.checked)}/><span><strong>APK artifacts</strong><small>APK terhapus dan tidak bisa didownload lagi</small></span></label>
          <label><input type="checkbox" checked={options.history} onChange={event => onOptionChange('history', event.target.checked)}/><span><strong>riwayat dan log build</strong><small>melepas binary cache terkait lalu menjalankan garbage collection</small></span></label>
          <label><input type="checkbox" checked={options.gradleCache} onChange={event => onOptionChange('gradleCache', event.target.checked)}/><span><strong>Gradle dependency cache</strong><small>build berikutnya perlu download dependency lagi</small></span></label>
        </div>
        <div className="storage-actions">
          <button className="tool-btn refresh-storage" onClick={onRefresh} disabled={loading}>{loading ? 'memuat…' : 'refresh ukuran'}</button>
          <button className="danger-btn" onClick={onCleanup} disabled={loading}>{loading ? 'membersihkan…' : 'bersihkan yang dipilih'}</button>
        </div>
      </div>
    </div>
  )
}

function ArtifactDialog({ artifacts, projects, loading, deletingId, error, onRefresh, onDelete, onClose }: {
  artifacts: APKArtifact[]
  projects: Project[]
  loading: boolean
  deletingId: string | null
  error: string
  onRefresh: () => void
  onDelete: (artifact: APKArtifact) => void
  onClose: () => void
}) {
  const projectNames = new Map(projects.map(project => [project.id, project.name]))
  const grouped = new Map<string, APKArtifact[]>()
  for (const artifact of artifacts) {
    const items = grouped.get(artifact.projectId) ?? []
    items.push(artifact)
    grouped.set(artifact.projectId, items)
  }

  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className="modal artifact-modal" onMouseDown={event => event.stopPropagation()}>
        <div className="modal-head">
          <div><small>BUILD ARTIFACTS</small><h2>APK tersimpan</h2></div>
          <div className="artifact-head-actions">
            <button className="icon-btn" title="Muat ulang" aria-label="Muat ulang" onClick={onRefresh} disabled={loading}>
              <RefreshCw size={16}/>
            </button>
            <button className="icon-btn" title="Tutup" aria-label="Tutup" onClick={onClose}><X size={18}/></button>
          </div>
        </div>
        <div className="artifact-summary">
          {loading ? 'memuat daftar APK…' : `${artifacts.length} APK di ${grouped.size} project`}
        </div>
        {error && <div className="login-error">{error}</div>}
        {!loading && artifacts.length === 0 && !error && (
          <div className="artifact-empty">belum ada APK hasil build</div>
        )}
        <div className="artifact-groups">
          {[...grouped.entries()].map(([projectId, items]) => (
            <section className="artifact-group" key={projectId}>
              <div className="artifact-project">
                <strong>{projectNames.get(projectId) ?? projectId}</strong>
                <span>{items.length} APK</span>
              </div>
              {items.map(artifact => (
                <div className="artifact-row" key={artifact.buildId}>
                  <div className="artifact-info">
                    <strong>{artifact.filename}</strong>
                    <span>{artifact.variant} · {new Date(artifact.createdAt).toLocaleString()} · {formatBytes(artifact.size)}</span>
                    {!artifact.available && <small className="artifact-missing">file APK tidak tersedia di server</small>}
                  </div>
                  <div className="artifact-actions">
                    {artifact.available
                      ? <a className="icon-btn" title="Download APK" aria-label="Download APK"
                          href={`/api/builds/${encodeURIComponent(artifact.buildId)}/artifact`} download>
                          <Download size={16}/>
                        </a>
                      : <button className="icon-btn" title="APK tidak tersedia" aria-label="APK tidak tersedia" disabled><Download size={16}/></button>}
                    <button className="icon-btn artifact-delete" title="Hapus APK" aria-label="Hapus APK"
                      onClick={() => onDelete(artifact)} disabled={deletingId !== null}>
                      <Trash2 size={16}/>
                    </button>
                  </div>
                </div>
              ))}
            </section>
          ))}
        </div>
      </div>
    </div>
  )
}

function CreateProjectModal({ onClose, onCreate }: {
  onClose: () => void
  onCreate: (project: Omit<Project, 'id' | 'createdAt' | 'updatedAt' | 'templateId'>) => void
}) {
  const [name, setName] = useState('My App')
  const [packageName, setPackageName] = useState('com.example.myapp')
  const [language, setLanguage] = useState<'kotlin' | 'java'>('kotlin')
  const [minSdk, setMinSdk] = useState(24)
  const [targetSdk, setTargetSdk] = useState(34)
  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className="modal" onMouseDown={e => e.stopPropagation()}>
        <div className="modal-head"><div><small>NEW PROJECT</small><h2>Empty App</h2></div><button className="icon-btn" onClick={onClose}><X size={18}/></button></div>
        <label>App name<input value={name} onChange={e => setName(e.target.value)}/></label>
        <label>Package name<input value={packageName} onChange={e => setPackageName(e.target.value)} placeholder="com.example.myapp"/></label>
        <div className="form-row"><label>Language<select value={language} onChange={e => setLanguage(e.target.value as 'kotlin' | 'java')}><option value="kotlin">Kotlin</option><option value="java">Java</option></select></label><label>Template<select><option>Empty App</option></select></label></div>
        <div className="form-row"><label>Min SDK<input type="number" value={minSdk} onChange={e => setMinSdk(Number(e.target.value))}/></label><label>Target SDK<input type="number" value={targetSdk} onChange={e => setTargetSdk(Number(e.target.value))}/></label></div>
        <button className="primary-btn full" disabled={!name.trim() || !/^([a-zA-Z_]\w*\.)+[a-zA-Z_]\w*$/.test(packageName)}
          onClick={() => onCreate({ name: name.trim(), packageName: packageName.trim(), language, minSdk, targetSdk })}>Buat project</button>
      </div>
    </div>
  )
}

export default App
