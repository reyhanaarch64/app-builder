import { useEffect, useMemo, useRef, useState } from 'react'
import Editor from '@monaco-editor/react'
import Dexie, { type Table } from 'dexie'
import JSZip from 'jszip'
import {
  Box, ChevronRight, Download, FileCode2, FilePlus2, FolderPlus,
  Menu, PackagePlus, Play, Search, Trash2, Upload,
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
  size: number
  createdAt: number
  updatedAt: number
}

type Build = {
  id: string
  projectId: string
  status: 'queued' | 'building' | 'success' | 'failed' | 'canceled'
  variant: string
  createdAt: string
  error?: string
  artifact?: string
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
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [buildOpen, setBuildOpen] = useState(false)
  const [buildStatus, setBuildStatus] = useState<'idle' | 'queued' | 'building' | 'success' | 'failed' | 'canceled'>('idle')
  const [logs, setLogs] = useState<string[]>([])
  const [buildId, setBuildId] = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [search, setSearch] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set(['app', 'app/src', 'app/src/main']))
  const [toast, setToast] = useState('')
  const autoSaveTimers = useRef(new Map<string, number>())
  const buildEvents = useRef<EventSource | null>(null)
  const [buildHistory, setBuildHistory] = useState<Build[]>([])
  const [restoreProjectId] = useState(() => localStorage.getItem('android-web-ide.activeProjectId'))
  const activeProject = projects.find(p => p.id === activeProjectId) ?? null
  const activeFile = files.find(f => f.id === activeFileId) ?? null

  useEffect(() => {
    db.projects.orderBy('updatedAt').reverse().toArray().then(items => {
      setProjects(items)
      if (restoreProjectId && items.some(project => project.id === restoreProjectId)) setActiveProjectId(restoreProjectId)
    })
  }, [restoreProjectId])

  useEffect(() => {
    if (activeProjectId) localStorage.setItem('android-web-ide.activeProjectId', activeProjectId)
    else localStorage.removeItem('android-web-ide.activeProjectId')
  }, [activeProjectId])

  useEffect(() => {
    if (!activeProjectId) { setFiles([]); return }
    db.files.where('projectId').equals(activeProjectId).toArray().then(setFiles)
  }, [activeProjectId])

  useEffect(() => {
    buildEvents.current?.close()
    buildEvents.current = null
    if (!activeProjectId) {
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
  }, [activeProjectId])

  useEffect(() => {
    if (!toast) return
    const timer = window.setTimeout(() => setToast(''), 2800)
    return () => window.clearTimeout(timer)
  }, [toast])

  const tree = useMemo(() => buildTree(
    search ? files.filter(f => f.path.toLowerCase().includes(search.toLowerCase())) : files
  ), [files, search])

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
    const updated = { ...file, content, size: new Blob([content]).size, updatedAt: Date.now() }
    await db.files.put(updated)
    setFiles(prev => prev.map(f => f.id === file.id ? updated : f))
    setOpenTabs(prev => prev.map(f => f.id === file.id ? updated : f))
  }

  function editFile(content: string | undefined) {
    if (!activeFile || content === undefined) return
    setFiles(prev => prev.map(f => f.id === activeFile.id ? { ...f, content } : f))
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
      events.close()
      if (buildEvents.current === events) buildEvents.current = null
      fetch(`/api/builds?projectId=${encodeURIComponent(build.projectId)}`)
        .then(response => response.ok ? response.json() as Promise<Build[]> : [])
        .then(setBuildHistory)
        .catch(() => undefined)
    })
  }

  async function startBuild() {
    if (!activeProject) return
    if (buildStatus === 'queued' || buildStatus === 'building') {
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
    setBuildStatus('queued')
    setLogs(['Memvalidasi dan mengemas project...'])
    const zip = new JSZip()
    for (const file of buildFiles) {
      if (file.binary) zip.file(file.path, file.binary)
      else zip.file(file.path, file.content ?? '')
    }
    const blob = await zip.generateAsync({ type: 'blob' })
    if (blob.size > 50 * 1024 * 1024) {
      setBuildStatus('failed')
      setLogs(prev => [...prev, 'ZIP project melebihi batas 50 MB'])
      return
    }
    const form = new FormData()
    form.append('project', blob, `${activeProject.name}.zip`)
    form.append('projectId', activeProject.id)
    try {
      const response = await fetch('/api/builds', { method: 'POST', body: form })
      const body = await response.text()
      let data: { error?: string; build?: Build } & Partial<Build>
      try { data = JSON.parse(body) as typeof data }
      catch { data = { error: body || `request gagal (${response.status})` } }
      if (response.status === 409 && data.build) {
        setToast('Project ini masih punya build aktif')
        followBuild(data.build as Build)
        return
      }
      if (!response.ok) throw new Error(data.error || `Gagal mengirim build (${response.status})`)
      setBuildHistory(prev => [data as Build, ...prev])
      followBuild(data as Build)
    } catch (error) {
      setBuildStatus('failed')
      setLogs(prev => [...prev, error instanceof Error ? error.message : 'Gagal upload build'])
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
                next.has(node.path) ? next.delete(node.path) : next.add(node.path)
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

  return (
    <div className="app-shell">
      <header className="topbar">
        <button className="icon-btn" onClick={() => setDrawerOpen(v => !v)}><Menu size={20}/></button>
        <div className="brand"><div className="brand-mark"><Box size={18}/></div><span>Droid Studio</span></div>
        {activeProject && <div className="project-title">{activeProject.name}</div>}
        <button className="build-btn" onClick={startBuild} disabled={!activeProject || buildStatus === 'queued' || buildStatus === 'building'}>
          <Play size={16} fill="currentColor"/><span>{buildStatus === 'queued' || buildStatus === 'building' ? 'Building...' : 'Build APK'}</span>
        </button>
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
            <div className="home-head"><div><small>ANDROID PROJECTS</small><h1>Workspace</h1></div><button className="primary-btn" onClick={() => setShowCreate(true)}><PackagePlus size={17}/>Project baru</button></div>
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
          <div className="build-head"><div><small>BUILD STATUS</small><strong>{buildStatus === 'idle' ? 'Debug APK' : buildStatus}</strong></div><button className="icon-btn" onClick={() => setBuildOpen(false)}><X size={18}/></button></div>
          {buildHistory.length > 1 && <div className="build-history"><label>Riwayat build<select value={buildId ?? ''} onChange={event => {
            const build = buildHistory.find(item => item.id === event.target.value)
            if (build) followBuild(build)
          }}>{buildHistory.map(build => <option key={build.id} value={build.id}>{build.status} · {new Date(build.createdAt).toLocaleString()}</option>)}</select></label></div>}
          <div className={`status-line ${buildStatus}`}><span></span>{buildStatus === 'building' ? 'Building...' : buildStatus === 'success' ? 'Build berhasil' : buildStatus === 'failed' ? 'Build gagal' : buildStatus === 'canceled' ? 'Build dibatalkan' : 'Menunggu worker'}</div>
          <div className="log-view">{logs.map((line, i) => <div key={i}><span>{String(i + 1).padStart(3, '0')}</span>{line}</div>)}</div>
          {buildStatus === 'success' && buildId && <a className="download-btn" href={`/api/builds/${buildId}/artifact`} download><Download size={17}/>Download APK</a>}
        </div>
      )}
      {toast && <div className="toast">{toast}</div>}
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
