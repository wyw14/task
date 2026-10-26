// Package varreport builds static variable reference reports for Taskfiles.
//
// The report is produced entirely from the parsed Taskfile graph: it never
// executes tasks, never evaluates "sh:" variables and never downloads or
// clones remote includes. It connects the four stages that govern variable
// behavior: the reader's include graph, the ast.Vars containers, the
// templater's template syntax and the precedence implemented by the
// compiler (mirrored statically here).
package varreport

// ScopeKind identifies where a variable definition comes from. The values
// mirror the resolution order implemented by Compiler.getVariables:
// environment < special < taskfile.env < taskfile.vars < include.vars <
// included taskfile.vars < call.vars < task.vars.
type ScopeKind string

const (
	// ScopeEnvironment is a variable inherited from the OS environment
	// (env.GetEnviron in the compiler).
	ScopeEnvironment ScopeKind = "environment"
	// ScopeSpecial is a variable Task provides itself (TASK, TASK_DIR,
	// CLI_ARGS, ITEM and friends).
	ScopeSpecial ScopeKind = "special"
	// ScopeTaskfileEnv is a variable declared in a Taskfile "env:" block.
	// Such variables are exported to the shell environment of tasks.
	ScopeTaskfileEnv ScopeKind = "taskfile.env"
	// ScopeTaskfileVars is a variable declared in a Taskfile "vars:" block.
	ScopeTaskfileVars ScopeKind = "taskfile.vars"
	// ScopeIncludeVars is a variable declared in an include's "vars:" block.
	ScopeIncludeVars ScopeKind = "include.vars"
	// ScopeTaskEnv is a variable declared in a task "env:" block. It is
	// exported to the task's shell environment, but is not available as a
	// template variable.
	ScopeTaskEnv ScopeKind = "task.env"
	// ScopeTaskVars is a variable declared in a task "vars:" block.
	ScopeTaskVars ScopeKind = "task.vars"
	// ScopeCallVars is a variable passed inline through a task call or
	// dependency ("cmds:/deps: ... vars:").
	ScopeCallVars ScopeKind = "call.vars"
)

// Definition describes one place where a variable is defined.
type Definition struct {
	// Scope is the kind of definition (its precedence bucket).
	Scope ScopeKind `json:"scope"`
	// URI is the absolute location of the Taskfile containing the
	// definition. Empty for environment and special definitions.
	URI string `json:"uri,omitempty"`
	// Namespace is set for include.vars definitions.
	Namespace string `json:"namespace,omitempty"`
	// Task is the local (non-namespaced) task name for task/call scopes.
	Task string `json:"task,omitempty"`
	// Dynamic is true when the definition uses "sh:".
	Dynamic bool `json:"dynamic"`
	// Sh is the statically recorded shell snippet; it is never executed.
	Sh string `json:"sh,omitempty"`
	// Ref is true when the definition uses "ref:".
	Ref string `json:"ref,omitempty"`
	// Secret marks "secret: true" definitions.
	Secret bool `json:"secret,omitempty"`
	// Exported is true for definitions exported to the shell environment
	// (taskfile.env and task.env blocks).
	Exported bool `json:"exported,omitempty"`
}

// Usage records one place that references a variable.
type Usage struct {
	// Task is the full, namespaced task name (e.g. "lib:sub:build"). It is
	// empty for file-level usages such as include path templates.
	Task string `json:"task,omitempty"`
	// URI is the Taskfile in which the reference was found.
	URI string `json:"uri"`
	// Field describes where the reference lives, e.g. "cmds", "dir",
	// "vars:OTHER" or "include:lib".
	Field string `json:"field"`
}

// Variable is one row of the report.
type Variable struct {
	// Name is the variable name.
	Name string `json:"name"`
	// Definitions lists every static definition, in discovery order.
	Definitions []*Definition `json:"definitions"`
	// Effective is the definition predicted to win template resolution
	// following Compiler.getVariables precedence. It is nil when the name
	// is only exported to the shell (task-level env block) or cannot be
	// resolved statically.
	Effective *Definition `json:"effective,omitempty"`
	// UsedBy lists the tasks referencing the variable, directly or
	// transitively through other variables.
	UsedBy []*Usage `json:"usedBy"`
	// ReferencedByVars lists names of other variables whose definition
	// expression references this one.
	ReferencedByVars []string `json:"referencedByVars,omitempty"`
	// References lists the variable names used by this variable's own
	// definition expression.
	References []string `json:"references,omitempty"`
	// Dynamic is true when at least one definition uses "sh:".
	Dynamic bool `json:"dynamic"`
	// DynamicVia, when non-empty, gives the static reference chain through
	// which this (static) variable depends on a shell-evaluated one, e.g.
	// ["GREETING", "NOW"] means this variable uses GREETING which uses the
	// sh-variable NOW.
	DynamicVia []string `json:"dynamicVia,omitempty"`
	// EnvValue is the current OS-environment value when the name exists in
	// the environment. Values of secret variables are masked.
	EnvValue string `json:"envValue,omitempty"`
	// EnvOverridden is true when an OS-environment value exists but a
	// Taskfile definition wins template resolution.
	EnvOverridden bool `json:"envOverridden,omitempty"`
	// Unused is true when no task, variable definition or shell export uses
	// the name.
	Unused bool `json:"unused"`
	// Undefined is true when the name is referenced but never defined
	// (neither by a Taskfile, special variable nor the environment).
	Undefined bool `json:"undefined"`
	// Cycles lists the full reference cycles this variable participates
	// in. Each cycle repeats the starting name at the end, e.g.
	// ["A","B","C","A"].
	Cycles [][]string `json:"cycles,omitempty"`
	// CyclePaths is the human-readable form of Cycles with scope and
	// Taskfile location for every node.
	CyclePaths []string `json:"cyclePaths,omitempty"`
}

// RemoteInclude describes a remote include that was not analyzed.
type RemoteInclude struct {
	Source    string `json:"source"`
	Namespace string `json:"namespace,omitempty"`
	URI       string `json:"uri"`
}

// ParseError records a template that could not be parsed statically.
type ParseError struct {
	URI   string `json:"uri"`
	Field string `json:"field"`
	Err   string `json:"err"`
}

// Report is the full static variable reference report.
type Report struct {
	// RootURI is the root Taskfile location.
	RootURI string `json:"rootUri"`
	// Files lists every local Taskfile vertex that was analyzed.
	Files []string `json:"files"`
	// Variables holds the report rows sorted by name.
	Variables []*Variable `json:"variables"`
	// SkippedRemote lists remote includes that were not downloaded.
	SkippedRemote []*RemoteInclude `json:"skippedRemote,omitempty"`
	// ParseErrors holds non-fatal template parse errors.
	ParseErrors []*ParseError `json:"parseErrors,omitempty"`
	// Resolved holds the static (no-shell) resolution preview produced by
	// Compiler.FastGetVariables for variables visible at Taskfile scope.
	Resolved map[string]string `json:"resolved,omitempty"`
}
