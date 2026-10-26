// Package migration provides read-only previews of Taskfile schema version
// migrations.
//
// The preview works on in-memory copies of the parsed Taskfiles: it never
// writes Taskfiles back to disk and it never executes tasks or commands. In
// particular, dynamic (sh:) variables are left unresolved, so previewing a
// Taskfile cannot have any side effect on the host system.
//
// A preview reports three kinds of findings per file:
//
//   - [Change]s are field-level rewrites that can be applied automatically
//     (for example bumping version: '2' to version: '3').
//   - [Warning]s describe behaviour changes the user should be aware of even
//     though the YAML itself is still valid.
//   - [ManualAction]s are items that cannot be migrated automatically, such
//     as unknown extension fields or removed auto-loading conventions.
package migration

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/Masterminds/semver/v3"
	"go.yaml.in/yaml/v3"

	"github.com/go-task/task/v3/taskfile/ast"
)

// Actions recorded by a [Change].
const (
	// ActionSet replaces the value of an existing field.
	ActionSet = "set"
	// ActionAdd adds a field that did not exist before.
	ActionAdd = "add"
	// ActionRemove removes a field that is no longer supported.
	ActionRemove = "remove"
)

// Finding codes. They are stable and safe to match on by consumers.
const (
	codeExpansionsRemoved    = "EXPANSIONS_REMOVED"
	codeMethodDefaultChanged = "METHOD_DEFAULT_CHANGED"
	codeCLIVarsGlobal        = "CLI_VARS_GLOBAL"
	codeUnknownRootField     = "UNKNOWN_ROOT_FIELD"
	codeUnknownTaskField     = "UNKNOWN_TASK_FIELD"
	codeUnknownIncludeField  = "UNKNOWN_INCLUDE_FIELD"
	codeTaskvarsAutoload     = "TASKVARS_AUTOLOAD_REMOVED"
	codeOSTaskfileAutoload   = "OS_TASKFILE_AUTOINCLUDE_REMOVED"
	codeIncludedDotenv       = "INCLUDED_DOTENV_UNSUPPORTED"
	codeNestedIncludes       = "NESTED_INCLUDES_ENABLED"
)

// Location points at a field inside a Taskfile.
type Location struct {
	Taskfile string `json:"taskfile" yaml:"taskfile"`
	Line     int    `json:"line" yaml:"line"`
	Column   int    `json:"column" yaml:"column"`
}

// Change is an automatic rewrite of a single field.
type Change struct {
	Location `json:",inline" yaml:",inline"`
	// Path identifies the field using a dotted path from the Taskfile root,
	// e.g. "version", "expansions", "method".
	Path string `json:"path" yaml:"path"`
	// Action is one of ActionSet, ActionAdd or ActionRemove.
	Action string `json:"action" yaml:"action"`
	// OldValue is the value before the migration, when known.
	OldValue string `json:"oldValue,omitempty" yaml:"old_value,omitempty"`
	// NewValue is the value after the migration, when known.
	NewValue string `json:"newValue,omitempty" yaml:"new_value,omitempty"`
	// Reason explains why the change is required.
	Reason string `json:"reason" yaml:"reason"`
}

// Warning describes a behaviour change that does not require a YAML rewrite
// but may affect how the Taskfile runs on the target version.
type Warning struct {
	Location `json:",inline" yaml:",inline"`
	Code     string `json:"code" yaml:"code"`
	Message  string `json:"message" yaml:"message"`
}

