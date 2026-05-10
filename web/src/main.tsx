import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import AuthPage from './AuthPage'
import './index.css'

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
