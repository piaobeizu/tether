import { useEffect, useRef } from 'react'

// Shell pane — xterm.js PTY terminal wired to /wt/shell in s6.
export default function ShellPane() {
  const containerRef = useRef<HTMLDivElement>(null)

  // xterm.js initialization wired in s6.
  useEffect(() => {
    // placeholder: xterm attach in s6
  }, [])

  return (
    <>
      <div className="pane-header">Shell</div>
      <div
        ref={containerRef}
        className="pane-body"
        style={{ background: '#0a0a0a', color: '#ccc', fontFamily: 'monospace', fontSize: 12 }}
      >
        shell terminal (xterm.js wired in s6)
      </div>
    </>
  )
}
