import { createRef } from 'react'

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { Button, buttonVariants, Input } from '../src/ui/primitives'

// 🔴 `cleanup` has to be called by hand here. @testing-library/react registers it
// in a global `afterEach` only when one is on globalThis, and web/vite.config.ts
// does not set vitest's `globals: true`. Without this the rendered trees pile up
// in one jsdom document across every `it` in the file, and the failure it produces
// says "Found multiple elements" rather than anything about the primitives.
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

// Behaviour of the vendored CloudCLI UI primitives, reached through tether's wrap
// barrel. tether#194.
//
// ⚠️ There is deliberately NO assertion here about how anything LOOKS, and adding
// one is the trap this file is positioned against. vitest runs in jsdom, and jsdom
// runs no PostCSS: `getComputedStyle(button).backgroundColor` returns exactly the
// same value whether tailwind is wired perfectly or not installed at all. An
// assertion on it would be green in both states, which makes it worse than no
// assertion — it would look like coverage of the one thing it cannot see.
//
// Whether the stylesheet is real is a question about web/dist, and it is asked
// there, by scripts/check-tailwind-emitted.sh. What THIS file can answer is
// whether the primitives behave: whether the composed class string is computed
// rather than concatenated, whether cva's variant selection works, whether the
// ref reaches the DOM node, and whether the elements respond to input.
//
// ── on the two literal class names below ────────────────────────────────────
// `bg-primary` and `bg-destructive` are read out of upstream's variant table, so
// in the strict sense they are a copy of the object under test. That is deliberate
// and it is safe here for a reason that does not generalise: the file they come
// from is byte-locked by scripts/check-vendor-provenance.sh, so the copy cannot
// drift silently — the only way it can go stale is an absorption, which moves the
// pin and puts this test's red squarely in the diff that caused it. A variant
// upstream renames is a change tether should be told about, not one to paper over.
describe('vendored primitives: Button', () => {
  // The sharpest assertion in the file. `cn` is upstream's
  // twMerge(clsx(...)) helper, vendored as plain JS at
  // web/src/vendor/cloudcli/src/lib/utils.js. tailwind-merge's job is to make the
  // LAST conflicting utility in a class list win, so passing a height cancels the
  // size variant's height instead of sitting beside it.
  //
  // Nothing but the real helper produces that. A naive template concatenation, a
  // `clsx`-only stub, or a `cn` that got dropped in the wrap would all leave both
  // classes present and this test red — which is what makes it a check on the
  // vendored dependency being genuinely wired, not on an import resolving.
  it('resolves conflicting utilities through tailwind-merge, last one winning', () => {
    render(<Button className="h-20">press</Button>)
    const classes = screen.getByRole('button').className.split(/\s+/)

    expect(classes).toContain('h-20')
    expect(classes).not.toContain('h-10')
  })

  it('applies cva default variants when none are given', () => {
    render(<Button>press</Button>)
    const classes = screen.getByRole('button').className.split(/\s+/)

    expect(classes).toContain('bg-primary')
    expect(classes).not.toContain('bg-destructive')
  })

  it('switches the variant class set when a variant is selected', () => {
    render(<Button variant="destructive">delete</Button>)
    const classes = screen.getByRole('button').className.split(/\s+/)

    expect(classes).toContain('bg-destructive')
    expect(classes).not.toContain('bg-primary')
  })

  // buttonVariants is exported alongside Button so callers can compose the same
  // class set onto something that is not a <button> (a link, typically). Asserted
  // because it is part of the barrel's surface: if it stopped agreeing with what
  // the component renders, every such caller would silently drift from it.
  // Not compared as equal strings, and the reason is the point of the previous
  // test: the component renders `cn(buttonVariants(...))`, so what reaches the DOM
  // is the twMerge-resolved form while the helper returns the raw one. At this pin
  // the raw form carries `rounded-md` and `text-sm` twice each — once from the base
  // and once from `size: 'sm'` — so string equality is false on a correctly wired
  // build. Asserting the two relations that do hold says more than an equality
  // would anyway: the DOM invents nothing the helper did not offer, and it carries
  // no duplicates.
  it('exports a variant helper that agrees with what the component renders', () => {
    render(
      <Button variant="outline" size="sm">
        edit
      </Button>,
    )
    const offered = new Set(
      buttonVariants({ variant: 'outline', size: 'sm' }).split(/\s+/).filter(Boolean),
    )
    const rendered = screen.getByRole('button').className.split(/\s+/).filter(Boolean)

    expect(rendered.filter((c) => !offered.has(c))).toEqual([])
    expect(new Set(rendered).size).toBe(rendered.length)
    expect(rendered).toContain('border-input')
    expect(rendered).toContain('h-9')
  })

  it('forwards its ref to the underlying button element', () => {
    const ref = createRef<HTMLButtonElement>()
    render(<Button ref={ref}>press</Button>)

    expect(ref.current).toBe(screen.getByRole('button'))
    expect(ref.current?.tagName).toBe('BUTTON')
  })

  it('passes DOM props through and does not fire when disabled', () => {
    const onClick = vi.fn()
    render(
      <Button disabled onClick={onClick}>
        press
      </Button>,
    )
    const button = screen.getByRole('button') as HTMLButtonElement

    expect(button.disabled).toBe(true)
    button.click()
    expect(onClick).not.toHaveBeenCalled()
  })

  it('fires its click handler when enabled', () => {
    const onClick = vi.fn()
    render(<Button onClick={onClick}>press</Button>)

    screen.getByRole('button').click()
    expect(onClick).toHaveBeenCalledTimes(1)
  })
})

