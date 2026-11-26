import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import Editor from '@monaco-editor/react'
import type { editor as MonacoEditor } from 'monaco-editor'
import Dexie, { type Table } from 'dexie'
import JSZip from 'jszip'
import {
  Activity, AlertCircle, Archive, ArrowLeft, Ban, Box, ChevronRight, Copy, Download, FileCode2,
  FilePlus2, FileText, FolderPlus, Gauge, HardDrive, Mail, LogOut, Menu, PackagePlus, Play, RefreshCw,
  Search, Server, Smartphone, Trash2, Upload, WifiOff,
  X,
} from 'lucide-react'
import './App.css'

type ProjectType = 'android' | 'java-library'
type BuildVariant = 'debug' | 'release' | 'jar' | 'fat-jar'

type Project = {
  iconResource?: string
  id: string
  name: string
  projectType?: ProjectType
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

type EditorDiagnostic = {
  fileId: string
  path: string
  line: number
  column: number
  message: string
}

type SearchResult = {
  fileId: string
  path: string
  line: number
  column: number
  preview: string
}

type Build = {
  id: string
  projectId: string
  status: 'preparing' | 'assembling' | 'queued' | 'building' | 'success' | 'failed' | 'canceled'
  variant: BuildVariant
  createdAt: string
  error?: string
  artifact?: string
  startedAt?: string
  finishedAt?: string
}

type HostResourceMetrics = {
  cpuPercent: number
  memoryUsedBytes: number
  memoryTotalBytes: number
  memoryAvailableBytes: number
  memoryPercent: number
  sampledAt: string
}

type BuildResourceMetrics = {
  cpuPercent: number
  memoryUsedBytes: number
  memoryLimitBytes: number
  memoryPercent: number
  peakCpuPercent: number
  peakMemoryUsedBytes: number
  sampledAt: string
}

type BuildMonitor = {
  buildId: string
  status: Build['status']
  variant: BuildVariant
  error?: string
  metrics?: BuildResourceMetrics
}

type MetricHistoryPoint = { cpu: number; memory: number }

type BuildOptions = {
  variant: BuildVariant
  keystore?: File
  keyAlias?: string
  storePassword?: string
  keyPassword?: string
}

type BuildArtifact = {
  buildId: string
  projectId: string
  variant: string
  projectType?: ProjectType
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

type AppDialogRequest = {
  kind: 'confirm' | 'prompt' | 'alert'
  title: string
  message: string
  confirmLabel?: string
  cancelLabel?: string
  placeholder?: string
  initialValue?: string
  destructive?: boolean
}

const editorStateKey = (projectId: string) => `android-web-ide.editorState.${projectId}`
const accessModeKey = 'android-web-ide.accessMode'
const offlineUnlockKey = 'android-web-ide.offlineUnlock'
const defaultExpandedPaths = ['app', 'app/src', 'app/src/main']

function projectTypeOf(project: Project): ProjectType {
  return project.projectType ?? 'android'
}

function editorDiagnostics(file: ProjectFile): EditorDiagnostic[] {
  const source = file.content
  if (source === undefined) return []
  const extension = file.path.split('.').pop()?.toLowerCase()
  const diagnostic = (line: number, column: number, message: string): EditorDiagnostic => ({
    fileId: file.id, path: file.path, line, column, message,
  })

  if (extension === 'json') {
    try {
      JSON.parse(source)
      return []
    } catch (error) {
      const message = error instanceof Error ? error.message : 'JSON tidak valid.'
      const position = message.match(/position\s+(\d+)/i)
      const lineColumn = message.match(/line\s+(\d+)\s+column\s+(\d+)/i)
      let line = Number(lineColumn?.[1] ?? 1)
      let column = Number(lineColumn?.[2] ?? 1)
      if (position) {
        const beforeError = source.slice(0, Number(position[1]))
        line = beforeError.split('\n').length
        column = beforeError.length - beforeError.lastIndexOf('\n')
      }
      return [diagnostic(line, column, message)]
    }
  }

  if (extension === 'xml') {
    const parsed = new DOMParser().parseFromString(source, 'application/xml')
    const parseError = parsed.getElementsByTagName('parsererror')[0]
    if (!parseError) return []
    const message = parseError.textContent?.trim().replace(/\s+/g, ' ') || 'XML tidak valid.'
    const location = message.match(/line\s+(\d+).*?(?:column|col)\s+(\d+)/i)
    return [diagnostic(Number(location?.[1] ?? 1), Number(location?.[2] ?? 1), message.slice(0, 240))]
  }

  if (!['java', 'kt', 'kts', 'gradle'].includes(extension ?? '')) return []
  const errors: EditorDiagnostic[] = []
  const stack: { token: string; line: number; column: number }[] = []
  let line = 1
  let column = 1
  let index = 0
  let state: 'code' | 'line-comment' | 'block-comment' | 'string' | 'multiline-string' = 'code'
  let quote = ''
  let blockCommentDepth = 0
  let stringStart = { line: 1, column: 1 }
  let commentStart = { line: 1, column: 1 }
  const closers: Record<string, string> = { ')': '(', ']': '[', '}': '{' }

  const advance = (count = 1) => {
    for (let step = 0; step < count && index < source.length; step++) {
      if (source[index] === '\n') {
        line++
        column = 1
      } else {
        column++
      }
      index++
    }
  }

  while (index < source.length && errors.length < 100) {
    const char = source[index]
    const next = source[index + 1]
    if (state === 'line-comment') {
      if (char === '\n') state = 'code'
      advance()
      continue
    }
    if (state === 'block-comment') {
      if (char === '/' && next === '*' && (extension === 'kt' || extension === 'kts')) {
        blockCommentDepth++
        advance(2)
      } else if (char === '*' && next === '/') {
        blockCommentDepth--
        if (blockCommentDepth === 0) state = 'code'
        advance(2)
      } else {
        advance()
      }
      continue
    }
    if (state === 'multiline-string') {
      if (source.startsWith('"""', index)) {
        state = 'code'
        advance(3)
      } else {
        advance()
      }
      continue
    }
    if (state === 'string') {
      if (char === '\\') {
        advance(Math.min(2, source.length - index))
      } else if (char === quote) {
        state = 'code'
        advance()
      } else if (char === '\n') {
        errors.push(diagnostic(stringStart.line, stringStart.column, 'String tidak ditutup sebelum akhir baris.'))
        state = 'code'
        advance()
      } else {
        advance()
      }
      continue
    }

    if (char === '/' && next === '/') {
      state = 'line-comment'
      commentStart = { line, column }
      advance(2)
    } else if (char === '/' && next === '*') {
      state = 'block-comment'
      blockCommentDepth = 1
      commentStart = { line, column }
      advance(2)
    } else if (source.startsWith('"""', index)) {
      state = 'multiline-string'
      stringStart = { line, column }
      advance(3)
    } else if (char === '"' || char === "'") {
      state = 'string'
      quote = char
      stringStart = { line, column }
      advance()
    } else if ('([{'.includes(char)) {
      stack.push({ token: char, line, column })
      advance()
    } else if (closers[char]) {
      const opening = stack.at(-1)
      if (!opening || opening.token !== closers[char]) {
        errors.push(diagnostic(line, column, `Kurung "${char}" tidak punya pasangan pembuka yang sesuai.`))
      } else {
        stack.pop()
      }
      advance()
    } else {
      advance()
    }
  }

  if (state === 'string' || state === 'multiline-string') {
    errors.push(diagnostic(stringStart.line, stringStart.column, 'String tidak ditutup.'))
  } else if (state === 'block-comment') {
    errors.push(diagnostic(commentStart.line, commentStart.column, 'Komentar blok tidak ditutup.'))
  }
  for (const opening of stack) {
    if (errors.length >= 100) break
    errors.push(diagnostic(opening.line, opening.column, `Kurung "${opening.token}" tidak ditutup.`))
  }
  return errors
}

function findProjectMatches(
  files: ProjectFile[],
  activeFileId: string,
  scope: 'file' | 'project',
  query: string,
  limit = 200,
): SearchResult[] {
  const needle = query.trim().toLocaleLowerCase()
  if (!needle) return []
  const results: SearchResult[] = []
  const candidates = scope === 'file'
    ? files.filter(file => file.id === activeFileId)
    : files
  for (const file of candidates) {
    if (file.content === undefined) continue
    const lines = file.content.split('\n')
    for (let index = 0; index < lines.length; index++) {
      const lowered = lines[index].toLocaleLowerCase()
      let column = lowered.indexOf(needle)
      while (column !== -1) {
        results.push({
          fileId: file.id, path: file.path, line: index + 1,
          column: column + 1, preview: lines[index].trim().slice(0, 240),
        })
        if (results.length >= limit) return results
        column = lowered.indexOf(needle, column + needle.length)
      }
    }
  }
  return results
}

function manifestIconResource(manifest: string | undefined): string | undefined {
  const application = manifest?.match(/<application\b[^>]*>/i)?.[0]
  if (!application) return undefined
  const icon = application.match(/android:icon=["']@(mipmap|drawable)\/([^"']+)["']/i)
  const roundIcon = application.match(/android:roundIcon=["']@(mipmap|drawable)\/([^"']+)["']/i)
  const resource = icon ?? roundIcon
  return resource ? `@${resource[1].toLowerCase()}/${resource[2]}` : undefined
}

const androidDefaultIcon = `data:image/svg+xml,${encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><g fill="#3ddc84"><path d="M14.4 16.4 11.6 12a.8.8 0 0 1 1.4-.8l2.9 4.5a15 15 0 0 1 16.2 0l2.9-4.5a.8.8 0 1 1 1.4.8l-2.8 4.4A13 13 0 0 1 39 27H9a13 13 0 0 1 5.4-10.6ZM15 21a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Zm18 0a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3ZM9 29h30v5a3 3 0 0 1-3 3h-1v5a2 2 0 0 1-4 0v-5H17v5a2 2 0 0 1-4 0v-5h-1a3 3 0 0 1-3-3v-5Zm-5 0a2 2 0 0 1 4 0v7a2 2 0 0 1-4 0v-7Zm36 0a2 2 0 0 1 4 0v7a2 2 0 0 1-4 0v-7Z"/></g></svg>',
)}`

function svgDataURL(svg: string): string {
  return `data:image/svg+xml,${encodeURIComponent(svg)}`
}

function androidAttribute(element: Element, name: string): string | null {
  return element.getAttribute(`android:${name}`) ??
    element.getAttributeNS('http://schemas.android.com/apk/res/android', name)
}

function androidColorToCSS(color: string): string {
  const hex = color.match(/^#([0-9a-f]{3,8})$/i)?.[1]
  if (!hex) return color
  if (hex.length === 4) {
    const [alpha, red, green, blue] = hex
    return `#${red}${red}${green}${green}${blue}${blue}${alpha}${alpha}`
  }
  if (hex.length === 8) return `#${hex.slice(2)}${hex.slice(0, 2)}`
  return color
}

function vectorDrawableDataURL(source: string, colors: Map<string, string> = new Map()): string | null {
  const document = new DOMParser().parseFromString(source, 'application/xml')
  const root = document.documentElement
  if (root.localName !== 'vector') return null
  const width = Number.parseFloat(androidAttribute(root, 'width') ?? '24')
  const height = Number.parseFloat(androidAttribute(root, 'height') ?? '24')
  const viewportWidth = Number.parseFloat(androidAttribute(root, 'viewportWidth') ?? String(width))
  const viewportHeight = Number.parseFloat(androidAttribute(root, 'viewportHeight') ?? String(height))
  if (![width, height, viewportWidth, viewportHeight].every(Number.isFinite) ||
      width <= 0 || height <= 0 || viewportWidth <= 0 || viewportHeight <= 0) return null

  const renderNode = (node: Element): string => {
    if (node.localName === 'path') {
      const pathData = androidAttribute(node, 'pathData')
      if (!pathData) return ''
      const fill = androidAttribute(node, 'fillColor')
      const stroke = androidAttribute(node, 'strokeColor')
      const fillAlpha = Number.parseFloat(androidAttribute(node, 'fillAlpha') ?? '1')
      const strokeAlpha = Number.parseFloat(androidAttribute(node, 'strokeAlpha') ?? '1')
      const fillType = androidAttribute(node, 'fillType')
      const fillColor = fill?.startsWith('@color/') ? colors.get(fill.slice('@color/'.length)) : fill
      const strokeColor = stroke?.startsWith('@color/') ? colors.get(stroke.slice('@color/'.length)) : stroke
      const resolvedFill = fillColor ? androidColorToCSS(fillColor) : fillColor
      const resolvedStroke = strokeColor ? androidColorToCSS(strokeColor) : strokeColor
      const attributes = [
        `d="${pathData.replaceAll('&', '&amp;').replaceAll('"', '&quot;')}"`,
        resolvedFill && !resolvedFill.startsWith('@') && !resolvedFill.startsWith('?') ? `fill="${resolvedFill}"` : 'fill="none"',
        resolvedStroke && !resolvedStroke.startsWith('@') && !resolvedStroke.startsWith('?') ? `stroke="${resolvedStroke}"` : '',
        androidAttribute(node, 'strokeWidth') ? `stroke-width="${androidAttribute(node, 'strokeWidth')}"` : '',
        Number.isFinite(fillAlpha) && fillAlpha < 1 ? `fill-opacity="${fillAlpha}"` : '',
        Number.isFinite(strokeAlpha) && strokeAlpha < 1 ? `stroke-opacity="${strokeAlpha}"` : '',
        fillType === 'evenOdd' ? 'fill-rule="evenodd"' : '',
      ].filter(Boolean).join(' ')
      return `<path ${attributes}/>`
    }
    if (node.localName === 'group') {
      const rotation = Number.parseFloat(androidAttribute(node, 'rotation') ?? '0')
      const pivotX = Number.parseFloat(androidAttribute(node, 'pivotX') ?? '0')
      const pivotY = Number.parseFloat(androidAttribute(node, 'pivotY') ?? '0')
      const scaleX = Number.parseFloat(androidAttribute(node, 'scaleX') ?? '1')
      const scaleY = Number.parseFloat(androidAttribute(node, 'scaleY') ?? '1')
      const translateX = Number.parseFloat(androidAttribute(node, 'translateX') ?? '0')
      const translateY = Number.parseFloat(androidAttribute(node, 'translateY') ?? '0')
      const transform = `translate(${translateX} ${translateY}) translate(${pivotX} ${pivotY}) rotate(${rotation}) scale(${scaleX} ${scaleY}) translate(${-pivotX} ${-pivotY})`
      return `<g transform="${transform}">${Array.from(node.children, renderNode).join('')}</g>`
    }
    if (node.localName === 'clip-path') {
      const pathData = androidAttribute(node, 'pathData')
      return pathData ? `<path d="${pathData.replaceAll('&', '&amp;').replaceAll('"', '&quot;')}" clip-rule="evenodd"/>` : ''
    }
    return Array.from(node.children, renderNode).join('')
  }
  const paths = Array.from(root.children, renderNode).join('')
  return svgDataURL(`<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${viewportWidth} ${viewportHeight}">${paths}</svg>`)
}

const resourceDirectories = [
  'mipmap-anydpi-v26', 'mipmap-anydpi', 'mipmap-xxxhdpi', 'mipmap-xxhdpi',
  'mipmap-xhdpi', 'mipmap-hdpi', 'mipmap-mdpi', 'mipmap-nodpi', 'mipmap',
  'drawable-anydpi-v26', 'drawable-anydpi', 'drawable-xxxhdpi', 'drawable-xxhdpi',
  'drawable-xhdpi', 'drawable-hdpi', 'drawable-mdpi', 'drawable-nodpi', 'drawable',
]
const resourceExtensions = ['xml', 'png', 'webp', 'jpg', 'jpeg']

async function projectIconURL(project: Project): Promise<string | null> {
  const match = project.iconResource?.match(/^@(mipmap|drawable)\/([A-Za-z0-9_]+)$/)
  if (!match) return null

  const getFile = async (type: 'mipmap' | 'drawable', name: string) => {
    const paths = resourceDirectories
      .filter(directory => directory.startsWith(type))
      .flatMap(directory => resourceExtensions.map(extension => `app/src/main/res/${directory}/${name}.${extension}`))
    const files = await db.files.where('[projectId+path]').anyOf(paths.map(path => [project.id, path])).toArray()
    const byPath = new Map(files.map(file => [file.path, file]))
    return paths.map(path => byPath.get(path)).find(Boolean)
  }

  const colorNames = new Set<string>()
  const resolveColor = async (name: string): Promise<string | undefined> => {
    const paths = ['values', 'values-night', 'values-v21', 'values-night-v21']
      .flatMap(directory => [`app/src/main/res/${directory}/colors.xml`, `app/src/main/res/${directory}/${name}.xml`])
    const files = await db.files.where('[projectId+path]').anyOf(paths.map(path => [project.id, path])).toArray()
    for (const file of files) {
      if (!file.content) continue
      const document = new DOMParser().parseFromString(file.content, 'application/xml')
      const color = Array.from(document.getElementsByTagName('color'))
        .find(element => element.getAttribute('name') === name)?.textContent?.trim()
      if (color) return color
    }
    return undefined
  }

  const toDataURL = async (type: 'mipmap' | 'drawable', name: string, seen = new Set<string>()): Promise<string | null> => {
    const key = `${type}/${name}`
    if (seen.has(key)) return null
    seen.add(key)
    const file = await getFile(type, name)
    if (!file) return null
    if (!file.path.endsWith('.xml')) {
      if (!file.binary) return null
      const extension = file.path.split('.').pop()?.toLowerCase() ?? ''
      const mimeType = ({
        png: 'image/png', webp: 'image/webp', jpg: 'image/jpeg', jpeg: 'image/jpeg',
      }[extension] ?? file.binary.type)
      if (!mimeType) return null
      const binary = file.binary
      return new Promise((resolve, reject) => {
        const reader = new FileReader()
        reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : null)
        reader.onerror = () => reject(reader.error ?? new Error('Gagal membaca berkas ikon proyek.'))
        reader.readAsDataURL(new Blob([binary], { type: mimeType }))
      })
    }

    const source = file.content
    if (!source) return null
    const document = new DOMParser().parseFromString(source, 'application/xml')
    const root = document.documentElement
    if (root.localName === 'vector') {
      const references = [...source.matchAll(/@color\/([A-Za-z0-9_]+)/g)].map(reference => reference[1])
      await Promise.all(references.map(async color => {
        if (colorNames.has(color)) return
        colorNames.add(color)
        const value = await resolveColor(color)
        if (value) colorValues.set(color, value)
      }))
      return vectorDrawableDataURL(source, colorValues)
    }
    if (root.localName !== 'adaptive-icon') return null

    const drawable = (element: Element | undefined) => element ? androidAttribute(element, 'drawable') : null
    const background = drawable(Array.from(root.children).find(child => child.localName === 'background'))
    const foreground = drawable(Array.from(root.children).find(child => child.localName === 'foreground'))
    const resolveReference = async (reference: string | null): Promise<string | null> => {
      const resource = reference?.match(/^@(mipmap|drawable)\/([A-Za-z0-9_]+)$/)
      if (resource) {
        const resourceType = resource[1] === 'mipmap' ? 'mipmap' : 'drawable'
        return toDataURL(resourceType, resource[2], seen)
      }
      const color = reference?.match(/^@color\/([A-Za-z0-9_]+)$/)
      if (color) {
        const value = await resolveColor(color[1])
        return value ? androidColorToCSS(value) : null
      }
      return reference && /^#[\da-f]{3,8}$/i.test(reference) ? androidColorToCSS(reference) : null
    }
    const [backgroundValue, foregroundValue] = await Promise.all([
      resolveReference(background), resolveReference(foreground),
    ])
    if (!backgroundValue && !foregroundValue) return null
    const backgroundSVG = backgroundValue?.startsWith('#')
      ? `<rect width="100%" height="100%" fill="${backgroundValue}"/>`
      : ''
    const foregroundSVG = foregroundValue && !foregroundValue.startsWith('#')
      ? `<image href="${foregroundValue.replaceAll('&', '&amp;').replaceAll('"', '&quot;')}" x="16.67%" y="16.67%" width="66.66%" height="66.66%" preserveAspectRatio="xMidYMid meet"/>`
      : ''
    return svgDataURL(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 108 108">${backgroundSVG}${foregroundSVG}</svg>`)
  }

  const colorValues = new Map<string, string>()
  const type = match[1] === 'mipmap' ? 'mipmap' : 'drawable'
  return toDataURL(type, match[2])
}

function ProjectIcon({ project, className }: { project: Project; className: string }) {
  const [loadedIcon, setLoadedIcon] = useState<{ key: string; url: string } | null>(null)
  const iconKey = `${project.id}:${project.iconResource ?? ''}`
  useEffect(() => {
    let active = true
    if (projectTypeOf(project) === 'java-library') return () => { active = false }
    void (async () => {
      const manifestPath = 'app/src/main/AndroidManifest.xml'
      const manifest = await db.files.where('[projectId+path]').equals([project.id, manifestPath]).first()
      const iconResource = manifestIconResource(manifest?.content) ?? project.iconResource
      if (!iconResource) return
      const url = await projectIconURL({ ...project, iconResource })
      if (active && url) setLoadedIcon({ key: iconKey, url })
    })().catch(error => console.error(`Gagal memuat ikon proyek ${project.name}:`, error))
    return () => { active = false }
  }, [iconKey, project])

  if (projectTypeOf(project) === 'java-library') {
    return <div className={className}>J</div>
  }
  const iconURL = loadedIcon?.key === iconKey ? loadedIcon.url : androidDefaultIcon
  return <div className={className}><img src={iconURL} alt=""/></div>
}

function buildArtifactLabel(variant: BuildVariant): string {
  return variant === 'jar' || variant === 'fat-jar' ? 'JAR' : 'APK'
}

function projectArtifactLabel(project: Project | null): string {
  return project && projectTypeOf(project) === 'java-library' ? 'JAR' : 'APK'
}

function buildStatusLabel(status: Build['status']): string {
  const labels: Record<Build['status'], string> = {
    preparing: 'Menyiapkan',
    assembling: 'Menyusun',
    queued: 'Dalam antrean',
    building: 'Sedang membangun',
    success: 'Berhasil',
    failed: 'Gagal',
    canceled: 'Dibatalkan',
  }
  return labels[status]
}

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
    this.version(2).stores({
      projects: 'id, name, updatedAt',
      files: 'id, projectId, path, updatedAt, [projectId+path]',
    })
  }
}

const db = new IDEDatabase()

const uid = () => crypto.randomUUID()
const packagePath = (packageName: string) => packageName.replace(/\./g, '/')

function emptyTemplate(project: Project): ProjectFile[] {
  const now = Date.now()
  if (projectTypeOf(project) === 'java-library') {
    const sourcePath = `src/main/java/${packagePath(project.packageName)}/Library.java`
    const files: Record<string, string> = {
      'settings.gradle.kts': `pluginManagement {\n    repositories {\n        gradlePluginPortal()\n        mavenCentral()\n    }\n}\ndependencyResolutionManagement {\n    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)\n    repositories { mavenCentral() }\n}\nrootProject.name = "${project.name}"\n`,
      'build.gradle.kts': `plugins {\n    \`java-library\`\n}\n\ngroup = "${project.packageName}"\nversion = "1.0.0"\n\njava {\n    toolchain {\n        languageVersion.set(JavaLanguageVersion.of(17))\n    }\n}\n\ndependencies {\n    // Tambahkan dependency runtime di sini agar ikut ke fat JAR.\n}\n\ntasks.register<Jar>("fatJar") {\n    archiveClassifier.set("all")\n    duplicatesStrategy = DuplicatesStrategy.EXCLUDE\n    from(sourceSets.main.get().output)\n    dependsOn(configurations.runtimeClasspath)\n    from({ configurations.runtimeClasspath.get().filter { it.isFile }.map { zipTree(it) } })\n}\n`,
      'gradle.properties': `org.gradle.jvmargs=-Xmx2048m\n`,
      [sourcePath]: `package ${project.packageName};\n\npublic final class Library {\n    private Library() {}\n\n    public static String hello() {\n        return "Hello, ${project.name}!";\n    }\n}\n`,
    }
    return Object.entries(files).map(([path, content]) => ({
      id: uid(), projectId: project.id, path, type: 'file', content,
      size: new Blob([content]).size, createdAt: now, updatedAt: now,
    }))
  }
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
    'app/src/main/AndroidManifest.xml': `<?xml version="1.0" encoding="utf-8"?>\n<manifest xmlns:android="http://schemas.android.com/apk/res/android">\n    <application\n        android:allowBackup="true"\n        android:icon="@drawable/ic_launcher"\n        android:label="@string/app_name"\n        android:theme="@style/Theme.MyApp">\n        <activity android:name=".MainActivity" android:exported="true">\n            <intent-filter>\n                <action android:name="android.intent.action.MAIN"/>\n                <category android:name="android.intent.category.LAUNCHER"/>\n            </intent-filter>\n        </activity>\n    </application>\n</manifest>\n`,
    'app/src/main/res/drawable/ic_launcher.xml': `<?xml version="1.0" encoding="utf-8"?>\n<vector xmlns:android="http://schemas.android.com/apk/res/android"\n    android:width="48dp"\n    android:height="48dp"\n    android:viewportWidth="48"\n    android:viewportHeight="48">\n    <path\n        android:fillColor="#3DDC84"\n        android:pathData="M14.4,16.4L11.6,12a0.8,0.8 0,0 1,1.4 -0.8l2.9,4.5a15,15 0,0 1,16.2 0l2.9,-4.5a0.8,0.8 0,1 1,1.4 0.8l-2.8,4.4A13,13 0,0 1,39 27L9,27a13,13 0,0 1,5.4 -10.6ZM15,21a1.5,1.5 0,1 0,0 -3a1.5,1.5 0,0 0,0 3ZM33,21a1.5,1.5 0,1 0,0 -3a1.5,1.5 0,0 0,0 3ZM9,29L39,29v5a3,3 0,0 1,-3 3h-1v5a2,2 0,0 1,-4 0v-5L17,37v5a2,2 0,0 1,-4 0v-5h-1a3,3 0,0 1,-3 -3zM4,29a2,2 0,0 1,4 0v7a2,2 0,0 1,-4 0zM40,29a2,2 0,0 1,4 0v7a2,2 0,0 1,-4 0z" />\n</vector>\n`,
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
  if (projectTypeOf(project) === 'java-library') {
    if (!paths.has(`${root}build.gradle`) && !paths.has(`${root}build.gradle.kts`)) {
      return 'build.gradle atau build.gradle.kts tidak ditemukan'
    }
    if (!files.some(file => file.path.startsWith(`${root}src/main/java/`) && file.path.endsWith('.java'))) {
      return 'Tambahkan minimal satu file Java di src/main/java'
    }
    if (files.length > 20000) return 'Proyek memiliki lebih dari 20.000 berkas.'
    return null
  }
  if (!/^([a-zA-Z_]\w*\.)+[a-zA-Z_]\w*$/.test(project.packageName)) return 'Nama paket tidak valid.'
  if (!Number.isInteger(project.minSdk) || !Number.isInteger(project.targetSdk) ||
      project.minSdk < 1 || project.targetSdk < project.minSdk) return 'Rentang Min SDK dan Target SDK tidak valid.'
  if (!paths.has(`${root}app/build.gradle`) && !paths.has(`${root}app/build.gradle.kts`)) {
    return 'Module app/build.gradle atau app/build.gradle.kts tidak ditemukan'
  }
  if (!paths.has(`${root}app/src/main/AndroidManifest.xml`)) {
    return 'app/src/main/AndroidManifest.xml tidak ditemukan'
  }
  if (files.length > 20000) return 'Proyek memiliki lebih dari 20.000 berkas.'
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
  const [editorPanel, setEditorPanel] = useState<'search' | 'diagnostics' | null>(null)
  const [editorSearchScope, setEditorSearchScope] = useState<'file' | 'project'>('project')
  const [editorSearchQuery, setEditorSearchQuery] = useState('')
  const [searchResults, setSearchResults] = useState<SearchResult[]>([])
  const [searchInProgress, setSearchInProgress] = useState(false)
  const [editorNavigation, setEditorNavigation] = useState<{ fileId: string; line: number; column: number } | null>(null)
  const [editorStateProjectId, setEditorStateProjectId] = useState<string | null>(null)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [buildOpen, setBuildOpen] = useState(false)
  const [fullLogOpen, setFullLogOpen] = useState(false)
  const [monitorDashboardOpen, setMonitorDashboardOpen] = useState(false)
  const [hostMetrics, setHostMetrics] = useState<HostResourceMetrics | null>(null)
  const [hostMetricsHistory, setHostMetricsHistory] = useState<MetricHistoryPoint[]>([])
  const [hostMetricsError, setHostMetricsError] = useState('')
  const [buildMetrics, setBuildMetrics] = useState<BuildResourceMetrics | null>(null)
  const [buildMetricsHistory, setBuildMetricsHistory] = useState<MetricHistoryPoint[]>([])
  const [buildOptionsOpen, setBuildOptionsOpen] = useState(false)
  const [importingZip, setImportingZip] = useState(false)
  const [importingGitHub, setImportingGitHub] = useState(false)
  const [activeBuilds, setActiveBuilds] = useState<Record<string, Build>>({})
  const [buildStatus, setBuildStatus] = useState<Build['status'] | 'idle'>('idle')
  const [buildVariant, setBuildVariant] = useState<Build['variant']>('debug')
  const [buildCanceling, setBuildCanceling] = useState(false)
  const [logs, setLogs] = useState<string[]>([])
  const [buildId, setBuildId] = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [appDialog, setAppDialog] = useState<AppDialogRequest | null>(null)
  const [dialogInput, setDialogInput] = useState('')
  const [search, setSearch] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set(defaultExpandedPaths))
  const [toast, setToast] = useState('')
  const [storageOpen, setStorageOpen] = useState(false)
  const [storageLoading, setStorageLoading] = useState(false)
  const [storageStats, setStorageStats] = useState<StorageStats | null>(null)
  const [artifactsOpen, setArtifactsOpen] = useState(false)
  const [artifacts, setArtifacts] = useState<BuildArtifact[]>([])
  const [artifactsLoading, setArtifactsLoading] = useState(false)
  const [artifactDeletingId, setArtifactDeletingId] = useState<string | null>(null)
  const [artifactsError, setArtifactsError] = useState('')
  const [notificationsOpen, setNotificationsOpen] = useState(false)
  const [notificationSettings, setNotificationSettings] = useState<NotificationSettings | null>(null)
  const [notificationLoading, setNotificationLoading] = useState(false)
  const [notificationSaving, setNotificationSaving] = useState(false)
  const [notificationError, setNotificationError] = useState('')
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
  const editorRef = useRef<MonacoEditor.IStandaloneCodeEditor | null>(null)
  const monacoRef = useRef<typeof import('monaco-editor') | null>(null)
  const editorSearchInput = useRef<HTMLInputElement>(null)
  const buildEvents = useRef<EventSource | null>(null)
  const buildAbortController = useRef<AbortController | null>(null)
  const buildPrepareInFlight = useRef(false)
  const buildCancelRequested = useRef(false)
  const pendingBuildId = useRef<string | null>(null)
  const requestedBuildId = useRef<string | null>(null)
  const [buildHistory, setBuildHistory] = useState<Build[]>([])
  const dialogResolver = useRef<((value: string | boolean | null) => void) | null>(null)
  const projectZipInput = useRef<HTMLInputElement>(null)
  const [restoreProjectId] = useState(() => localStorage.getItem('android-web-ide.activeProjectId'))
  const cachedAccessMode = localStorage.getItem(accessModeKey)
  const authMode = authStatus?.mode ?? (cachedAccessMode === 'public' ? 'public' : 'private')
  const onlineAccess = isOnline && serverAvailable
  const authenticated = authStatus?.mode === 'public' || authStatus?.authenticated === true ||
    (!serverAvailable && authMode === 'public') ||
    (!serverAvailable && authMode === 'private' && offlineUnlocked)
  const activeProject = projects.find(p => p.id === activeProjectId) ?? null
  const activeFile = files.find(f => f.id === activeFileId) ?? null
  const [diagnostics, setDiagnostics] = useState<EditorDiagnostic[]>([])
  const activeProjectFiles = useMemo(
    () => files.filter(file => file.projectId === activeProjectId),
    [files, activeProjectId],
  )

  useEffect(() => {
    const viewport = window.visualViewport
    const updateViewport = () => {
      if (!viewport) return
      const visibleBottom = viewport.offsetTop + viewport.height
      const keyboardInset = Math.max(0, window.innerHeight - visibleBottom)
      document.documentElement.style.setProperty('--visible-viewport-height', `${viewport.height}px`)
      document.documentElement.style.setProperty('--keyboard-inset-bottom', `${keyboardInset}px`)
    }
    updateViewport()
    viewport?.addEventListener('resize', updateViewport)
    viewport?.addEventListener('scroll', updateViewport)
    return () => {
      viewport?.removeEventListener('resize', updateViewport)
      viewport?.removeEventListener('scroll', updateViewport)
      document.documentElement.style.removeProperty('--visible-viewport-height')
      document.documentElement.style.removeProperty('--keyboard-inset-bottom')
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    const timer = window.setTimeout(() => {
      void (async () => {
        const found: EditorDiagnostic[] = []
        for (let offset = 0; offset < activeProjectFiles.length; offset += 20) {
          found.push(...activeProjectFiles.slice(offset, offset + 20).flatMap(editorDiagnostics))
          if (offset + 20 < activeProjectFiles.length) await new Promise(resolve => window.setTimeout(resolve, 0))
          if (cancelled) return
        }
        setDiagnostics(found)
      })()
    }, 120)
    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [activeProjectFiles])

  useEffect(() => {
    const editor = editorRef.current
    const model = editor?.getModel()
    const monaco = monacoRef.current
    if (!editor || !model || !monaco) return
    const markers = diagnostics
      .filter(item => item.fileId === activeFileId)
      .map(item => ({
        severity: monaco.MarkerSeverity.Error,
        message: item.message,
        startLineNumber: Math.max(1, item.line),
        startColumn: Math.max(1, item.column),
        endLineNumber: Math.max(1, item.line),
        endColumn: Math.max(2, item.column + 1),
      }))
    monaco.editor.setModelMarkers(model, 'android-web-ide-local-checks', markers)
    return () => monaco.editor.setModelMarkers(model, 'android-web-ide-local-checks', [])
  }, [diagnostics, activeFileId, activeFile?.content])

  useEffect(() => {
    if (!editorNavigation || editorNavigation.fileId !== activeFileId) return
    const editor = editorRef.current
    const model = editor?.getModel()
    if (!editor || !model) return
    const line = Math.min(Math.max(1, editorNavigation.line), model.getLineCount())
    const column = Math.min(Math.max(1, editorNavigation.column), model.getLineMaxColumn(line))
    editor.setPosition({ lineNumber: line, column })
    editor.revealPositionInCenter({ lineNumber: line, column })
    editor.focus()
    setEditorNavigation(null)
  }, [editorNavigation, activeFileId, activeFile?.content])

  useEffect(() => {
    if (editorPanel !== 'search' || !activeProjectId || !editorSearchQuery.trim()) {
      setSearchResults([])
      setSearchInProgress(false)
      return
    }
    let cancelled = false
    setSearchInProgress(true)
    const timer = window.setTimeout(() => {
      void (async () => {
        const candidates = editorSearchScope === 'file'
          ? activeProjectFiles.filter(file => file.id === activeFileId)
          : activeProjectFiles
        const results: SearchResult[] = []
        for (let offset = 0; offset < candidates.length && results.length < 200; offset += 100) {
          results.push(...findProjectMatches(candidates.slice(offset, offset + 100), activeFileId ?? '', 'project', editorSearchQuery, 200 - results.length))
          if (offset + 100 < candidates.length) await new Promise(resolve => window.setTimeout(resolve, 0))
          if (cancelled) return
        }
        setSearchResults(results)
        setSearchInProgress(false)
      })()
    }, 180)
    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [activeProjectId, activeFileId, activeProjectFiles, editorPanel, editorSearchQuery, editorSearchScope])

  useEffect(() => {
    if (editorPanel === 'search') editorSearchInput.current?.focus()
  }, [editorPanel])

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
      setToast(`Gagal menyiapkan PWA: ${error}`)
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
      if (!disposed) setToast(error instanceof Error ? error.message : 'Gagal memuat berkas proyek.')
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
      setToast(error instanceof Error ? error.message : 'Gagal menyimpan status editor.')
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
        if (!response.ok) throw new Error('Gagal memuat riwayat build.')
        return response.json() as Promise<Build[]>
      })
      .then(builds => {
        if (disposed) return
        setBuildHistory(builds)
        const requested = requestedBuildId.current
        const selected = builds.find(build => build.id === requested) ?? builds[0]
        if (selected) {
          requestedBuildId.current = null
          setBuildOpen(true)
          followBuild(selected)
          if (requested) setFullLogOpen(true)
        } else {
          setBuildId(null)
          setBuildStatus('idle')
          setLogs([])
          setBuildOpen(false)
        }
      })
      .catch(error => {
        if (!disposed) setToast(error instanceof Error ? error.message : 'Gagal memuat riwayat build.')
      })
    return () => {
      disposed = true
      buildEvents.current?.close()
      buildEvents.current = null
    }
  }, [activeProjectId, authenticated, onlineAccess])

  useEffect(() => {
    if (!onlineAccess || !authenticated || activeProjectId) {
      setActiveBuilds({})
      return
    }
    let disposed = false
    const refreshActiveBuilds = async () => {
      try {
        const response = await fetch('/api/builds/active')
        if (!response.ok) throw new Error('Gagal memuat status build aktif.')
        const builds = await response.json() as Build[]
        if (!disposed) setActiveBuilds(Object.fromEntries(builds.map(build => [build.projectId, build])))
      } catch (error) {
        if (!disposed) setToast(error instanceof Error ? error.message : 'Gagal memuat status build aktif.')
      }
    }
    void refreshActiveBuilds()
    const timer = window.setInterval(() => void refreshActiveBuilds(), 3000)
    return () => {
      disposed = true
      window.clearInterval(timer)
    }
  }, [activeProjectId, authenticated, onlineAccess])

  useEffect(() => {
    if (!onlineAccess || !monitorDashboardOpen || authMode !== 'private') {
      setHostMetrics(null)
      setHostMetricsHistory([])
      setHostMetricsError('')
      return
    }
    let disposed = false
    const refreshHostMetrics = async () => {
      try {
        const response = await fetch('/api/monitor/host')
        const body = await response.text()
        if (!response.ok) throw new Error(body || `Gagal memuat metrik server (${response.status}).`)
        const metrics = JSON.parse(body) as HostResourceMetrics
        if (disposed) return
        setHostMetrics(metrics)
        setHostMetricsError('')
        setHostMetricsHistory(previous => [...previous, {
          cpu: metrics.cpuPercent, memory: metrics.memoryPercent,
        }].slice(-60))
      } catch (error) {
        if (!disposed) setHostMetricsError(error instanceof Error ? error.message : 'Gagal memuat metrik server.')
      }
    }
    void refreshHostMetrics()
    const timer = window.setInterval(() => void refreshHostMetrics(), 2000)
    return () => {
      disposed = true
      window.clearInterval(timer)
    }
  }, [authMode, monitorDashboardOpen, onlineAccess])

  useEffect(() => {
    if (!onlineAccess || !authenticated || authMode !== 'private' || !activeProjectId || !buildId) {
      setBuildMetrics(null)
      setBuildMetricsHistory([])
      return
    }
    let disposed = false
    const refreshBuildMetrics = async () => {
      try {
        const response = await fetch(`/api/monitor/builds/${encodeURIComponent(buildId)}`)
        const body = await response.text()
        if (!response.ok) throw new Error(body || `Gagal memuat metrik build (${response.status}).`)
        const monitor = JSON.parse(body) as BuildMonitor
        if (disposed) return
        const metrics = monitor.metrics ?? null
        setBuildMetrics(metrics)
        if (metrics) {
          setBuildMetricsHistory(previous => [...previous, {
            cpu: metrics.cpuPercent, memory: metrics.memoryPercent,
          }].slice(-60))
        }
      } catch (error) {
        if (!disposed) setToast(error instanceof Error ? error.message : 'Gagal memuat metrik build.')
      }
    }
    void refreshBuildMetrics()
    const timer = window.setInterval(() => void refreshBuildMetrics(), 2000)
    return () => {
      disposed = true
      window.clearInterval(timer)
    }
  }, [activeProjectId, authMode, authenticated, buildId, onlineAccess])

  useEffect(() => {
    if (!toast) return
    const timer = window.setTimeout(() => setToast(''), 2800)
    return () => window.clearTimeout(timer)
  }, [toast])

  const tree = useMemo(() => buildTree(
    search ? files.filter(f => f.path.toLowerCase().includes(search.toLowerCase())) : files
  ), [files, search])

  function showAppDialog(request: AppDialogRequest): Promise<string | boolean | null> {
    if (dialogResolver.current) return Promise.resolve(null)
    setDialogInput(request.initialValue ?? '')
    setAppDialog(request)
    return new Promise(resolve => {
      dialogResolver.current = resolve
    })
  }

  function closeAppDialog(value: string | boolean | null) {
    const resolve = dialogResolver.current
    dialogResolver.current = null
    setAppDialog(null)
    resolve?.(value)
  }

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
      if (!response.ok) throw new Error(body.error || 'Proses masuk gagal.')
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
        if (!response.ok) throw new Error(await response.text() || 'Gagal keluar dari server.')
      } catch (error) {
        setToast(error instanceof Error ? error.message : 'Gagal keluar dari server.')
      }
    }
  }

  async function refreshStorageStats() {
    if (!onlineAccess) {
      setToast('Pengelolaan penyimpanan build memerlukan koneksi ke server.')
      return
    }
    setStorageLoading(true)
    try {
      const response = await fetch('/api/maintenance/storage')
      if (!response.ok) throw new Error(await response.text())
      setStorageStats(await response.json() as StorageStats)
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'Gagal membaca penggunaan penyimpanan.')
    } finally {
      setStorageLoading(false)
    }
  }

  function openStorage() {
    if (!onlineAccess) {
      setToast('Pengelolaan penyimpanan build memerlukan koneksi ke server.')
      return
    }
    setStorageOpen(true)
  }

  function exitProject() {
    setActiveProjectId(null)
    setDrawerOpen(false)
    setSearch('')
    setBuildOpen(false)
    setFullLogOpen(false)
  }

  async function loadArtifacts() {
    if (!onlineAccess) {
      setArtifactsError('Daftar hasil build memerlukan koneksi ke server.')
      return
    }
    setArtifactsLoading(true)
    setArtifactsError('')
    try {
      const response = await fetch('/api/artifacts')
      const body = await response.text()
      if (!response.ok) throw new Error(body || `Gagal memuat daftar hasil build (${response.status}).`)
      setArtifacts(JSON.parse(body) as BuildArtifact[])
    } catch (error) {
      setArtifactsError(error instanceof Error ? error.message : 'Gagal memuat daftar hasil build.')
    } finally {
      setArtifactsLoading(false)
    }
  }

  function openArtifacts() {
    setArtifactsOpen(true)
    void loadArtifacts()
  }

  async function deleteArtifact(artifact: BuildArtifact) {
    const projectName = projects.find(project => project.id === artifact.projectId)?.name ?? artifact.projectId
    const confirmed = await showAppDialog({
      kind: 'confirm',
      title: 'Hapus hasil build?',
      message: `Hapus ${artifact.filename} dari proyek "${projectName}"? Riwayat build akan tetap disimpan.`,
      confirmLabel: 'Hapus',
      destructive: true,
    })
    if (confirmed !== true) return
    setArtifactDeletingId(artifact.buildId)
    setArtifactsError('')
    try {
      const response = await fetch(`/api/artifacts/${encodeURIComponent(artifact.buildId)}`, { method: 'DELETE' })
      const body = await response.text()
      if (!response.ok) throw new Error(body || `Gagal menghapus hasil build (${response.status}).`)
      setArtifacts(prev => prev.filter(item => item.buildId !== artifact.buildId))
      setToast('Hasil build dihapus. Riwayat build tetap disimpan.')
    } catch (error) {
      setArtifactsError(error instanceof Error ? error.message : 'Gagal menghapus hasil build.')
    } finally {
      setArtifactDeletingId(null)
    }
  }

  async function cleanStorage() {
    if (!onlineAccess) {
      setToast('Pembersihan penyimpanan build memerlukan koneksi ke server.')
      return
    }
    if (!Object.values(cleanupOptions).some(Boolean)) {
      setToast('Pilih data yang akan dibersihkan.')
      return
    }
    const confirmed = await showAppDialog({
      kind: 'confirm',
      title: 'Bersihkan penyimpanan?',
      message: `Lanjutkan pembersihan penyimpanan?${cleanupOptions.history ? '\nRiwayat, log, dan cache berkas biner build akan dihapus.' : ''}${cleanupOptions.artifacts || cleanupOptions.history ? '\nHasil build yang dipilih akan dihapus.' : ''}${cleanupOptions.gradleCache ? '\nCache Gradle akan dihapus dan perlu diunduh ulang saat build berikutnya.' : ''}`,
      confirmLabel: 'Bersihkan',
      destructive: true,
    })
    if (confirmed !== true) return
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
      if (!response.ok) throw new Error(data.error || `Pembersihan gagal (${response.status}).`)
      setStorageStats(data.storage ?? null)
      setToast(`Penyimpanan dibersihkan. ${formatBytes(Object.values(data.removed ?? {}).reduce((sum, size) => sum + size, 0))} berhasil dibebaskan.`)
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
      setToast(error instanceof Error ? error.message : 'Pembersihan gagal.')
    } finally {
      setStorageLoading(false)
    }
  }

  async function createProject(data: Omit<Project, 'id' | 'createdAt' | 'updatedAt' | 'templateId'>) {
    const now = Date.now()
    const project: Project = {
      ...data,
      iconResource: data.projectType === 'java-library' ? undefined : '@drawable/ic_launcher',
      id: uid(), templateId: 'empty-app', createdAt: now, updatedAt: now,
    }
    const generated = emptyTemplate(project)
    await db.projects.add(project)
    await db.files.bulkAdd(generated)
    setProjects(prev => [project, ...prev])
    setActiveProjectId(project.id)
    setShowCreate(false)
    setToast('Proyek berhasil dibuat.')
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
      setToast(error instanceof Error ? error.message : 'Gagal memperbarui hash berkas.')
    })
    const existingTimer = autoSaveTimers.current.get(activeFile.id)
    if (existingTimer) window.clearTimeout(existingTimer)
    const timer = window.setTimeout(() => saveFile(activeFile, content), 800)
    autoSaveTimers.current.set(activeFile.id, timer)
  }

  function insertEditorText(value: string, closing?: string) {
    const editor = editorRef.current
    const model = editor?.getModel()
    const selection = editor?.getSelection()
    if (!editor || !model || !selection) return
    const selectedText = model.getValueInRange(selection)
    const startOffset = model.getOffsetAt(selection.getStartPosition())
    const text = closing ? `${value}${selectedText}${closing}` : value
    editor.executeEdits('editor-helper-toolbar', [{
      range: selection,
      text,
      forceMoveMarkers: true,
    }])
    if (closing) {
      const start = model.getPositionAt(startOffset + value.length)
      const end = model.getPositionAt(startOffset + value.length + selectedText.length)
      if (selectedText) editor.setSelection({
        startLineNumber: start.lineNumber, startColumn: start.column,
        endLineNumber: end.lineNumber, endColumn: end.column,
      })
      else editor.setPosition(start)
    }
    editor.focus()
  }

  async function openFile(file: ProjectFile) {
    if (!openTabs.some(f => f.id === file.id)) setOpenTabs(prev => [...prev, file])
    setActiveFileId(file.id)
    setDrawerOpen(false)
  }

  async function deleteProject(project: Project) {
    const confirmed = await showAppDialog({
      kind: 'confirm',
      title: 'Hapus proyek?',
      message: `Hapus proyek "${project.name}" beserta seluruh berkasnya?`,
      confirmLabel: 'Hapus',
      destructive: true,
    })
    if (confirmed !== true) return
    await db.files.where('projectId').equals(project.id).delete()
    await db.projects.delete(project.id)
    localStorage.removeItem(editorStateKey(project.id))
    setProjects(prev => prev.filter(p => p.id !== project.id))
    if (activeProjectId === project.id) setActiveProjectId(null)
  }

  async function exportProject(project = activeProject) {
    if (!project) return
    try {
      const projectFiles = project.id === activeProjectId
        ? files
        : await db.files.where('projectId').equals(project.id).toArray()
      const zip = new JSZip()
      for (const file of projectFiles) {
        if (file.binary) zip.file(file.path, file.binary)
        else zip.file(file.path, file.content ?? '')
      }
      const blob = await zip.generateAsync({ type: 'blob' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `${project.name.replace(/\s+/g, '-')}.zip`
      a.click()
      window.setTimeout(() => URL.revokeObjectURL(url), 1000)
    } catch (error) {
      setToast(error instanceof Error ? `Gagal mengekspor proyek: ${error.message}` : 'Gagal mengekspor proyek.')
    }
  }

  async function importZip(file: File) {
    if (importingZip) return
    setImportingZip(true)
    try {
      if (file.size > 250 * 1024 * 1024) throw new Error('Ukuran arsip ZIP melebihi 250 MB.')
      const zip = await JSZip.loadAsync(file)
      const projectId = uid()
      const now = Date.now()
      const projectName = file.name.replace(/\.zip$/i, '') || 'Aplikasi Impor'
      const imported: ProjectFile[] = []
      let totalBytes = 0
      for (const [path, entry] of Object.entries(zip.files)) {
        const clean = path.replace(/^(\.\/)+/, '')
        if (!clean || entry.dir) continue
        if (clean.startsWith('/') || clean.includes('\\') ||
            clean.split('/').some(part => !part || part === '.' || part === '..')) {
          throw new Error(`Lokasi berkas dalam ZIP tidak valid: ${clean}`)
        }
        const segments = clean.split('/')
        if (segments.some(part => ['.git', '.idea', '.gradle'].includes(part)) ||
            segments.some((part, index) => part === 'build' && (index === 0 || segments[index - 1] === 'app')) ||
            /(^|\/)(local\.properties|.*\.iml)$/.test(clean)) continue
        const lower = clean.toLowerCase()
        const text = /\.(kt|java|xml|gradle|kts|properties|json|txt|md|pro|toml|ya?ml|sh|aidl|cfg|conf|html|css|js)$/i.test(lower)
        const content = text ? await entry.async('string') : undefined
        const binary = text ? undefined : await entry.async('blob')
        const size = binary?.size ?? new Blob([content ?? '']).size
        if (size > 50 * 1024 * 1024) throw new Error(`Berkas dalam ZIP melebihi 50 MB: ${clean}`)
        totalBytes += size
        if (totalBytes > 250 * 1024 * 1024) throw new Error('Ukuran proyek setelah diekstrak melebihi 250 MB.')
        imported.push({ id: uid(), projectId, path: clean, type: 'file', content, binary, size, createdAt: now, updatedAt: now })
        if (imported.length > 20000) throw new Error('Proyek memiliki lebih dari 20.000 berkas.')
      }

      const settingsFiles = imported.filter(file => /(^|\/)settings\.gradle(\.kts)?$/.test(file.path))
      const roots = [...new Set(settingsFiles.map(file => {
        const slash = file.path.lastIndexOf('/')
        return slash < 0 ? '' : file.path.slice(0, slash + 1)
      }))]
      const projectRoots = roots.filter(root =>
        (imported.some(file => file.path === `${root}app/build.gradle` || file.path === `${root}app/build.gradle.kts`)) &&
        imported.some(file => file.path === `${root}app/src/main/AndroidManifest.xml`))
      if (projectRoots.length !== 1) {
        throw new Error(projectRoots.length > 1
          ? 'Arsip ZIP berisi lebih dari satu proyek Android. Impor hanya mendukung proyek dengan satu modul.'
          : 'ZIP harus berisi settings.gradle, app/build.gradle, dan AndroidManifest.xml')
      }
      const root = projectRoots[0]
      const projectFiles = imported
        .filter(file => file.path.startsWith(root))
        .map(file => ({ ...file, path: file.path.slice(root.length) }))
      const manifest = projectFiles.find(file => file.path === 'app/src/main/AndroidManifest.xml')
      const manifestMatch = manifest?.content?.match(/<manifest[^>]*package=["']([^"']+)/)
      const project: Project = {
        id: projectId, name: projectName,
        iconResource: manifestIconResource(manifest?.content),
        packageName: manifestMatch?.[1] || 'com.example.imported',
        language: projectFiles.some(file => file.path.endsWith('.kt')) ? 'kotlin' : 'java',
        templateId: 'imported', minSdk: 24, targetSdk: 34, createdAt: now, updatedAt: now,
      }
      const invalidReason = validateBuildProject(project, projectFiles)
      if (invalidReason) throw new Error(invalidReason)
      await db.transaction('rw', db.projects, db.files, async () => {
        await db.projects.add(project)
        await db.files.bulkAdd(projectFiles)
      })
      setProjects(prev => [project, ...prev])
      setActiveProjectId(projectId)
      setToast('Arsip ZIP proyek berhasil diimpor.')
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'Gagal mengimpor ZIP')
    } finally {
      setImportingZip(false)
    }
  }

  async function importGitHubProject() {
    if (importingZip || importingGitHub) return
    const value = await showAppDialog({
      kind: 'prompt',
      title: 'Impor dari GitHub',
      message: 'Masukkan URL repositori GitHub publik yang berisi proyek Android.',
      placeholder: 'https://github.com/pemilik/repositori',
      confirmLabel: 'Impor',
    })
    if (typeof value !== 'string' || !value.trim()) return
    const repositoryURL = value.trim()
    setImportingGitHub(true)
    try {
      const response = await fetch('/api/import/github', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: repositoryURL.trim() }),
      })
      if (!response.ok) throw new Error(await response.text() || 'Gagal mengunduh repositori dari GitHub.')
      const archive = await response.blob()
      const repositoryName = repositoryURL.trim().replace(/\/+$/, '').replace(/\.git$/i, '').split('/').pop() || 'github-project'
      await importZip(new File([archive], `${repositoryName}.zip`, { type: 'application/zip' }))
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'Gagal mengimpor repositori GitHub.')
    } finally {
      setImportingGitHub(false)
    }
  }

  async function createFile() {
    if (!activeProject) return
    const value = await showAppDialog({
      kind: 'prompt',
      title: 'Buat file baru',
      message: 'Masukkan path file relatif dari root project.',
      placeholder: 'app/src/main/java/Main.kt',
      confirmLabel: 'Buat file',
    })
    if (typeof value !== 'string' || !value) return
    const path = value
    const clean = path.replace(/^\/+/, '')
    if (clean.includes('..') || clean.includes('\\')) { setToast('Lokasi berkas tidak valid.'); return }
    if (files.some(f => f.path === clean)) { setToast('Berkas sudah ada.'); return }
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
    const confirmed = await showAppDialog({
      kind: 'confirm',
      title: 'Hapus file?',
      message: `Hapus "${file.path}" dari project?`,
      confirmLabel: 'Hapus',
      destructive: true,
    })
    if (confirmed !== true) return
    await db.files.delete(file.id)
    setFiles(prev => prev.filter(f => f.id !== file.id))
    setOpenTabs(prev => prev.filter(f => f.id !== file.id))
    if (activeFileId === file.id) setActiveFileId(null)
  }

  function followBuild(build: Build) {
    buildEvents.current?.close()
    setBuildId(build.id)
    setBuildVariant(build.variant)
    setBuildStatus(build.status)
    setLogs([])
    setBuildMetrics(null)
    setBuildMetricsHistory([])
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

  function openBuildFromDashboard(build: Build) {
    requestedBuildId.current = build.id
    setMonitorDashboardOpen(false)
    setActiveProjectId(build.projectId)
  }

  async function copyLogs() {
    try {
      await navigator.clipboard.writeText(logs.join('\n'))
      setToast('Log berhasil disalin.')
    } catch (error) {
      setToast(error instanceof Error ? `Gagal menyalin log: ${error.message}` : 'Gagal menyalin log.')
    }
  }

  async function copyValue(value: string, label: string) {
    try {
      await navigator.clipboard.writeText(value)
      setToast(`${label} berhasil disalin.`)
    } catch (error) {
      setToast(error instanceof Error ? `Gagal menyalin ${label.toLowerCase()}: ${error.message}` : `Gagal menyalin ${label.toLowerCase()}.`)
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
      setToast('Pembatalan build memerlukan koneksi ke server.')
      return
    }
    setBuildCanceling(true)
    let waitForBuildEnd = false
    try {
      const response = await fetch(`/api/builds/${encodeURIComponent(currentBuildId)}`, { method: 'DELETE' })
      if (!response.ok) {
        const message = await response.text()
        throw new Error(message || `Gagal membatalkan build (${response.status}).`)
      }
      if (controller) {
        controller.abort()
        setBuildStatus('canceled')
        setLogs(prev => [...prev, 'Build dibatalkan.'])
        setToast('Build dibatalkan.')
      } else {
        waitForBuildEnd = buildEvents.current !== null
        if (!waitForBuildEnd) setBuildStatus('canceled')
        setLogs(prev => [...prev, 'Permintaan pembatalan build dikirim, menunggu worker berhenti...'])
        setToast('Permintaan pembatalan build dikirim.')
      }
    } catch (error) {
      setToast(error instanceof Error ? error.message : 'Gagal membatalkan build.')
    } finally {
      if (!waitForBuildEnd) setBuildCanceling(false)
    }
  }

  function openBuildOptions() {
    if (!onlineAccess) {
      setToast(`Build ${projectArtifactLabel(activeProject)} memerlukan koneksi ke server.`)
      return
    }
    if (!activeProject) return
    if (buildStatus === 'preparing' || buildStatus === 'assembling' ||
        buildStatus === 'queued' || buildStatus === 'building') {
      setBuildOpen(true)
      return
    }
    setBuildOptionsOpen(true)
  }

  async function startBuild(options: BuildOptions) {
    setBuildOptionsOpen(false)
    if (!onlineAccess || !activeProject) return
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
    setBuildVariant(options.variant)
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
      const maxProjectBytes = authMode === 'public' ? 10 * 1024 * 1024 : 250 * 1024 * 1024
      if (totalBytes > maxProjectBytes) {
        throw new Error(authMode === 'public'
          ? 'Ukuran project mode publik melebihi 10 MiB'
          : 'Ukuran project melebihi 250 MB')
      }
      buildPrepareInFlight.current = true
      const response = await fetch('/api/builds/prepare', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        signal: controller.signal,
        body: JSON.stringify({
          projectId: activeProject.id,
          idempotencyKey: crypto.randomUUID(),
          projectType: projectTypeOf(activeProject),
          variant: options.variant,
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
        setToast('Proyek ini masih memiliki build aktif.')
        followBuild(data.build)
        return
      }
      if (!response.ok) throw new Error(data.error || body || `Gagal menyiapkan build (${response.status})`)
      if (!data.build?.id) throw new Error('Server tidak mengembalikan reservasi build.')
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
          throw new Error(message || `Gagal membatalkan build (${cancelResponse.status}).`)
        }
        controller.abort()
        buildCancelRequested.current = false
        setBuildStatus('canceled')
        setLogs(prev => [...prev, 'Build dibatalkan.'])
        setToast('Build dibatalkan.')
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
        if (!binary) throw new Error(`Berkas biner ${hash} tidak ditemukan di IndexedDB.`)
        setLogs(prev => [...prev, `Mengupload binary ${index + 1}/${data.missingBlobs.length}...`])
        const uploadResponse = await fetch(`/api/builds/${buildReservation.id}/blobs/${hash}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/octet-stream' },
          signal: controller.signal,
          body: binary,
        })
        if (!uploadResponse.ok) throw new Error(await uploadResponse.text() || `Pengunggahan berkas biner gagal (${uploadResponse.status}).`)
      }
      setBuildStatus('assembling')
      setLogs(prev => [...prev, 'Binary lengkap, merakit workspace build...'])
      finalizeStarted = true
      const finalizeRequest: RequestInit = {
        method: 'POST',
        signal: controller.signal,
      }
      if (options.keystore) {
        const formData = new FormData()
        formData.append('keystore', options.keystore)
        formData.append('keyAlias', options.keyAlias ?? '')
        formData.append('storePassword', options.storePassword ?? '')
        formData.append('keyPassword', options.keyPassword ?? '')
        finalizeRequest.body = formData
      }
      const finalizeResponse = await fetch(`/api/builds/${buildReservation.id}/finalize`, finalizeRequest)
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
      setToast('Buka menu peramban, lalu pilih "Pasang aplikasi" atau "Tambahkan ke layar utama".')
      return
    }
    try {
      await installPrompt.prompt()
      const choice = await installPrompt.userChoice
      if (choice.outcome === 'accepted') setToast('Droid Studio berhasil dipasang.')
      setInstallPrompt(null)
    } catch (error) {
      setToast(error instanceof Error ? `Gagal memasang aplikasi: ${error.message}` : 'Gagal memasang aplikasi.')
    }
  }

  async function openNotifications() {
    setNotificationsOpen(true)
    setNotificationLoading(true)
    setNotificationError('')
    try {
      const settingsResponse = await fetch('/api/notifications/settings')
      const settings = await settingsResponse.json() as NotificationSettings & { error?: string }
      if (!settingsResponse.ok) throw new Error(settings.error || 'Gagal membaca pengaturan email.')
      setNotificationSettings(settings)
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'gagal membaca pengaturan notifikasi')
    } finally {
      setNotificationLoading(false)
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
      if (!response.ok) throw new Error(data.error || 'Gagal menyimpan pengaturan email.')
      setNotificationSettings(data)
      setToast('Pengaturan email tersimpan.')
    } catch (error) {
      setNotificationError(error instanceof Error ? error.message : 'gagal menyimpan pengaturan email')
    } finally {
      setNotificationSaving(false)
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
        {!activeProject && monitorDashboardOpen && <button className="exit-project-btn" title="Kembali ke ruang kerja" aria-label="Kembali ke ruang kerja" onClick={() => setMonitorDashboardOpen(false)}><ArrowLeft size={17}/><span>Ruang kerja</span></button>}
        {!onlineAccess && <div className="offline-indicator"><WifiOff size={14}/><span>Offline · edit lokal</span></div>}
        {!isInstalled && <button className="install-btn" title="Pasang Droid Studio" onClick={() => void installApp()}><Smartphone size={16}/><span>Pasang</span></button>}
        {activeProject && buildId && <button className="icon-btn" title={buildOpen ? 'Sembunyikan panel build' : 'Tampilkan panel build'} aria-label={buildOpen ? 'Sembunyikan panel build' : 'Tampilkan panel build'} onClick={() => setBuildOpen(open => !open)}><Activity size={18}/></button>}
        <button className="build-btn" onClick={openBuildOptions} disabled={!onlineAccess || !activeProject || buildCanceling || buildStatus === 'preparing' || buildStatus === 'assembling' || buildStatus === 'queued' || buildStatus === 'building'} title={!onlineAccess ? `Build ${projectArtifactLabel(activeProject)} memerlukan koneksi ke server` : undefined}>
        <Play size={16} fill="currentColor"/><span>{!onlineAccess ? 'Offline' : buildStatus === 'preparing' || buildStatus === 'assembling' || buildStatus === 'queued' || buildStatus === 'building' ? 'Sedang membangun…' : `Build ${projectArtifactLabel(activeProject)}`}</span>
        </button>
        <button className="icon-btn" title={onlineAccess ? 'Arsip hasil build' : 'Memerlukan koneksi ke server'} aria-label="Arsip hasil build" onClick={openArtifacts} disabled={!onlineAccess}><Archive size={18}/></button>
        {authMode === 'private' && <button className="icon-btn" title={onlineAccess ? 'Notifikasi email build' : 'Memerlukan koneksi ke server'} aria-label="Notifikasi email build" onClick={() => void openNotifications()} disabled={!onlineAccess}><Mail size={18}/></button>}
        <button className="icon-btn storage-btn" title={onlineAccess ? 'Kelola penyimpanan build' : 'Memerlukan koneksi ke server'} aria-label="Kelola penyimpanan build" onClick={openStorage} disabled={!onlineAccess}><HardDrive size={18}/></button>
        {authMode === 'private' && <button className="icon-btn" title="Keluar" aria-label="Keluar" onClick={logout}><LogOut size={18}/></button>}
      </header>

      <aside className={`sidebar ${drawerOpen ? 'open' : ''}`}>
        <div className="sidebar-head">
          <div><small>{activeProject ? 'PENJELAJAH FILE' : 'RUANG KERJA'}</small><strong>{activeProject?.name || 'Proyek'}</strong></div>
          <div className="sidebar-head-actions">
            {activeProject && <button className="icon-btn close-project-btn" title="Tutup proyek" aria-label="Tutup proyek" onClick={exitProject}><ArrowLeft size={17}/></button>}
            <button className="icon-btn" title="Tutup laci" aria-label="Tutup laci" onClick={() => setDrawerOpen(false)}><X size={17}/></button>
          </div>
        </div>
        {activeProject ? (
          <>
            <div className="sidebar-actions">
              <label className="tool-btn" title="Impor ZIP"><Upload size={15}/><input type="file" accept=".zip" disabled={importingZip} onChange={event => {
                const input = event.currentTarget
                const file = input.files?.[0]
                input.value = ''
                if (file) void importZip(file)
              }}/></label>
              <button className="tool-btn" title="Buat file" aria-label="Buat file" onClick={createFile}><FilePlus2 size={15}/></button>
              <button className="tool-btn" title="Ekspor ZIP" aria-label="Ekspor ZIP" onClick={() => void exportProject()}><Download size={15}/></button>
              <button className="tool-btn" title="Buat folder" aria-label="Buat folder"><FolderPlus size={15}/></button>
            </div>
            <div className="search-box"><Search size={14}/><input placeholder="Cari file" value={search} onChange={e => setSearch(e.target.value)}/></div>
            <div className="tree">{renderTree(tree)}</div>
          </>
        ) : (
          <div className="project-list">
            {projects.map(project => (
              <article className="project-card" key={project.id}>
                <button className="project-open" onClick={() => { setMonitorDashboardOpen(false); setActiveProjectId(project.id) }}>
                  <ProjectIcon project={project} className="app-icon"/>
                  <div><strong>{project.name}</strong><span>{project.packageName}</span>
                    {activeBuilds[project.id] && <AnimatedBuildStatus variant={activeBuilds[project.id].variant}/>}
                  </div>
                </button>
                <div className="project-card-actions">
                  <button className="icon-btn" title="Ekspor ZIP proyek" aria-label={`Ekspor ${project.name} sebagai ZIP`}
                    onClick={() => void exportProject(project)}><Download size={15}/></button>
                  <button className="icon-btn" title="Hapus proyek" aria-label={`Hapus ${project.name}`}
                    onClick={() => void deleteProject(project)}><Trash2 size={15}/></button>
                </div>
              </article>
            ))}
            {projects.length === 0 && <div className="empty-state"><Box size={38}/><strong>Belum ada proyek</strong><span>Buat proyek Android pertama.</span></div>}
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
              <>
                <div className="editor-command-row">
                  <button className={`icon-btn ${editorPanel === 'search' ? 'active' : ''}`} title="Cari di proyek" aria-label="Cari di proyek"
                    onClick={() => setEditorPanel(panel => panel === 'search' ? null : 'search')}><Search size={16}/></button>
                  <button className={`icon-btn ${editorPanel === 'diagnostics' ? 'active' : ''}`} title="Lihat pemeriksaan kode" aria-label={`Lihat pemeriksaan kode, ${diagnostics.length} masalah`}
                    onClick={() => setEditorPanel(panel => panel === 'diagnostics' ? null : 'diagnostics')}>
                    <AlertCircle size={16}/>{diagnostics.length > 0 && <span className="diagnostic-count">{diagnostics.length}</span>}
                  </button>
                  <span className="editor-file-path">{activeFile.path}</span>
                  {editorPanel && <button className="icon-btn editor-panel-close" title="Tutup panel" aria-label="Tutup panel"
                    onClick={() => setEditorPanel(null)}><X size={15}/></button>}
                </div>
                {editorPanel === 'search' && (
                  <section className="editor-panel">
                    <div className="editor-search-controls">
                      <input ref={editorSearchInput} value={editorSearchQuery} aria-label="Kata yang dicari"
                        placeholder="Cari teks di proyek ini" onChange={event => setEditorSearchQuery(event.target.value)}/>
                      <div className="editor-search-scope">
                        <button className={editorSearchScope === 'file' ? 'selected' : ''}
                          onClick={() => setEditorSearchScope('file')}>File aktif</button>
                        <button className={editorSearchScope === 'project' ? 'selected' : ''}
                          onClick={() => setEditorSearchScope('project')}>Proyek</button>
                      </div>
                    </div>
                    <div className="editor-result-list">
                      {searchInProgress ? <span className="editor-panel-hint">Mencari…</span> : searchResults.length > 0
                        ? searchResults.map((result, index) => (
                          <button className="editor-result" key={`${result.fileId}:${result.line}:${result.column}:${index}`}
                            onClick={() => {
                              setEditorNavigation({ fileId: result.fileId, line: result.line, column: result.column })
                              setEditorPanel(null)
                              const file = activeProjectFiles.find(candidate => candidate.id === result.fileId)
                              if (file) void openFile(file)
                            }}>
                            <span className="editor-result-location">{result.path}:{result.line}</span>
                            <code>{result.preview}</code>
                          </button>
                        ))
                        : <span className="editor-panel-hint">{editorSearchQuery.trim() ? 'Tidak ada hasil di proyek ini.' : 'Ketik kata atau teks yang mau dicari.'}</span>}
                      {searchResults.length === 200 && <span className="editor-panel-hint">Menampilkan maksimal 200 hasil.</span>}
                    </div>
                  </section>
                )}
                {editorPanel === 'diagnostics' && (
                  <section className="editor-panel">
                    <div className="editor-diagnostic-list">
                      {diagnostics.length === 0 ? <span className="editor-panel-hint">Tidak ditemukan kurung, string, JSON, atau XML yang belum ditutup.</span>
                        : diagnostics.map((item, index) => (
                          <button className="editor-result editor-diagnostic" key={`${item.fileId}:${item.line}:${item.column}:${index}`}
                            onClick={() => {
                              setEditorNavigation({ fileId: item.fileId, line: item.line, column: item.column })
                              setEditorPanel(null)
                              const file = activeProjectFiles.find(candidate => candidate.id === item.fileId)
                              if (file) void openFile(file)
                            }}>
                            <span className="editor-result-location">{item.path}:{item.line}:{item.column}</span>
                            <code>{item.message}</code>
                          </button>
                        ))}
                    </div>
                    <span className="editor-panel-hint">Pemeriksaan lokal dasar; bukan pengganti compiler atau language server.</span>
                  </section>
                )}
                <div className="editor-canvas">
                  <Editor
                    height="100%"
                    language={activeFile.path.endsWith('.kt') ? 'kotlin' : activeFile.path.endsWith('.java') ? 'java' : activeFile.path.endsWith('.xml') ? 'xml' : activeFile.path.endsWith('.json') ? 'json' : activeFile.path.endsWith('.gradle') || activeFile.path.endsWith('.kts') ? 'gradle' : 'plaintext'}
                    value={activeFile.content ?? ''}
                    theme="vs-dark"
                    onChange={editFile}
                    onMount={(editor, monaco) => {
                      editorRef.current = editor
                      monacoRef.current = monaco
                      editor.onDidDispose(() => {
                        if (editorRef.current === editor) {
                          editorRef.current = null
                          monacoRef.current = null
                        }
                      })
                    }}
                    options={{ fontSize: 14, lineNumbers: 'on', minimap: { enabled: false }, wordWrap: 'on', automaticLayout: true, padding: { top: 12 } }}
                  />
                </div>
                <div className="editor-helper-bar" aria-label="Tombol karakter editor">
                  {[
                    { label: '{}', value: '{', closing: '}' },
                    { label: '()', value: '(', closing: ')' },
                    { label: '[]', value: '[', closing: ']' },
                    { label: '""', value: '"', closing: '"' },
                    { label: "''", value: "'", closing: "'" },
                    { label: ';', value: ';' },
                    { label: ',', value: ',' },
                    { label: '=', value: '=' },
                  ].map(button => (
                    <button key={button.label} onClick={() => insertEditorText(button.value, button.closing)}
                      aria-label={`Masukkan ${button.label}`}>{button.label}</button>
                  ))}
                </div>
              </>
            ) : <div className="editor-empty"><FileCode2 size={44}/><strong>Editor siap</strong><span>Buka file untuk mulai mengedit.</span></div>}
          </>
        ) : (
          <div className="home">
            <div className="home-head"><div><small>{monitorDashboardOpen ? 'PEMANTAUAN SERVER' : 'PROYEK ANDROID'}</small><h1>{monitorDashboardOpen ? 'Monitor server' : 'Ruang kerja'}</h1></div>
              {!monitorDashboardOpen && <div className="home-actions">
                {authMode === 'private' && <button className="tool-btn artifact-dashboard-btn" onClick={() => setMonitorDashboardOpen(true)} disabled={!onlineAccess}><Gauge size={16}/>Monitor server</button>}
                <button className="tool-btn artifact-dashboard-btn" onClick={openArtifacts} disabled={!onlineAccess}><Archive size={16}/>Arsip hasil build</button>
                <button className="tool-btn artifact-dashboard-btn" onClick={() => projectZipInput.current?.click()} disabled={importingZip || importingGitHub}>
                  <Upload size={16}/>{importingZip ? 'Memproses ZIP…' : 'Impor proyek'}
                </button>
                <button className="tool-btn artifact-dashboard-btn" onClick={() => void importGitHubProject()}
                  disabled={!onlineAccess || importingZip || importingGitHub}>
                  <Download size={16}/>{importingGitHub ? 'Mengunduh repositori…' : 'Impor dari GitHub'}
                </button>
                <input ref={projectZipInput} type="file" accept=".zip" style={{ display: 'none' }} onChange={event => {
                  const input = event.currentTarget
                  const file = input.files?.[0]
                  input.value = ''
                  if (file) void importZip(file)
                }}/>
                <button className="primary-btn" onClick={() => setShowCreate(true)}><PackagePlus size={17}/>Proyek baru</button>
              </div>}
            </div>
            {monitorDashboardOpen && authMode === 'private' ? (
              <MonitorDashboard
                metrics={hostMetrics}
                history={hostMetricsHistory}
                error={hostMetricsError}
                builds={Object.values(activeBuilds)}
                projects={projects}
                onOpenBuild={openBuildFromDashboard}
                onCopyURL={copyValue}
              />
            ) : (
              <div className="project-grid">
              {projects.map(project => (
                <article className="project-card large" key={project.id}>
                  <button className="project-open" onClick={() => setActiveProjectId(project.id)}>
                    <ProjectIcon project={project} className="app-icon large-icon"/>
                    <div><strong>{project.name}</strong><span>{projectTypeOf(project) === 'java-library' ? 'PUSTAKA JAVA' : `${project.language.toUpperCase()} • SDK ${project.minSdk}+`}</span><small>{project.packageName}</small>
                      {activeBuilds[project.id] && <AnimatedBuildStatus variant={activeBuilds[project.id].variant}/>}
                    </div>
                  </button>
                  <div className="project-card-actions">
                    <button className="icon-btn" title="Ekspor ZIP proyek" aria-label={`Ekspor ${project.name} sebagai ZIP`}
                      onClick={() => void exportProject(project)}><Download size={16}/></button>
                    <button className="icon-btn" title="Hapus proyek" aria-label={`Hapus ${project.name}`}
                      onClick={() => void deleteProject(project)}><Trash2 size={16}/></button>
                  </div>
                </article>
              ))}
              {projects.length === 0 && <button className="create-card" onClick={() => setShowCreate(true)}><PackagePlus size={30}/><strong>Buat proyek baru</strong><span>Aplikasi Android atau pustaka Java</span></button>}
              </div>
            )}
          </div>
        )}
      </main>

      {appDialog && (
        <div className="modal-backdrop app-dialog-backdrop" onMouseDown={event => {
          if (event.target === event.currentTarget) closeAppDialog(null)
        }}>
          <section
            className="modal app-dialog"
            role={appDialog.kind === 'confirm' ? 'alertdialog' : 'dialog'}
            aria-modal="true"
            aria-labelledby="app-dialog-title"
            onKeyDown={event => {
              if (event.key === 'Escape') {
                event.preventDefault()
                closeAppDialog(null)
              }
            }}
          >
            <div className="modal-head">
              <div><small>{appDialog.kind === 'confirm' ? 'KONFIRMASI' : appDialog.kind === 'prompt' ? 'INPUT' : 'INFO'}</small>
                <h2 id="app-dialog-title">{appDialog.title}</h2>
              </div>
              <button className="icon-btn" type="button" aria-label="Tutup" onClick={() => closeAppDialog(null)}><X size={18}/></button>
            </div>
            <form onSubmit={event => {
              event.preventDefault()
              closeAppDialog(appDialog.kind === 'prompt' ? dialogInput : true)
            }}>
              <p>{appDialog.message}</p>
              {appDialog.kind === 'prompt' && (
                <input
                  className="app-dialog-input"
                  autoFocus
                  value={dialogInput}
                  placeholder={appDialog.placeholder}
                  onChange={event => setDialogInput(event.target.value)}
                />
              )}
              <div className="app-dialog-actions">
                {appDialog.kind !== 'alert' && <button className="tool-btn" type="button" onClick={() => closeAppDialog(null)}>
                  {appDialog.cancelLabel ?? 'Batal'}
                </button>}
                <button className={`primary-btn app-dialog-confirm${appDialog.destructive ? ' destructive' : ''}`}
                  type="submit" autoFocus={appDialog.kind !== 'prompt'}>
                  {appDialog.confirmLabel ?? (appDialog.kind === 'confirm' ? 'Lanjutkan' : 'Oke')}
                </button>
              </div>
            </form>
          </section>
        </div>
      )}
      {showCreate && <CreateProjectModal onClose={() => setShowCreate(false)} onCreate={createProject}/>}
      {buildOptionsOpen && activeProject && <BuildOptionsDialog
        projectType={projectTypeOf(activeProject)}
        onClose={() => setBuildOptionsOpen(false)}
        onStart={options => void startBuild(options)}
      />}
      {buildOpen && (
        <div className="build-sheet">
          <div className="build-head">
            <div><small>STATUS BUILD</small><strong>{buildStatus === 'idle' ? `${buildVariant} ${buildArtifactLabel(buildVariant)}` : `${buildVariant} · ${buildStatusLabel(buildStatus)}`}</strong></div>
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
          }}>{buildHistory.map(build => <option key={build.id} value={build.id}>{build.variant} · {buildStatusLabel(build.status)} · {new Date(build.createdAt).toLocaleString()}</option>)}</select></label></div>}
          <div className={`status-line ${buildStatus}`}><span></span>{buildStatus === 'preparing' ? 'Menyiapkan manifest dan berkas biner…' : buildStatus === 'assembling' ? 'Menyusun ruang kerja…' : buildStatus === 'building' ? 'Sedang membangun…' : buildStatus === 'success' ? 'Build berhasil' : buildStatus === 'failed' ? 'Build gagal' : buildStatus === 'canceled' ? 'Build dibatalkan' : 'Menunggu proses build'}</div>
          {authMode === 'private' && <BuildResourcePanel metrics={buildMetrics} history={buildMetricsHistory}/>}
          <div className="log-view">{logs.map((line, i) => <div key={i}><span>{String(i + 1).padStart(3, '0')}</span>{line}</div>)}</div>
          <button className="tool-btn full-log-btn" onClick={() => setFullLogOpen(true)}><FileText size={16}/><span>Log lengkap</span></button>
          {buildStatus === 'success' && buildId && buildHistory.find(build => build.id === buildId)?.artifact &&
            <a className="download-btn" href={`/api/builds/${buildId}/artifact`} download><Download size={17}/>Unduh {buildArtifactLabel(buildHistory.find(build => build.id === buildId)?.variant ?? buildVariant)}</a>}
        </div>
      )}
      {fullLogOpen && buildId && (
        <BuildLogScreen
          buildId={buildId}
          authMode={authMode}
          variant={buildVariant}
          status={buildStatus}
          logs={logs}
          metrics={authMode === 'private' ? buildMetrics : null}
          history={buildMetricsHistory}
          onBack={() => { setFullLogOpen(false); setBuildOpen(true) }}
          onCopy={copyLogs}
          onCopyURL={copyValue}
        />
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
          error={notificationError}
          onSettingsChange={setNotificationSettings}
          onSave={saveNotificationSettings}
          onClose={() => setNotificationsOpen(false)}
        />
      )}
      {toast && <div className="toast">{toast}</div>}
    </div>
  )
}

function AnimatedBuildStatus({ variant }: { variant: Build['variant'] }) {
  const [dots, setDots] = useState(0)

  useEffect(() => {
    const timer = window.setInterval(() => setDots(current => (current + 1) % 4), 450)
    return () => window.clearInterval(timer)
  }, [])

  return <small className="project-build-status">Membangun<span>{'.'.repeat(dots)}</span> • {variant}</small>
}

function MetricSparkline({ values, label, color }: { values: number[]; label: string; color: string }) {
  const max = Math.max(100, ...values)
  const points = values.map((value, index) => {
    const x = values.length < 2 ? 0 : (index / (values.length - 1)) * 180
    const y = 30 - Math.min(100, value / max * 100) * 0.28
    return `${x},${y}`
  }).join(' ')
  return (
    <svg className="metric-sparkline" viewBox="0 0 180 34" role="img" aria-label={`${label}, 60 sampel terakhir`}>
      <polyline points={points} fill="none" stroke={color} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round"/>
    </svg>
  )
}

function ResourceTile({ title, value, detail, percent, history, color }: {
  title: string
  value: string
  detail: string
  percent: number
  history: number[]
  color: string
}) {
  const normalized = Math.min(100, Math.max(0, percent))
  return (
    <article className="resource-tile">
      <div className="resource-tile-head"><span>{title}</span><strong>{percent.toFixed(1)}%</strong></div>
      <strong className="resource-value">{value}</strong>
      <span className="resource-detail">{detail}</span>
      <div className="resource-meter"><span style={{ width: `${normalized}%`, background: color }}/></div>
      <MetricSparkline values={history} label={title} color={color}/>
    </article>
  )
}

function CopyValueButton({ value, label, onCopy }: {
  value: string
  label: string
  onCopy: (value: string, label: string) => void
}) {
  return (
    <button className="icon-btn copy-url-btn" title={`Salin ${label}`} aria-label={`Salin ${label}`}
      onClick={() => onCopy(value, label)}>
      <Copy size={15}/>
    </button>
  )
}

function MonitorDashboard({ metrics, history, error, builds, projects, onOpenBuild, onCopyURL }: {
  metrics: HostResourceMetrics | null
  history: MetricHistoryPoint[]
  error: string
  builds: Build[]
  projects: Project[]
  onOpenBuild: (build: Build) => void
  onCopyURL: (value: string, label: string) => void
}) {
  const projectNames = new Map(projects.map(project => [project.id, project.name]))
  const cpuHistory = history.map(point => point.cpu)
  const memoryHistory = history.map(point => point.memory)
  const hostMonitorURL = `${window.location.origin}/api/monitor/host`
  return (
    <section className="monitor-dashboard">
      <div className="monitor-dashboard-title">
        <div><small>PEMANTAUAN SERVER · MODE PRIVAT</small><h2><Server size={20}/>Sumber daya server</h2></div>
        <span className="monitor-live"><i/>Langsung · 2 detik</span>
      </div>
      {error && <div className="monitor-error">{error}</div>}
      <div className="resource-grid">
        <ResourceTile
          title="Penggunaan CPU server"
          value={metrics ? `${metrics.cpuPercent.toFixed(1)}%` : '—'}
          detail={metrics ? `Diperbarui ${new Date(metrics.sampledAt).toLocaleTimeString()}` : 'Menunggu data server'}
          percent={metrics?.cpuPercent ?? 0}
          history={cpuHistory}
          color="#65b7ff"
        />
        <ResourceTile
          title="Penggunaan RAM server"
          value={metrics ? `${formatBytes(metrics.memoryUsedBytes)} / ${formatBytes(metrics.memoryTotalBytes)}` : '—'}
          detail={metrics ? `${formatBytes(metrics.memoryAvailableBytes)} tersedia` : 'Menunggu data server'}
          percent={metrics?.memoryPercent ?? 0}
          history={memoryHistory}
          color="#45d6a4"
        />
      </div>
      <div className="monitor-api-note">
        <strong><Activity size={15}/> Endpoint server privat</strong>
        <div className="monitor-api-url">
          <code>GET {hostMonitorURL}</code>
          <CopyValueButton value={hostMonitorURL} label="URL monitor server" onCopy={onCopyURL}/>
        </div>
        <code>curl -H 'X-API-Key: $IDE_API_KEY' '{hostMonitorURL}'</code>
      </div>
      <div className="monitor-builds-head"><h3>Build aktif</h3><span>{builds.length}</span></div>
      {builds.length === 0 ? <div className="monitor-build-empty">Tidak ada build yang sedang berjalan.</div> : (
        <div className="monitor-build-list">
          {builds.map(build => (
            <article className="monitor-build-row" key={build.id}>
              <div className="monitor-build-details">
                <strong>{projectNames.get(build.projectId) ?? build.projectId}</strong>
                <span>{build.variant} · {buildStatusLabel(build.status)} · {build.id}</span>
              </div>
              <button className="tool-btn monitor-open-log" onClick={() => onOpenBuild(build)}>
                <FileText size={15}/><span>Log lengkap</span>
              </button>
            </article>
          ))}
        </div>
      )}
    </section>
  )
}

function BuildResourcePanel({ metrics, history }: {
  metrics: BuildResourceMetrics | null
  history: MetricHistoryPoint[]
}) {
  return (
    <section className="build-resource-panel">
      <div className="build-resource-heading"><Activity size={15}/><strong>Sumber daya proses build</strong>
        <span>{metrics ? `Puncak CPU ${metrics.peakCpuPercent.toFixed(1)}%` : 'Menunggu proses build'}</span>
      </div>
      {metrics ? (
        <div className="build-resource-values">
          <div><span>cpu</span><strong>{metrics.cpuPercent.toFixed(1)}%</strong><MetricSparkline values={history.map(point => point.cpu)} label="cpu build" color="#65b7ff"/></div>
          <div><span>ram</span><strong>{formatBytes(metrics.memoryUsedBytes)} / {formatBytes(metrics.memoryLimitBytes)}</strong>
            <small>{metrics.memoryPercent.toFixed(1)}% · peak {formatBytes(metrics.peakMemoryUsedBytes)}</small>
            <MetricSparkline values={history.map(point => point.memory)} label="ram build" color="#45d6a4"/>
          </div>
        </div>
      ) : <p>Data CPU dan RAM tersedia saat proses build berjalan.</p>}
    </section>
  )
}

function BuildLogScreen({ buildId, authMode, variant, status, logs, metrics, history, onBack, onCopy, onCopyURL }: {
  buildId: string
  authMode: AuthStatus['mode']
  variant: BuildVariant
  status: Build['status'] | 'idle'
  logs: string[]
  metrics: BuildResourceMetrics | null
  history: MetricHistoryPoint[]
  onBack: () => void
  onCopy: () => void
  onCopyURL: (value: string, label: string) => void
}) {
  const apiURL = `${window.location.origin}/api/monitor/builds/${encodeURIComponent(buildId)}/logs`
  return (
    <section className="build-log-screen">
      <header className="build-log-screen-head">
        <button className="exit-project-btn" onClick={onBack}><ArrowLeft size={17}/><span>Kembali ke proyek</span></button>
        <div className="build-log-title"><small>LOG BUILD · {buildId}</small><strong>{variant} {buildArtifactLabel(variant)} · {status === 'idle' ? 'Belum dimulai' : buildStatusLabel(status)}</strong></div>
        <button className="tool-btn build-log-copy" onClick={onCopy} disabled={logs.length === 0}><Copy size={16}/><span>Salin log</span></button>
      </header>
      {authMode === 'private' && <BuildResourcePanel metrics={metrics} history={history}/>}
      <div className="build-log-api">
        <span>Stream log build · SSE</span>
        <div className="build-log-api-url">
          <code>GET {apiURL}</code>
          <CopyValueButton value={apiURL} label="URL stream log build" onCopy={onCopyURL}/>
        </div>
        <code>{authMode === 'private'
          ? `curl -N -H 'X-API-Key: $IDE_API_KEY' '${apiURL}'`
          : `curl -N '${apiURL}'`}</code>
        {status !== 'building' && status !== 'queued' && status !== 'preparing' && status !== 'assembling' &&
          <a className="download-btn" href={`/api/builds/${encodeURIComponent(buildId)}/artifact`} download>
            <Download size={15}/>Unduh {buildArtifactLabel(variant)}
          </a>}
      </div>
      <div className="log-view full-log-view" aria-live="polite">
        {logs.map((line, index) => <div key={`${index}-${line}`}><span>{String(index + 1).padStart(4, '0')}</span>{line}</div>)}
      </div>
    </section>
  )
}

function BuildOptionsDialog({ projectType, onStart, onClose }: {
  projectType: ProjectType
  onStart: (options: BuildOptions) => void
  onClose: () => void
}) {
  const [variant, setVariant] = useState<BuildVariant>(projectType === 'java-library' ? 'jar' : 'debug')
  const [signRelease, setSignRelease] = useState(true)
  const [keystore, setKeystore] = useState<File | null>(null)
  const [keyAlias, setKeyAlias] = useState('')
  const [storePassword, setStorePassword] = useState('')
  const [keyPassword, setKeyPassword] = useState('')
  const [error, setError] = useState('')

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError('')
    if (projectType === 'android' && variant === 'release' && signRelease) {
      if (!keystore || !keyAlias.trim() || !storePassword || !keyPassword) {
        setError('Pilih keystore, lalu isi alias dan kedua kata sandi.')
        return
      }
      onStart({ variant, keystore, keyAlias: keyAlias.trim(), storePassword, keyPassword })
      return
    }
    onStart({ variant })
  }

  return (
    <div className="modal-backdrop">
      <section className="modal build-options-modal">
        <div className="modal-head">
          <div><small>{projectType === 'android' ? 'BUILD APK' : 'BUILD JAR'}</small><h2>Pengaturan build</h2></div>
          <button className="icon-btn" title="Tutup" onClick={onClose}><X size={18}/></button>
        </div>
        <form onSubmit={submit}>
          <label>Varian
            <select value={variant} onChange={event => setVariant(event.target.value as BuildVariant)}>
              {projectType === 'android' ? <>
                <option value="debug">Debug · untuk testing</option>
                <option value="release">Release · untuk distribusi</option>
              </> : <>
                <option value="jar">JAR standar · dependency terpisah</option>
                <option value="fat-jar">Fat JAR · dependency runtime disertakan</option>
              </>}
            </select>
          </label>
          {projectType === 'android' && variant === 'release' && (
            <>
              <label className="build-sign-toggle">
                <input type="checkbox" checked={signRelease} onChange={event => setSignRelease(event.target.checked)}/>
                <span>Tandatangani APK dengan keystore</span>
              </label>
              {signRelease ? (
                <>
                  <p className="build-signing-note">Keystore dan kata sandi hanya digunakan untuk build ini. Berkas keystore akan dihapus dari server setelahnya.</p>
                  <label>File keystore (.jks, .keystore, .p12)
                    <input type="file" accept=".jks,.keystore,.p12,.pfx" onChange={event => {
                      const file = event.currentTarget.files?.[0] ?? null
                      if (file && file.size > 32 * 1024 * 1024) {
                        setKeystore(null)
                        setError('Ukuran keystore maksimal 32 MB')
                        event.currentTarget.value = ''
                        return
                      }
                      setError('')
                      setKeystore(file)
                    }}/>
                  </label>
                  <label>Alias kunci<input value={keyAlias} onChange={event => setKeyAlias(event.target.value)} autoComplete="off" required/></label>
                  <div className="form-row">
                    <label>Kata sandi keystore<input type="password" value={storePassword} onChange={event => setStorePassword(event.target.value)} autoComplete="new-password" required/></label>
                    <label>Kata sandi kunci<input type="password" value={keyPassword} onChange={event => setKeyPassword(event.target.value)} autoComplete="new-password" required/></label>
                  </div>
                </>
              ) : (
                <p className="build-signing-note unsigned">Build release tanpa tanda tangan tidak dapat langsung dipasang di Android.</p>
              )}
            </>
          )}
          {error && <div className="build-signing-error">{error}</div>}
          <button className="primary-btn full" type="submit">
            Build {variant === 'fat-jar' ? 'fat JAR' : variant === 'jar' ? 'JAR' : `${variant} APK`}
          </button>
        </form>
      </section>
    </div>
  )
}

function NotificationDialog({ settings, loading, saving, error, onSettingsChange, onSave, onClose }: {
  settings: NotificationSettings | null
  loading: boolean
  saving: boolean
  error: string
  onSettingsChange: (settings: NotificationSettings | null) => void
  onSave: (event: FormEvent<HTMLFormElement>) => void
  onClose: () => void
}) {
  const busy = loading || saving
  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <form className="modal notification-modal" onMouseDown={event => event.stopPropagation()} onSubmit={onSave}>
        <div className="modal-head">
          <div><small>NOTIFIKASI BUILD</small><h2>Notifikasi email build</h2></div>
          <button type="button" className="icon-btn" onClick={onClose}><X size={18}/></button>
        </div>
        {loading ? <p className="notification-info">Memuat pengaturan…</p> : settings && (
          <>
            <div className={`notification-info ${settings.smtpConfigured ? 'configured' : 'not-configured'}`}>
              {settings.smtpConfigured
                ? 'SMTP siap digunakan. Kredensial pengirim tersimpan di data/IDE.config.'
                : 'SMTP belum dikonfigurasi. Lengkapi bagian smtp di data/IDE.config, lalu mulai ulang layanan.'}
            </div>
            <label>Email penerima
              <input type="email" autoComplete="email" maxLength={320} value={settings.recipient}
                onChange={event => onSettingsChange({ ...settings, recipient: event.target.value })}
                placeholder="nama@example.com"/>
            </label>
            <div className="notification-options">
              <label><input type="checkbox" checked={settings.notifySuccess}
                onChange={event => onSettingsChange({ ...settings, notifySuccess: event.target.checked })}/>
                <span><strong>Build berhasil</strong><small>Kirim notifikasi setelah APK selesai dibuat.</small></span></label>
              <label><input type="checkbox" checked={settings.notifyFailure}
                onChange={event => onSettingsChange({ ...settings, notifyFailure: event.target.checked })}/>
                <span><strong>Build gagal</strong><small>Kirim notifikasi jika build gagal.</small></span></label>
            </div>
          </>
        )}
        {error && <div className="login-error">{error}</div>}
        <div className="notification-actions">
          <button type="submit" className="primary-btn" disabled={busy || !settings}>
            {saving ? 'Menyimpan…' : 'Simpan'}
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
        <h1>{loading ? 'Menghubungkan…' : 'Ruang kerja privat'}</h1>
        <p>{offline ? 'Login memerlukan koneksi. Akses offline tersedia setelah Anda masuk melalui peramban ini.' : 'Masukkan kata sandi untuk melanjutkan ke ruang kerja.'}</p>
        <label>Kata sandi<input type="password" autoComplete="current-password" value={password}
          onChange={event => onPasswordChange(event.target.value)} autoFocus disabled={loading || offline} required/></label>
        {error && <div className="login-error">{error}</div>}
        <button className="primary-btn full" type="submit" disabled={busy || loading || offline || !password}>
          {busy ? 'Memeriksa…' : 'Masuk'}
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
          <div><small>PENYIMPANAN SERVER</small><h2>Cache dan data build</h2></div>
          <button className="icon-btn" onClick={onClose}><X size={18}/></button>
        </div>
        <div className="storage-stats">
          <div><span>Ruang kerja build</span><strong>{stats ? formatBytes(stats.workspaceBytes) : '—'}</strong></div>
          <div><span>Hasil build</span><strong>{stats ? formatBytes(stats.artifactBytes) : '—'}</strong></div>
          <div><span>Cache berkas biner</span><strong>{stats ? formatBytes(stats.blobBytes) : '—'}</strong></div>
          <div><span>Cache Gradle · volume Docker</span><strong>{stats ? formatBytes(stats.gradleCacheBytes) : '—'}</strong></div>
          <div><span>Basis data · {stats?.buildHistory ?? 0} riwayat build</span><strong>{stats ? formatBytes(stats.databaseBytes) : '—'}</strong></div>
        </div>
        <p className="storage-paths">Lokasi: <code>DATA_DIR/workspaces</code>, <code>DATA_DIR/artifacts</code>, dan volume <code>androidide_gradle</code>. Proyek di IndexedDB peramban tidak ikut dihapus.</p>
        <div className="cleanup-options">
          <label><input type="checkbox" checked={options.workspaces} onChange={event => onOptionChange('workspaces', event.target.checked)}/><span><strong>Ruang kerja lama</strong><small>Hapus arsip ZIP sumber dan keluaran build yang tersisa.</small></span></label>
          <label><input type="checkbox" checked={options.artifacts} onChange={event => onOptionChange('artifacts', event.target.checked)}/><span><strong>Hasil build</strong><small>Hasil build yang dihapus tidak dapat diunduh kembali.</small></span></label>
          <label><input type="checkbox" checked={options.history} onChange={event => onOptionChange('history', event.target.checked)}/><span><strong>Riwayat dan log build</strong><small>Hapus referensi cache biner terkait dan jalankan pembersihan cache.</small></span></label>
          <label><input type="checkbox" checked={options.gradleCache} onChange={event => onOptionChange('gradleCache', event.target.checked)}/><span><strong>Cache dependensi Gradle</strong><small>Build berikutnya akan mengunduh ulang dependensi.</small></span></label>
        </div>
        <div className="storage-actions">
          <button className="tool-btn refresh-storage" onClick={onRefresh} disabled={loading}>{loading ? 'Memuat…' : 'Perbarui ukuran'}</button>
          <button className="danger-btn" onClick={onCleanup} disabled={loading}>{loading ? 'Membersihkan…' : 'Hapus pilihan'}</button>
        </div>
      </div>
    </div>
  )
}

function ArtifactDialog({ artifacts, projects, loading, deletingId, error, onRefresh, onDelete, onClose }: {
  artifacts: BuildArtifact[]
  projects: Project[]
  loading: boolean
  deletingId: string | null
  error: string
  onRefresh: () => void
  onDelete: (artifact: BuildArtifact) => void
  onClose: () => void
}) {
  const projectNames = new Map(projects.map(project => [project.id, project.name]))
  const grouped = new Map<string, BuildArtifact[]>()
  for (const artifact of artifacts) {
    const items = grouped.get(artifact.projectId) ?? []
    items.push(artifact)
    grouped.set(artifact.projectId, items)
  }

  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className="modal artifact-modal" onMouseDown={event => event.stopPropagation()}>
        <div className="modal-head">
          <div><small>ARSIP BUILD</small><h2>Hasil build tersimpan</h2></div>
          <div className="artifact-head-actions">
            <button className="icon-btn" title="Muat ulang" aria-label="Muat ulang" onClick={onRefresh} disabled={loading}>
              <RefreshCw size={16}/>
            </button>
            <button className="icon-btn" title="Tutup" aria-label="Tutup" onClick={onClose}><X size={18}/></button>
          </div>
        </div>
        <div className="artifact-summary">
          {loading ? 'Memuat daftar hasil build…' : `${artifacts.length} hasil build dalam ${grouped.size} proyek`}
        </div>
        {error && <div className="login-error">{error}</div>}
        {!loading && artifacts.length === 0 && !error && (
          <div className="artifact-empty">Belum ada hasil build.</div>
        )}
        <div className="artifact-groups">
          {[...grouped.entries()].map(([projectId, items]) => (
            <section className="artifact-group" key={projectId}>
              <div className="artifact-project">
                <strong>{projectNames.get(projectId) ?? projectId}</strong>
                <span>{items.length} hasil build</span>
              </div>
              {items.map(artifact => (
                <div className="artifact-row" key={artifact.buildId}>
                  <div className="artifact-info">
                    <strong>{artifact.filename}</strong>
                    <span>{artifact.variant} · {new Date(artifact.createdAt).toLocaleString()} · {formatBytes(artifact.size)}</span>
                    {!artifact.available && <small className="artifact-missing">Berkas hasil build tidak tersedia di server.</small>}
                  </div>
                  <div className="artifact-actions">
                    {artifact.available
                      ? <a className="icon-btn" title={`Unduh ${artifact.projectType === 'java-library' ? 'JAR' : 'APK'}`} aria-label={`Unduh ${artifact.projectType === 'java-library' ? 'JAR' : 'APK'}`}
                          href={`/api/builds/${encodeURIComponent(artifact.buildId)}/artifact`} download>
                          <Download size={16}/>
                        </a>
                      : <button className="icon-btn" title="Hasil build tidak tersedia" aria-label="Hasil build tidak tersedia" disabled><Download size={16}/></button>}
                    <button className="icon-btn artifact-delete" title="Hapus hasil build" aria-label="Hapus hasil build"
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
  const [name, setName] = useState('Aplikasi Saya')
  const [packageName, setPackageName] = useState('com.example.myapp')
  const [projectType, setProjectType] = useState<ProjectType>('android')
  const [language, setLanguage] = useState<'kotlin' | 'java'>('kotlin')
  const [minSdk, setMinSdk] = useState(24)
  const [targetSdk, setTargetSdk] = useState(34)
  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className="modal" onMouseDown={e => e.stopPropagation()}>
        <div className="modal-head"><div><small>PROYEK BARU</small><h2>{projectType === 'android' ? 'Aplikasi Android' : 'Pustaka Java'}</h2></div><button className="icon-btn" onClick={onClose}><X size={18}/></button></div>
        <label>Jenis proyek<select value={projectType} onChange={e => setProjectType(e.target.value as ProjectType)}>
          <option value="android">Aplikasi Android</option><option value="java-library">Pustaka Java (.jar)</option>
        </select></label>
        <label>Nama proyek<input value={name} onChange={e => setName(e.target.value)}/></label>
        <label>{projectType === 'android' ? 'Nama paket' : 'Paket / grup Java'}<input value={packageName} onChange={e => setPackageName(e.target.value)} placeholder="com.example.myapp"/></label>
        {projectType === 'android' && <>
          <div className="form-row"><label>Bahasa<select value={language} onChange={e => setLanguage(e.target.value as 'kotlin' | 'java')}><option value="kotlin">Kotlin</option><option value="java">Java</option></select></label><label>Templat<select><option>Aplikasi kosong</option></select></label></div>
          <div className="form-row"><label>Min SDK<input type="number" value={minSdk} onChange={e => setMinSdk(Number(e.target.value))}/></label><label>Target SDK<input type="number" value={targetSdk} onChange={e => setTargetSdk(Number(e.target.value))}/></label></div>
        </>}
        <button className="primary-btn full" disabled={!name.trim() || !/^([a-zA-Z_]\w*\.)+[a-zA-Z_]\w*$/.test(packageName)}
          onClick={() => onCreate({ name: name.trim(), packageName: packageName.trim(), language: projectType === 'java-library' ? 'java' : language, projectType, minSdk, targetSdk })}>Buat proyek</button>
      </div>
    </div>
  )
}

export default App
