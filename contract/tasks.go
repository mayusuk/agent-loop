package contract

// TaskList is plans/<plan-key>/tasks.yaml (INTEGRATION-SPEC §7), checked
// against tasks.schema.json. This package has no YAML parser: materialise
// (v0.2) reads the file and makes the checks that need the plan directory
// and the profile (work plan D12).
type TaskList struct {
	Version int    `json:"version" yaml:"version"`
	Plan    string `json:"plan" yaml:"plan"`
	Tasks   []Task `json:"tasks" yaml:"tasks"`
}

// Task is one task in a TaskList.
type Task struct {
	Key       string   `json:"key" yaml:"key"`
	Type      string   `json:"type" yaml:"type"`
	Title     string   `json:"title" yaml:"title"`
	Body      string   `json:"body" yaml:"body"`
	DependsOn []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
}