// ManualAction describes something that cannot be migrated automatically and
// requires manual intervention.
type ManualAction struct {
	Location `json:",inline" yaml:",inline"`
	Code     string `json:"code" yaml:"code"`
	Message  string `json:"message" yaml:"message"`
	// Hint optionally describes how the issue can be resolved manually.
	Hint string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

// IncludeReference records an include declaration together with the source
// position it was declared at. Include source lines are preserved throughout
// the preview so that cross-file findings can point at the include that
// introduced them.
type IncludeReference struct {
	Namespace string `json:"namespace" yaml:"namespace"`
	Taskfile  string `json:"taskfile" yaml:"taskfile"`
	Line      int    `json:"line" yaml:"line"`
	Column    int    `json:"column" yaml:"column"`
}

// FileReport contains the preview findings for a single Taskfile.
type FileReport struct {
	URI           string `json:"uri" yaml:"uri"`
	FromVersion   string `json:"fromVersion" yaml:"from_version"`
	TargetVersion string `json:"targetVersion" yaml:"target_version"`
	// Includes lists every include declaration in source order with its
	// original line and column.
	Includes []IncludeReference `json:"includes,omitempty" yaml:"includes,omitempty"`
	// Changes are the automatic rewrites previewed for this Taskfile.
	Changes []Change `json:"changes" yaml:"changes"`
	// Warnings describe behaviour changes that do not rewrite the YAML.
	Warnings []Warning `json:"warnings" yaml:"warnings"`
	// ManualActions lists items that cannot be migrated automatically.
	ManualActions []ManualAction `json:"manualActions" yaml:"manual_actions"`

	// migrated is the in-memory migrated copy of the Taskfile. It is never
	// written back to disk.
	migrated *ast.Taskfile
	// doc is the migrated copy of the raw YAML document node.
	doc *yaml.Node
}

// Migrated returns the in-memory migrated copy of the Taskfile. The returned
// Taskfile is owned by the report and must not be written to disk by the
// caller; the preview itself never does so.
func (r *FileReport) Migrated() *ast.Taskfile {
	return r.migrated
}

// HasChanges reports whether the file contains at least one automatic change.
func (r *FileReport) HasChanges() bool { return len(r.Changes) > 0 }

// A Rule inspects and optionally rewrites the cloned document of a single
// Taskfile. Rules record findings on the [RuleContext]. If a rule returns an
// error, the migration is aborted and every in-memory change is discarded, so
// the original Taskfile and its parsed AST remain untouched.
type Rule func(*RuleContext) error

// RuleContext is passed to [Rule]s. Root is the cloned root mapping node of
// the Taskfile document; rules may modify it freely.
type RuleContext struct {
	URI    string
	Dir    string
	From   *semver.Version
	Target *semver.Version
	Root   *yaml.Node

	report *FileReport
}

// Change records an automatic rewrite.
func (c *RuleContext) Change(node *yaml.Node, path, action, oldValue, newValue, reason string) {
	c.report.Changes = append(c.report.Changes, Change{
		Location: locationOf(c.URI, node),
		Path:     path,
		Action:   action,
		OldValue: oldValue,
		NewValue: newValue,
		Reason:   reason,
	})
}

// Warn records a behaviour warning.
func (c *RuleContext) Warn(node *yaml.Node, code, message string) {
	c.report.Warnings = append(c.report.Warnings, Warning{
		Location: locationOf(c.URI, node),
		Code:     code,
		Message:  message,
	})
}

// Manual records an item that cannot be migrated automatically.
func (c *RuleContext) Manual(node *yaml.Node, code, message, hint string) {
	c.report.ManualActions = append(c.report.ManualActions, ManualAction{
		Location: locationOf(c.URI, node),
		Code:     code,
		Message:  message,
		Hint:     hint,
	})
}

// migrating reports whether the file is actually upgraded, as opposed to a
// no-op preview of a file already on the target schema version.
func (c *RuleContext) migrating() bool {
	return c.From != nil && c.From.LessThan(c.Target)
}

func locationOf(uri string, node *yaml.Node) Location {
	if node == nil {
		return Location{Taskfile: uri}
	}
	return Location{Taskfile: uri, Line: node.Line, Column: node.Column}
}

// Known schema keys. "expansions" is a v2-only key handled explicitly by
// ruleExpansions, but is listed as known here so that it is not reported as an
// extension field.
var (
	knownRootKeys = map[string]struct{}{
		"version":       {},
		"output":        {},
		"method":        {},
		"includes":      {},
		"set":           {},
		"shopt":         {},
		"vars":          {},
		"env":           {},
		"tasks":         {},
		"silent":        {},
		"dotenv":        {},
		"run":           {},
		"interval":      {},
		"use_gitignore": {},
		"expansions":    {}, // v2-only, removed by ruleExpansions
	}
	knownTaskKeys = map[string]struct{}{
		"cmds": {}, "cmd": {}, "deps": {}, "label": {}, "desc": {},
		"prompt": {}, "summary": {}, "requires": {}, "aliases": {},
		"sources": {}, "generates": {}, "status": {}, "preconditions": {},
		"dir": {}, "set": {}, "shopt": {}, "vars": {}, "env": {},
		"dotenv": {}, "silent": {}, "interactive": {}, "internal": {},
		"method": {}, "prefix": {}, "ignore_error": {}, "use_gitignore": {},
		"run": {}, "platforms": {}, "if": {}, "watch": {}, "failfast": {},
	}
	knownIncludeKeys = map[string]struct{}{
		"taskfile": {}, "dir": {}, "optional": {}, "internal": {},
		"flatten": {}, "aliases": {}, "excludes": {}, "vars": {}, "checksum": {},
	}
)

// defaultRules are applied in order to every Taskfile copy.
func defaultRules() []Rule {
	return []Rule{
		ruleExtensions,
		ruleVersion,
		ruleExpansions,
		ruleMethod,
		ruleSemanticWarnings,
		ruleLegacyFiles,
	}
}

// migrateDocument migrates a single Taskfile document to the target version.
// The incoming doc and tf are never modified: all rules run on deep copies. On
// success the returned [FileReport] owns the migrated copies; on error no
// partial migration is returned, which gives callers atomic rollback
// semantics.
func migrateDocument(
	uri string,
	doc *yaml.Node,
	tf *ast.Taskfile,
	target *semver.Version,
	extraRules []Rule,
) (*FileReport, error) {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("task: migration: %q: missing YAML document node", uri)
	}
	if tf == nil || tf.Version == nil {
		return nil, fmt.Errorf("task: migration: %q: missing schema version", uri)
	}
	if target == nil {
		return nil, fmt.Errorf("task: migration: %q: target schema version is required", uri)
	}
	from := tf.Version
	switch {
	case target.Major() != ast.V3.Major():
		return nil, fmt.Errorf("task: migration: %q: unsupported target schema version %s: only %d.x is supported", uri, target, ast.V3.Major())
	case from.Major() > target.Major():
		return nil, fmt.Errorf("task: migration: %q: downgrading schema version %s to %s is not supported", uri, from, target)
	case from.Major() < ast.V3.Major()-1:
		return nil, fmt.Errorf("task: migration: %q: schema version %s is too old to migrate automatically; manual upgrade required", uri, from)
	}

	report := &FileReport{
		URI:           uri,
		FromVersion:   from.String(),
		TargetVersion: target.String(),
		Includes:      collectIncludes(uri, doc),
	}
	clone := cloneNode(doc)
	ctx := &RuleContext{
		URI:    uri,
		Dir:    filepath.Dir(uri),
		From:   from,
		Target: target,
		Root:   clone.Content[0],
		report: report,
	}

	rules := append(defaultRules(), extraRules...)
	for _, rule := range rules {
		if err := rule(ctx); err != nil {
			// Discard the partially migrated copy entirely.
			return nil, err
		}
	}

	// Re-decode the rewritten document into a fresh AST. This is the migrated
	// in-memory copy used for graph merging and compiler validation; it is
	// never written back to disk.
	var migrated ast.Taskfile
	if err := clone.Content[0].Decode(&migrated); err != nil {
		return nil, fmt.Errorf("task: migration: %q: rewritten Taskfile is invalid: %w", uri, err)
	}
	migrated.Location = uri
	for task := range migrated.Tasks.Values(nil) {
		if task == nil {
			continue
		}
		if task.Location == nil {
			task.Location = &ast.Location{}
		}
		if task.Location.Taskfile == "" {
			task.Location.Taskfile = uri
		}
	}
	report.migrated = &migrated
	report.doc = clone
	return report, nil
}

