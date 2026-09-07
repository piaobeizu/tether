import { createRoot } from 'react-dom/client'
import AuthPage from './AuthPage'
import Shell from './shell/Shell'

// The old SPA — 82 files under web/src plus five specs under web/test — was
// deleted on this branch by tether#174; tether#173 decision 3 keeps it running on
// `main` as the visual/behavioural control while the replacement is written.
//
// What had to survive that deletion is the BUILD, not the UI. web/embed.go is a Go
// package (`//go:embed all:dist`) imported by internal/server/lifecycle.go:35 and
// internal/server/static.go:12, and `all:dist` cannot be satisfied by a committed
// placeholder — vite's emptyOutDir wipes web/dist on every build, which is
// tether#81's conclusion and is argued out in embed.go's own header. So an empty
// web/ would stop `go build`, `go vet`, `go test`, `go list ./...` and gopls dead.
//
// tether#174 held that open with a bare `<p>tether</p>`, and its comment said the
// placeholder was deliberately not a component so that "a reviewer of the shell wi
// has to decide whether to keep it". tether#195 is that wi and the decision is to
// replace it: `Shell` is the real root now. The `/auth` carve-out from tether#186
// stays exactly as it was — it matches the pathname check the deleted pre-#173
// main.tsx used, and what internal/server/static.go still serves index.html for.
//
// Routing is this one pathname test and nothing more. A router package would be a
// dependency bought for a single branch, and the daemon serves index.html for
// every non-API path, so the SHELL's own navigation is pane selection (see
// shell/selection.ts) rather than URL navigation. If URLs per pane are wanted
// later that is a deliberate change, not a gap here.
const root = document.getElementById('root')
if (!root) {
  throw new Error('#root missing from index.html')
}
createRoot(root).render(window.location.pathname === '/auth' ? <AuthPage /> : <Shell />)
