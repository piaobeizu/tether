import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import Canvas from './index'
import { useStore } from '../../lib/store'
import { fetchFile } from '../../lib/aihub'

// Mock the aihub client so FileMode/DetailMode never hit the network — only
// fetchFile is exercised by these tests (file-mode markdown/code rendering +
// header dedup + XSS). fetchItem/fetchSteps are mocked too since Canvas
// imports them, but no test here selects a work item.
vi.mock('../../lib/aihub', async () => {
  const actual = await vi.importActual<typeof import('../../lib/aihub')>('../../lib/aihub')
  return {
    ...actual,
    fetchItem: vi.fn(),
    fetchSteps: vi.fn(),
    fetchFile: vi.fn(),
  }
})

const mockFetchFile = vi.mocked(fetchFile)

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

describe('Canvas — empty state when nothing is selected (tether#26)', () => {
  it('shows the empty-state hint (the Work map moved to the right tab)', () => {
    // no file selected → the middle canvas is now just an empty-state hint; the
    // wi-relationship map moved out of the middle into the right Work tab.
    const { container } = render(<Canvas />)
    expect(container.querySelector('.canvas-empty')).toBeTruthy()
    // the middle no longer mounts the Work map (nor its "select a project" hint)
    expect(screen.queryByText('select a project')).toBeNull()
  })
})
