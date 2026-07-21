import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { CopyButton } from './CopyButton'

afterEach(() => cleanup())

// tether#42 — CopyButton copies getText() to the clipboard on click, with a
// brief ✓ confirmation. getText is resolved at click time so callers can copy
// live content (e.g. a code block's rendered textContent).
describe('CopyButton (tether#42)', () => {
  const stubClipboard = () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    return writeText
  }

  it('writes getText() to the clipboard on click', () => {
    const writeText = stubClipboard()
    render(<CopyButton getText={() => 'hello world'} />)
    fireEvent.click(screen.getByRole('button'))
    expect(writeText).toHaveBeenCalledWith('hello world')
  })

  it('shows a ✓ after copying', () => {
    stubClipboard()
    const { container } = render(<CopyButton getText={() => 'x'} />)
    expect(container.querySelector('.copy-ok')).toBeNull()
    fireEvent.click(screen.getByRole('button'))
    expect(container.querySelector('.copy-ok')?.textContent).toBe('✓')
  })

  it('does nothing when getText returns empty (no clipboard write, no ✓)', () => {
    const writeText = stubClipboard()
    const { container } = render(<CopyButton getText={() => ''} />)
    fireEvent.click(screen.getByRole('button'))
    expect(writeText).not.toHaveBeenCalled()
    expect(container.querySelector('.copy-ok')).toBeNull()
  })
})
