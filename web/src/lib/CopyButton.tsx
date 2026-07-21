import { useState } from 'react'

// CopyButton — copies text to the clipboard on click with a brief ✓ (tether#42).
// `getText` is resolved at click time so callers can copy live content (e.g. a
// code block's rendered textContent). Uses the same navigator.clipboard path
// already used elsewhere (MediaBlock copy-path); a missing clipboard (older /
// insecure context) is a safe no-op via optional chaining.
export function CopyButton({ getText, className, label = 'Copy' }: {
  getText: () => string
  className?: string
  label?: string
}) {
  const [copied, setCopied] = useState(false)
  const onClick = () => {
    const text = getText()
    if (!text) return
    // Swallow a denied/failed copy (permission, unfocused doc) so it doesn't
    // surface as an unhandled rejection; the ✓ is optimistic (review N2).
    navigator.clipboard?.writeText(text)?.catch(() => {})
    setCopied(true)
    // Transient confirmation; best-effort reset (no cleanup needed).
    window.setTimeout(() => setCopied(false), 1200)
  }
  return (
    <button
      type="button"
      className={'copy-btn' + (className ? ' ' + className : '')}
      onClick={onClick}
      aria-label={label}
      title={label}
    >
      {copied ? (
        <span className="copy-ok" aria-hidden="true">✓</span>
      ) : (
        <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" aria-hidden="true">
          <rect x="5.5" y="5.5" width="8" height="8" rx="1.3" />
          <path d="M10.5 5.5V4A1.3 1.3 0 0 0 9.2 2.7H4A1.3 1.3 0 0 0 2.7 4v5.2A1.3 1.3 0 0 0 4 10.5h1.5" />
        </svg>
      )}
    </button>
  )
}
