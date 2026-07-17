import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { ThinkingBlock, fmtThinkMs } from './index'

// tether#34 — ThinkingBlock is exported and prop-controlled so it tests directly,
// without mounting ChatPane (which opens a WebTransport connection on mount).

// vitest globals are off (matches Canvas.test.tsx), so register cleanup explicitly.
afterEach(() => cleanup())

describe('fmtThinkMs (tether#34)', () => {
  it('formats whole seconds, sub-10s decimals, and minutes', () => {
    expect(fmtThinkMs(8000)).toBe('8s')
    expect(fmtThinkMs(1200)).toBe('1.2s')
    expect(fmtThinkMs(500)).toBe('0.5s')
    expect(fmtThinkMs(12000)).toBe('12s')
    expect(fmtThinkMs(90000)).toBe('1m 30s')
  })
  it('handles rounding boundaries (review MINOR): no "10.0s", no "1m 60s"', () => {
    expect(fmtThinkMs(9999)).toBe('10s')
    expect(fmtThinkMs(119500)).toBe('2m 0s')
  })
  it('returns empty string for undefined/negative durations', () => {
    expect(fmtThinkMs(undefined)).toBe('')
    expect(fmtThinkMs(-5)).toBe('')
  })
})

describe('ThinkingBlock (tether#34)', () => {
  it('renders live streaming thinking while live', () => {
    render(<ThinkingBlock thinking="pondering the plan" live expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('思考中…')).toBeTruthy()
    expect(screen.getByText('pondering the plan')).toBeTruthy()
  })

  it('collapses to a "思考 Xs" summary once no longer live, hiding the text', () => {
    render(<ThinkingBlock thinking="secret reasoning" thinkingMs={8000} live={false} expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('思考 8s')).toBeTruthy()
    expect(screen.queryByText('secret reasoning')).toBeNull()
  })

  it('a collapsed thinking-only turn (no duration) still shows a bare "思考" summary', () => {
    render(<ThinkingBlock thinking="only thought" live={false} expanded={false} onToggle={() => {}} />)
    expect(screen.getByText('思考')).toBeTruthy()
    expect(screen.queryByText('only thought')).toBeNull()
  })

  it('shows the thinking text when expanded', () => {
    render(<ThinkingBlock thinking="secret reasoning" thinkingMs={8000} live={false} expanded onToggle={() => {}} />)
    expect(screen.getByText('secret reasoning')).toBeTruthy()
  })

  it('clicking the collapsed summary calls onToggle', () => {
    const onToggle = vi.fn()
    render(<ThinkingBlock thinking="x" thinkingMs={8000} live={false} expanded={false} onToggle={onToggle} />)
    fireEvent.click(screen.getByText('思考 8s'))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })
})
