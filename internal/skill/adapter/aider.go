package adapter

// Aider is a v0.1 stub. Spec §11.Z.4: v0.2 lossy adapter — concat
// SKILL.md content into CONVENTIONS.md, drop hooks with a warning.
type Aider struct{}

// Materialise panics — aider adapter is not implemented in v0.1.
func (Aider) Materialise(workspaceDir, skillName, skillSourceDir string) error {
	panic("adapter/aider: not impl in v0.1")
}

// Unmaterialise panics — aider adapter is not implemented in v0.1.
func (Aider) Unmaterialise(workspaceDir, skillName string) error {
	panic("adapter/aider: not impl in v0.1")
}
