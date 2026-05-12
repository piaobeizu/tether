import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import AuthPage from './AuthPage'
import './index.css'

// Dark mode: persist + restore theme on load.
const savedTheme = localStorage.getItem('tether_theme')
if (savedTheme === 'dark') {
  document.documentElement.setAttribute('data-theme', 'dark')
}

// Cmd+Shift+D (Mac) / Ctrl+Shift+D toggles dark mode.
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