// ruleVersion bumps the schema version to the target major version.
func ruleVersion(c *RuleContext) error {
	if !c.migrating() {
		return nil
	}
	key, val := findPair(c.Root, "version")
	if key == nil {
		return nil
	}
	old := val.Value
	val.Value = strconv.FormatUint(uint64(c.Target.Major()), 10)
	c.Change(key, "version", ActionSet, old, val.Value,
		fmt.Sprintf("schema version %s is no longer supported; upgrading to version %s", c.From, c.Target))
	return nil
}

// ruleExpansions removes the v2-only "expansions" key, which has no effect on
// v3 because variable expansion is handled differently.
func ruleExpansions(c *RuleContext) error {
	key, val := findPair(c.Root, "expansions")
	if key == nil {
		return nil
	}
	removePair(c.Root, key)
	old := val.Value
	c.Change(key, "expansions", ActionRemove, old, "",
		`"expansions" has no effect on schema version 3 and must be removed`)
	if old != "" && old != strconv.Itoa(defaultV2Expansions) {
		c.Warn(key, codeExpansionsRemoved,
			fmt.Sprintf(`"expansions: %s" changed how many times variables were expanded on version 2; version 3 expands variables until stable, which may change the resolved values`, old))
	}
	return nil
}

const defaultV2Expansions = 2

