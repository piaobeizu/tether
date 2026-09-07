import { createRoot } from 'react-dom/client'
import AuthPage from './AuthPage'

// Phase-1 scaffold (tether#174). The old SPA — 82 files under web/src plus five
// specs under web/test — was deleted on this branch; tether#173 decision 3 keeps
// it running on `main` as the visual/behavioural control while the replacement is
// written.
//
// What has to survive that deletion is the BUILD, not the UI. web/embed.go is a Go
// package (`//go:embed all:dist`) imported by internal/server/lifecycle.go:35 and
// internal/server/static.go:12, and `all:dist` cannot be satisfied by a committed
// placeholder — vite's emptyOutDir wipes web/dist on every build, which is
// tether#81's conclusion and is argued out in embed.go's own header. So an empty
// web/ would stop `go build`, `go vet`, `go test`, `go list ./...` and gopls dead,
// for every commit on this branch until the new shell lands. This file is the
// smallest thing that keeps `pnpm build` producing a dist/, and therefore the
// acceptance criterion "GOWORK=off go build ./... passes on every commit of the UI
// branch".
//
// Deliberately not a component and deliberately not styled. A placeholder
// component is something a reviewer of the shell wi has to decide whether to keep,
// and a styled placeholder invites the next person to extend it instead of
// replacing it. It does go through React rather than plain DOM, because that is
// what makes `pnpm build` prove the JSX transform and @vitejs/plugin-react are
// actually wired — a plain-DOM placeholder would build green with the react plugin
// misconfigured.
//
// tether#186 carves out an exception for requests landing on `/auth`: they
// render the restored login page instead of this placeholder, matching the
// pathname check the deleted old main.tsx used, and what
// internal/server/static.go still serves index.html for.
const root = document.getElementById('root')
if (!root) {
  throw new Error('#root missing from index.html')
}
createRoot(root).render(window.location.pathname === '/auth' ? <AuthPage /> : <p>tether</p>)
