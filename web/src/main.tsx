import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import AuthPage from './AuthPage'
import './index.css'

// Dark mode: persist + restore theme on load.
//
// This runs BEFORE createRoot below, and it has to: the attribute must be on
// <html> before first paint or every dark-mode load flashes light. That is also
// why the theme is not a store field — see the theme note in Settings.tsx.
const savedTheme = localStorage.getItem('tether_theme')
if (savedTheme === 'dark') {
  document.documentElement.setAttribute('data-theme', 'dark')
}

// Cmd+Shift+D (Mac) / Ctrl+Shift+D toggles dark mode.
//
// Setting the attribute is the whole notification (tether#129). Settings.tsx
// observes `data-theme` with a MutationObserver rather than being told, so this
// handler owes it nothing — do not add an event dispatch here to "keep Settings
// in sync". Before tether#129 Settings held a mount-time COPY of the attribute
// and this handler silently made it stale.
document.addEventListener('keydown', (e) => {
  const mod = e.metaKey || e.ctrlKey
  if (mod && e.shiftKey && e.key.toLowerCase() === 'd') {
    e.preventDefault()
    const isDark = document.documentElement.getAttribute('data-theme') === 'dark'
    if (isDark) {
      document.documentElement.removeAttribute('data-theme')
      localStorage.setItem('tether_theme', 'light')
    } else {
      document.documentElement.setAttribute('data-theme', 'dark')
      localStorage.setItem('tether_theme', 'dark')
    }
  }
})

if (window.location.pathname === '/auth') {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <AuthPage />
    </StrictMode>,
  )
} else {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  )
}