// ruleMethod preserves the v2 default up-to-date method: the global default
// changed from "timestamp" to "checksum" on v3.
func ruleMethod(c *RuleContext) error {
	if !c.migrating() {
		return nil
	}
	if key, _ := findPair(c.Root, "method"); key != nil {
		return nil
	}
	key := scalarNode("method", c.Root.Line)
	val := scalarNode("timestamp", c.Root.Line)
	c.Root.Content = append(c.Root.Content, key, val)
	c.Change(key, "method", ActionAdd, "", "timestamp",
		`the default up-to-date method changed from "timestamp" to "checksum" on version 3; setting "method: timestamp" preserves the previous behaviour`)
	c.Warn(key, codeMethodDefaultChanged,
		`without an explicit "method", version 3 defaults to the "checksum" method instead of "timestamp"; the preview adds "method: timestamp" to preserve the version 2 behaviour`)
	return nil
}

// ruleSemanticWarnings records behaviour changes that cannot be expressed as
// YAML rewrites.
func ruleSemanticWarnings(c *RuleContext) error {
	if !c.migrating() {
		return nil
	}
	if key, _ := findPair(c.Root, "version"); key != nil {
		c.Warn(key, codeCLIVarsGlobal,
			`on version 3, command line variables are always global variables and variable precedence changed; review tasks that rely on call-scoped variables`)
	}
	return nil
}

// ruleLegacyFiles reports files that were auto-loaded on v2 but are no longer
// auto-loaded on v3. Only read-only filesystem checks (os.Stat, Glob) are
// performed; nothing is executed or modified.
func ruleLegacyFiles(c *RuleContext) error {
	if !c.migrating() {
		return nil
	}
	anchor, _ := findPair(c.Root, "version")
	if anchor == nil {
		anchor = c.Root
	}

	// Taskvars.yml (and Taskvars_{GOOS}.yml) is no longer auto-included.
	for _, pattern := range []string{"Taskvars.yml", "Taskvars_*.yml"} {
		matches, err := filepath.Glob(filepath.Join(c.Dir, pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			c.Manual(anchor, codeTaskvarsAutoload,
				fmt.Sprintf("%s is no longer automatically included on version 3", filepath.Base(match)),
				`move its contents into "vars:" or include it explicitly from the Taskfile`)
		}
	}

	// Taskfile_{GOOS}.yml is no longer auto-merged.
	osTaskfile := filepath.Join(c.Dir, fmt.Sprintf("Taskfile_%s.yml", runtime.GOOS))
	if _, err := os.Stat(osTaskfile); err == nil {
		c.Manual(anchor, codeOSTaskfileAutoload,
			fmt.Sprintf("Taskfile_%s.yml is no longer automatically merged on version 3", runtime.GOOS),
			`include it explicitly, for example with an interpolated "includes:" entry`)
	}
	return nil
}

// ruleExtensions reports mapping keys the target schema does not know about.
// Those cannot be migrated automatically, because their intended semantics are
// unknown (typo, future-version feature, or third-party extension).
func ruleExtensions(c *RuleContext) error {
	visited := make(map[*yaml.Node]bool)

	// Unknown root keys.
	for i := 0; i+1 < len(c.Root.Content); i += 2 {
		keyNode := c.Root.Content[i]
		if _, ok := knownRootKeys[keyNode.Value]; !ok {
			c.Manual(keyNode, codeUnknownRootField,
				fmt.Sprintf("unknown root field %q is not part of the version %d schema", keyNode.Value, c.Target.Major()),
				`remove the field or check its spelling; unknown fields are ignored and cannot be migrated automatically`)
		}
	}

	// Unknown keys inside tasks. Anchor targets are inspected once even when
	// referenced through aliases or YAML merge keys.
	if _, tasksNode := findPair(c.Root, "tasks"); tasksNode != nil {
		tasksNode = resolve(tasksNode)
		if tasksNode.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(tasksNode.Content); i += 2 {
				nameNode := tasksNode.Content[i]
				taskNode := resolve(tasksNode.Content[i+1])
				if taskNode.Kind != yaml.MappingNode {
					continue // scalar/sequence shortcut forms have no fields
				}
				c.scanTaskMapping(nameNode.Value, taskNode, visited)
			}
		}
	}

	// Unknown keys inside include mappings.
	if _, includesNode := findPair(c.Root, "includes"); includesNode != nil {
		includesNode = resolve(includesNode)
		if includesNode.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(includesNode.Content); i += 2 {
				nsNode := includesNode.Content[i]
				includeNode := resolve(includesNode.Content[i+1])
				if includeNode.Kind != yaml.MappingNode {
					continue // string include form has no fields
				}
				for j := 0; j+1 < len(includeNode.Content); j += 2 {
					fieldNode := includeNode.Content[j]
					if _, ok := knownIncludeKeys[fieldNode.Value]; !ok {
						c.Manual(fieldNode, codeUnknownIncludeField,
							fmt.Sprintf("unknown field %q in include %q", fieldNode.Value, nsNode.Value),
							`remove the field or check its spelling`)
					}
				}
			}
		}
	}
	return nil
}

