package adapter

// Codex is a v0.1 stub. Spec §11.Z.4: v0.2 will implement textual rewrite
// of skills/ + .codex-plugin/plugin.json generation (~200 LOC).
type Codex struct{}

// Materialise panics — codex adapter is not implemented in v0.1.
func (Codex) Materialise(workspaceDir, skillName, skillSourceDir string) error {
	panic("adapter/codex: not impl in v0.1")
}

// Unmaterialise panics — codex adapter is not implemented in v0.1.
func (Codex) Unmaterialise(workspaceDir, skillName string) error {
	panic("adapter/codex: not impl in v0.1")
}
