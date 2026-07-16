import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import Canvas from './index'
import { useStore } from '../../lib/store'
import { fetchFile, fetchWorkspaces } from '../../lib/aihub'

// Mock the aihub client so FileMode/CanvasHome never hit the network — only
// fetchFile + fetchWorkspaces are exercised by these tests. fetchItem/fetchSteps
// are mocked too since Canvas imports them, but no test here selects a work item.
vi.mock('../../lib/aihub', async () => {
  const actual = await vi.importActual<typeof import('../../lib/aihub')>('../../lib/aihub')
  return {
    ...actual,
    fetchItem: vi.fn(),
    fetchSteps: vi.fn(),
    fetchFile: vi.fn(),
    fetchWorkspaces: vi.fn(),
  }
})

const mockFetchFile = vi.mocked(fetchFile)
const mockFetchWorkspaces = vi.mocked(fetchWorkspaces)

// @testing-library/react's auto-cleanup relies on a global `afterEach`, which
// isn't registered since vitest's `globals` option is off (matches Dag.test.tsx
// — no implicit globals). Clean up explicitly instead, and reset the shared
// zustand store's selection so tests don't leak into each other.
afterEach(() => {
  cleanup()
  useStore.getState().select(null)
  vi.clearAllMocks()
})

describe('Canvas — FileMode markdown rendering (tether#21)', () => {
  it('renders a .md file as real markdown, not a <pre> block', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'a.md',
      content: '# Title\n\n- item',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'a.md' } })

    const { container } = render(<Canvas />)

    await screen.findByText('Title')
    const h1 = container.querySelector('h1')
    expect(h1?.textContent).toBe('Title')

    const li = container.querySelector('li')
    expect(li?.textContent).toBe('item')

    // must NOT be wrapped in the plain-text <pre> fallback once resolved
    expect(container.querySelector('pre')).toBeNull()
  })

  it('renders a non-markdown file in a <pre> block, unrendered', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'main.go',
      content: 'package main',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'main.go' } })

    const { container } = render(<Canvas />)

    await screen.findByText('package main')
    const pre = container.querySelector('pre')
    expect(pre?.textContent).toBe('package main')
    expect(container.querySelector('h1')).toBeNull()
  })
})

describe('Canvas — FileMode header is a single full-path line (tether#21)', () => {
  it('shows just the name for a root file (path == filename)', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'CHANGELOG.md',
      content: 'notes',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'CHANGELOG.md' } })

    const { container } = render(<Canvas />)

    await screen.findByText('notes')
    expect(container.querySelector('.canvas-slug')?.textContent).toBe('CHANGELOG.md')
    // no separate basename / duplicate line
    expect(container.querySelector('.canvas-file-path')).toBeNull()
  })

  it('shows the full relative path (no separate basename) for a nested file', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'internal/x.go',
      content: 'package internal',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'internal/x.go' } })

    const { container } = render(<Canvas />)

    await screen.findByText('package internal')
    expect(container.querySelector('.canvas-slug')?.textContent).toBe('internal/x.go')
    expect(container.querySelector('.canvas-file-path')).toBeNull()
  })
})

describe('Canvas — markdown XSS safety (tether#21)', () => {
  it('renders an <img onerror> payload inert, no script execution surface', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'evil.md',
      content: '<img src=x onerror="alert(1)">',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'evil.md' } })

    const { container } = render(<Canvas />)

    // react-markdown's default (no rehype-raw) escapes embedded HTML — it
    // never becomes a real <img> element with a live onerror handler.
    // `selector: '.md-body'` restricts the match to that single container so
    // the substring matcher doesn't hit every ancestor element too.
    await screen.findByText(t => t.includes('onerror'), { selector: '.md-body' })
    expect(container.querySelector('img')).toBeNull()
  })

  it('renders a <script> payload as inert text, never as a real script element', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'evil2.md',
      content: '<script>alert(1)</script>',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'evil2.md' } })

    const { container } = render(<Canvas />)

    await screen.findByText(t => t.includes('alert(1)'), { selector: '.md-body' })
    expect(container.querySelector('script')).toBeNull()
  })
})

describe('Canvas — home when nothing is selected (tether#33)', () => {
  it('renders the branded home with quick actions (not a lone hint)', () => {
    mockFetchWorkspaces.mockResolvedValue([])
    render(<Canvas />)
    expect(screen.getByText('tether')).toBeTruthy()
    expect(screen.getByText('对话')).toBeTruthy()
    expect(screen.getByText('选个 wi')).toBeTruthy()
    expect(screen.getByText('打开文件')).toBeTruthy()
  })

  it('shows the primary workspace name + path (and count when multiple)', async () => {
    mockFetchWorkspaces.mockResolvedValue([
      { id: 'w1', name: 'tether', path: '/root/code/tether' },
      { id: 'w2', name: 'aihub', path: '/root/code/aihub' },
    ])
    const { container } = render(<Canvas />)
    await screen.findByText('/root/code/tether')
    const wsText = container.querySelector('.canvas-home-ws')?.textContent ?? ''
    expect(wsText).toContain('workspace')
    expect(wsText).toContain('tether')
    expect(wsText).toContain('+1 more')
  })

  it('still renders home (brand + actions) when the workspace fetch fails, with no workspace line', async () => {
    mockFetchWorkspaces.mockRejectedValue(new Error('boom'))
    const { container } = render(<Canvas />)
    expect(screen.getByText('tether')).toBeTruthy()
    expect(screen.getByText('对话')).toBeTruthy()
    // Flush microtasks + a macrotask so the rejected fetch fully settles; this
    // would catch a regression where the .catch erroneously set workspace state.
    await new Promise((r) => setTimeout(r, 0))
    expect(container.querySelector('.canvas-home-ws')).toBeNull()
  })

  it('quick actions dispatch the expected window events', () => {
    mockFetchWorkspaces.mockResolvedValue([])
    const events: string[] = []
    const onSelect = (e: Event) => events.push('select-tab:' + (e as CustomEvent).detail)
    const onFocus = () => events.push('focus-files')
    window.addEventListener('tether:select-tab', onSelect)
    window.addEventListener('tether:focus-files', onFocus)
    render(<Canvas />)
    fireEvent.click(screen.getByText('对话'))
    fireEvent.click(screen.getByText('选个 wi'))
    fireEvent.click(screen.getByText('打开文件'))
    window.removeEventListener('tether:select-tab', onSelect)
    window.removeEventListener('tether:focus-files', onFocus)
    expect(events).toEqual(['select-tab:chat', 'select-tab:work', 'focus-files'])
  })
})
