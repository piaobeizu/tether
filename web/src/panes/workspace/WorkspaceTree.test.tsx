// tether#20 Task 11 — clicking a file (not dir) row must select it in the
// shared canvas-selection store slice, with the path relative to the
// workspace root (matching fetchFile's `path` param). Directory rows must
// keep expanding/collapsing rather than selecting.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, fireEvent, waitFor } from '@testing-library/react'
import WorkspaceTree from './WorkspaceTree'
import { useStore } from '../../lib/store'

interface Entry { name: string; isDir: boolean; dirty: boolean }

function mockFiles(entries: Entry[]) {
  vi.stubGlobal('fetch', vi.fn(async () => ({
    ok: true,
    json: async () => entries,
  })))
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('WorkspaceTree file selection', () => {
  beforeEach(() => {
    useStore.setState({ selectedWiId: null, selectedFile: null })
  })

  it('clicking a file row selects it with workspace id + relative path', async () => {
    mockFiles([
      { name: 'src', isDir: true, dirty: false },
      { name: 'README.md', isDir: false, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('README.md'))
    fireEvent.click(screen.getByText('README.md'))

    expect(useStore.getState().selectedFile).toEqual({ wsId: 'ws-1', path: 'README.md' })
    expect(useStore.getState().selectedWiId).toBeNull()
  })

  it('clicking a directory row expands it instead of selecting a file', async () => {
    mockFiles([{ name: 'src', isDir: true, dirty: false }])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('src'))
    fireEvent.click(screen.getByText('src'))

    expect(useStore.getState().selectedFile).toBeNull()
  })
})