// scanTaskMapping reports unknown keys in a task mapping. It follows YAML
// merge keys ("<<") into their anchor targets and visits every anchor target
// at most once.
func (c *RuleContext) scanTaskMapping(taskName string, node *yaml.Node, visited map[*yaml.Node]bool) {
	if node == nil || node.Kind != yaml.MappingNode || visited[node] {
		return
	}
	visited[node] = true

	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]

		// A merge key contributes the keys of its anchor target to this task.
		if keyNode.Value == "<<" {
			merged := resolve(valueNode)
			if merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					c.scanTaskMapping(taskName, resolve(item), visited)
				}
			} else {
				c.scanTaskMapping(taskName, merged, visited)
			}
			continue
		}

		if _, ok := knownTaskKeys[keyNode.Value]; !ok {
			c.Manual(keyNode, codeUnknownTaskField,
				fmt.Sprintf("unknown field %q in task %q", keyNode.Value, taskName),
				`remove the field or check its spelling; unknown fields are ignored and cannot be migrated automatically`)
		}
	}
}

// findPair returns the key and value nodes for the given key of a mapping
// node, or nil, nil when absent.
func findPair(mapping *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// removePair removes the key/value pair identified by the key node from the
// mapping node.
func removePair(mapping *yaml.Node, key *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i] == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// resolve follows alias nodes to their anchor targets.
func resolve(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}

func scalarNode(value string, line int) *yaml.Node {
	return &yaml.Node{
		Kind:   yaml.ScalarNode,
		Tag:    "!!str",
		Value:  value,
		Line:   line,
		Column: 1,
	}
}

// collectIncludes returns every include declaration in source order with the
// line and column of its namespace key. Aliases are followed so includes
// declared through anchors are not missed.
func collectIncludes(uri string, doc *yaml.Node) []IncludeReference {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil
	}
	_, includesNode := findPair(doc.Content[0], "includes")
	includesNode = resolve(includesNode)
	if includesNode == nil || includesNode.Kind != yaml.MappingNode {
		return nil
	}
	includes := make([]IncludeReference, 0, len(includesNode.Content)/2)
	for i := 0; i+1 < len(includesNode.Content); i += 2 {
		keyNode := includesNode.Content[i]
		valueNode := resolve(includesNode.Content[i+1])
		ref := IncludeReference{
			Namespace: keyNode.Value,
			Line:      keyNode.Line,
			Column:    keyNode.Column,
		}
		switch valueNode.Kind {
		case yaml.ScalarNode:
			ref.Taskfile = valueNode.Value
		case yaml.MappingNode:
			if _, tf := findPair(valueNode, "taskfile"); tf != nil {
				ref.Taskfile = tf.Value
			}
		}
		includes = append(includes, ref)
	}
	return includes
}

// cloneNode returns a deep copy of a YAML node tree. Alias nodes are rewired
// to point at the clones of their anchor targets, and cycles terminate via
// the shared clone map.
func cloneNode(node *yaml.Node) *yaml.Node {
	clones := make(map[*yaml.Node]*yaml.Node)
	var clone func(*yaml.Node) *yaml.Node
	clone = func(n *yaml.Node) *yaml.Node {
		if n == nil {
			return nil
		}
		if c, ok := clones[n]; ok {
			return c
		}
		c := &yaml.Node{
			Kind:        n.Kind,
			Style:       n.Style,
			Tag:         n.Tag,
			Value:       n.Value,
			Anchor:      n.Anchor,
			HeadComment: n.HeadComment,
			LineComment: n.LineComment,
			FootComment: n.FootComment,
			Line:        n.Line,
			Column:      n.Column,
		}
		clones[n] = c
		for _, child := range n.Content {
			c.Content = append(c.Content, clone(child))
		}
		if n.Alias != nil {
			c.Alias = clone(n.Alias)
		}
		return c
	}
	return clone(node)
}
