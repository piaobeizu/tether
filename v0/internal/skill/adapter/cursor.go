package adapter

// Cursor is a v0.1 stub. Spec §11.Z.4: v0.2 lossy adapter — concat
// SKILL.md into .cursor/rules/, drop hooks with a warning.
type Cursor struct{}

// Materialise panics — cursor adapter is not implemented in v0.1.
func (Cursor) Materialise(workspaceDir, skillName, skillSourceDir string) error {
	panic("adapter/cursor: not impl in v0.1")
}

// Unmaterialise panics — cursor adapter is not implemented in v0.1.
func (Cursor) Unmaterialise(workspaceDir, skillName string) error {
	panic("adapter/cursor: not impl in v0.1")
}
