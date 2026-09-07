import { describe, it, expect, afterEach, vi } from 'vitest'
import { waitFor } from '@testing-library/react'

// Exercises web/src/main.tsx's own '/auth' branch directly -- the one line
// that IS tether#186's fix -- rather than a router or an extracted
// predicate. main.tsx reads window.location.pathname as a top-level side
// effect at import time, so each case stubs the pathname BEFORE importing,
// and vi.resetModules() forces that top-level code to re-run on the next
// import (ES module caching would otherwise reuse the first import's
// snapshot and the second case would just observe the first case's render).

const originalLocation = window.location

function stubPathname(pathname: string) {
  Object.defineProperty(window, 'location', {
    value: { ...originalLocation, pathname },
    writable: true,
  })
}

describe('main entry point routing', () => {
  afterEach(() => {
    Object.defineProperty(window, 'location', { value: originalLocation, writable: true })
  })

  it('renders the AuthPage login form when the pathname is /auth', async () => {
    document.body.innerHTML = '<div id="root"></div>'
    stubPathname('/auth')
    vi.resetModules()
    await import('./main')

    await waitFor(() => {
      expect(document.querySelector('#tether-access-token')).not.toBeNull()
    })
  })

  // tether#195 replaced tether#174's bare `<p>tether</p>` with the real shell, so
  // this case now asserts the shell root rather than a word. The old assertion was
  // `textContent` containing 'tether', which the shell does not print anywhere and
  // which would also have passed on any page that happened to say "tether"
  // somewhere — the structural check is both correct now and stronger then.
  it('renders the shell (no login form) on any other pathname', async () => {
    document.body.innerHTML = '<div id="root"></div>'
    stubPathname('/')
    vi.resetModules()
    await import('./main')

    await waitFor(() => {
      expect(document.querySelector('.sh-root')).not.toBeNull()
    })
    expect(document.querySelector('#tether-access-token')).toBeNull()
  })
})