describe('vendored primitives: Input', () => {
  it('forwards the type attribute to the underlying input', () => {
    render(<Input type="password" aria-label="secret" />)

    expect(screen.getByLabelText('secret')).toHaveProperty('type', 'password')
  })

  it('merges a caller class over its own without duplicating the conflict', () => {
    render(<Input aria-label="q" className="h-20" />)
    const classes = screen.getByLabelText('q').className.split(/\s+/)

    expect(classes).toContain('h-20')
    expect(classes).not.toContain('h-9')
  })

  // `fireEvent.change` rather than a hand-dispatched `input` event: react tracks
  // the previous value on the DOM node and drops a synthetic change when it has
  // not moved, so dispatching `new Event('input')` against a controlled input
  // whose value is unchanged calls nothing at all. Measured — that was this test's
  // first version, and it failed with 0 calls on a working component.
  // The edited value is captured INSIDE the handler, not read back off
  // `mock.calls[0][0].target` afterwards. `event.target` is the live DOM node, and
  // for a controlled input whose onChange sets no state react restores the node's
  // value to the `value` prop right after the handler returns — so reading it later
  // reports the OLD value and the test fails on a working component. Measured:
  // that was this test's second version, asserting 'hello world' and getting
  // 'hello'.
  //
  // Both halves are asserted deliberately, because together they are what
  // "controlled" means: the edit is reported, and it does not stick on its own.
  it('reports edits through onChange and keeps the controlled value', () => {
    const seen: string[] = []
    render(
      <Input aria-label="q" value="hello" onChange={(e) => seen.push(e.target.value)} />,
    )
    const input = screen.getByLabelText('q') as HTMLInputElement

    expect(input.value).toBe('hello')

    fireEvent.change(input, { target: { value: 'hello world' } })

    expect(seen).toEqual(['hello world'])
    expect(input.value).toBe('hello')
  })

  it('forwards its ref to the underlying input element', () => {
    const ref = createRef<HTMLInputElement>()
    render(<Input aria-label="q" ref={ref} />)

    expect(ref.current).toBe(screen.getByLabelText('q'))
    expect(ref.current?.tagName).toBe('INPUT')
  })
})
