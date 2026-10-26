package migration

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/Masterminds/semver/v3"
	"github.com/dominikbraun/graph"
	"go.yaml.in/yaml/v3"

	task "github.com/go-task/task/v3"
	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile"
	"github.com/go-task/task/v3/taskfile/ast"
)

// Report is the result of a migration preview. It contains one
// [FileReport] per Taskfile in the include graph, in read order (the root
// Taskfile first).
type Report struct {
	// TargetVersion is the schema version every Taskfile is previewed
	// against.
	TargetVersion string `json:"targetVersion" yaml:"target_version"`
	// Files contains the per-file findings, root Taskfile first.
	Files []*FileReport `json:"files" yaml:"files"`
	// StaticVars holds the root Taskfile variables resolved without executing
	// commands (dynamic sh: variables are left unresolved and omitted).
	StaticVars map[string]any `json:"staticVars,omitempty" yaml:"static_vars,omitempty"`
	// ValidationSkipped is set when the post-migration compiler validation
	// could not run because the Taskfile contains a known blocking issue
	// already reported as a manual action (for example an included Taskfile
	// declaring dotenv).
	ValidationSkipped bool `json:"validationSkipped,omitempty" yaml:"validation_skipped,omitempty"`
	// ValidationNote explains why validation was skipped when
	// ValidationSkipped is true.
	ValidationNote string `json:"validationNote,omitempty" yaml:"validation_note,omitempty"`
}

// HasChanges reports whether any file has at least one automatic change.
func (r *Report) HasChanges() bool {
	for _, f := range r.Files {
		if f.HasChanges() {
			return true
		}
	}
	return false
}

// HasWarnings reports whether any file produced a warning.
func (r *Report) HasWarnings() bool {
	for _, f := range r.Files {
		if len(f.Warnings) > 0 {
			return true
		}
	}
	return false
}

// HasManualActions reports whether any file has a non-migratable item.
func (r *Report) HasManualActions() bool {
	for _, f := range r.Files {
		if len(f.ManualActions) > 0 {
			return true
		}
	}
	return false
}

// File returns the report for the given Taskfile URI, or nil when absent.
func (r *Report) File(uri string) *FileReport {
	for _, f := range r.Files {
		if f.URI == uri {
			return f
		}
	}
	return nil
}

// Previewer builds read-only migration previews from a Taskfile include
// graph.
type Previewer struct {
	target     *semver.Version
	rules      []Rule
	readerOpts []taskfile.ReaderOption
}

// Option configures a [Previewer].
type Option func(*Previewer)

// WithTargetVersion sets the schema version to migrate to. It defaults to 3.
func WithTargetVersion(version *semver.Version) Option {
	return func(p *Previewer) {
		p.target = version
	}
}

// WithRule appends a custom migration rule. Custom rules run after the
// built-in rules on every Taskfile. Returning an error from a rule aborts the
// preview and discards every in-memory change (failure rollback).
func WithRule(rule Rule) Option {
	return func(p *Previewer) {
		p.rules = append(p.rules, rule)
	}
}

// WithReaderOptions forwards the given options to the underlying
// [taskfile.Reader]. This is useful for trusted hosts or custom certificates
// when previewing remote Taskfiles.
func WithReaderOptions(opts ...taskfile.ReaderOption) Option {
	return func(p *Previewer) {
		p.readerOpts = append(p.readerOpts, opts...)
	}
}

// NewPreviewer constructs a [Previewer].
func NewPreviewer(opts ...Option) *Previewer {
	p := &Previewer{target: ast.V3}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Preview reads the Taskfile graph rooted at node and previews a migration to
// the configured target version. It is strictly read-only: Taskfiles are not
// written back to disk and no task or command is executed (dynamic sh:
// variables are not evaluated).
func (p *Previewer) Preview(ctx context.Context, node taskfile.Node) (*Report, error) {
	// The reader keeps the parsed AST graph; documents holds the raw YAML
	// document nodes observed during reading. Neither is ever modified:
	// migration rules run on deep copies, and disk files are never written.
	documents := make(map[string]*yaml.Node)
	observer := func(uri string, _ []byte, doc *yaml.Node) {
		documents[uri] = doc
	}

	reader := taskfile.NewReader(
		append(p.readerOpts, taskfile.WithNodeObserver(observer))...,
	)
	graphIn, err := reader.Read(ctx, node)
	if err != nil {
		return nil, err
	}

	// Iterate in topological order so the root Taskfile is reported first.
	hashes, err := graph.TopologicalSort(graphIn.Graph)
	if err != nil {
		return nil, err
	}

	reports := make(map[string]*FileReport, len(hashes))
	files := make([]*FileReport, 0, len(hashes))
	for _, hash := range hashes {
		vertex, err := graphIn.Vertex(hash)
		if err != nil {
			return nil, err
		}

		doc, ok := documents[vertex.URI]
		if !ok {
			return nil, fmt.Errorf("task: migration: %q: raw document is unavailable", vertex.URI)
		}

		// migrateDocument works on deep copies; on error no partial report is
		// returned and the original graph/AST stay untouched.
		report, err := migrateDocument(vertex.URI, doc, vertex.Taskfile, p.target, p.rules)
		if err != nil {
			return nil, err
		}
		reports[vertex.URI] = report
		files = append(files, report)
	}

	// Cross-file findings need the edges between Taskfiles.
	if err := p.inspectEdges(graphIn, reports); err != nil {
		return nil, err
	}

	// Build a separate graph of the migrated copies, merge it and validate it
	// through the compiler. Failures here discard the preview, leaving the
	// original graph intact. staticVars are resolved on the fast path without
	// evaluating sh: variables, so no commands are executed.
	staticVars, validationSkipped, note, err := p.mergeAndValidate(node, graphIn, reports)
	if err != nil {
		return nil, err
	}

	return &Report{
		TargetVersion:     p.target.String(),
		Files:             files,
		StaticVars:        staticVars,
		ValidationSkipped: validationSkipped,
		ValidationNote:    note,
	}, nil
}

// inspectEdges adds findings that only make sense once the full include graph
// is known. All findings point at the include declaration in the parent
// Taskfile using its preserved source line.
func (p *Previewer) inspectEdges(g *ast.TaskfileGraph, reports map[string]*FileReport) error {
	adjacency, err := g.AdjacencyMap()
	if err != nil {
		return err
	}
	for parentURI, children := range adjacency {
		parentReport, ok := reports[parentURI]
		if !ok {
			continue
		}
		parentVertex, err := g.Vertex(parentURI)
		if err != nil {
			return err
		}
		parentMigrating := parentVertex.Taskfile.Version.LessThan(p.target)

		for childURI, edge := range children {
			childVertex, err := g.Vertex(childURI)
			if err != nil {
				return err
			}

			includes, _ := edge.Properties.Data.([]*ast.Include)
			for _, include := range includes {
				line, column := includeLine(parentReport, include.Namespace)

				// v3 rejects dotenv declarations in included Taskfiles.
				if len(childVertex.Taskfile.Dotenv) > 0 {
					parentReport.ManualActions = append(parentReport.ManualActions, ManualAction{
						Location: Location{Taskfile: parentURI, Line: line, Column: column},
						Code:     codeIncludedDotenv,
						Message: fmt.Sprintf(
							"included Taskfile %q declares dotenv, which version 3 does not allow in included Taskfiles",
							childURI),
						Hint: "move the dotenv declaration to the root Taskfile",
					})
				}

				// Nested includes only became fully supported on v3.
				if parentMigrating && childVertex.Taskfile.Includes.Len() > 0 {
					parentReport.Warnings = append(parentReport.Warnings, Warning{
						Location: Location{Taskfile: parentURI, Line: line, Column: column},
						Code:     codeNestedIncludes,
						Message: fmt.Sprintf(
							"included Taskfile %q itself contains includes, which are fully supported on version 3; review the resulting task namespaces",
							childURI),
					})
				}
			}
		}
	}
	return nil
}

// includeLine returns the source line and column of the include declaration
// with the given namespace, falling back to zero values when not found.
func includeLine(report *FileReport, namespace string) (int, int) {
	for _, include := range report.Includes {
		if include.Namespace == namespace {
			return include.Line, include.Column
		}
	}
	return 0, 0
}

// mergeAndValidate builds a graph from the migrated copies, merges it and
// validates static variable resolution through the compiler. It returns the
// resolved root variables. No commands are executed:
// [task.Compiler.FastGetVariables] leaves dynamic variables unresolved.
func (p *Previewer) mergeAndValidate(
	rootNode taskfile.Node,
	original *ast.TaskfileGraph,
	reports map[string]*FileReport,
) (map[string]any, bool, string, error) {
	migratedGraph := ast.NewTaskfileGraph()

	hashes, err := graph.TopologicalSort(original.Graph)
	if err != nil {
		return nil, false, "", err
	}
	for _, hash := range hashes {
		vertex, err := original.Vertex(hash)
		if err != nil {
			return nil, false, "", err
		}
		if err := migratedGraph.AddVertex(&ast.TaskfileVertex{
			URI:      vertex.URI,
			Taskfile: reports[vertex.URI].migrated,
		}); err != nil {
			return nil, false, "", err
		}
	}

	adjacency, err := original.AdjacencyMap()
	if err != nil {
		return nil, false, "", err
	}
	for src, children := range adjacency {
		for dst, edge := range children {
			includes, _ := edge.Properties.Data.([]*ast.Include)
			copied := make([]*ast.Include, 0, len(includes))
			for _, include := range includes {
				copied = append(copied, include.DeepCopy())
			}
			err := migratedGraph.AddEdge(src, dst,
				graph.EdgeData(copied),
				graph.EdgeWeight(len(copied)),
			)
			if err != nil {
				return nil, false, "", err
			}
		}
	}

	merged, err := migratedGraph.Merge()
	if err != nil {
		// Included dotenv is a documented non-migratable item: skip
		// validation instead of failing the whole preview.
		if errors.Is(err, ast.ErrIncludedTaskfilesCantHaveDotenvs) {
			return nil, true, "post-migration validation skipped: an included Taskfile declares dotenv, which is reported as a manual action", nil
		}
		return nil, false, "", fmt.Errorf("task: migration: migrated Taskfile is invalid: %w", err)
	}

	return validateWithCompiler(rootNode, merged)
}

// validateWithCompiler runs static variable resolution on the merged migrated
// Taskfile and returns the resolved root variables. The fast path is used so
// sh: variables are never evaluated and no command is executed; unresolved
// dynamic (sh:) variables are omitted from the result.
func validateWithCompiler(rootNode taskfile.Node, merged *ast.Taskfile) (map[string]any, bool, string, error) {
	rootURI := rootNode.Location()
	dir, entrypoint := filepath.Dir(rootURI), filepath.Base(rootURI)
	if taskfile.IsRemoteEntrypoint(rootURI) {
		dir, entrypoint = "", rootURI
	}

	compiler := &task.Compiler{
		Dir:            dir,
		Entrypoint:     entrypoint,
		UserWorkingDir: dir,
		TaskfileEnv:    merged.Env,
		TaskfileVars:   merged.Vars,
		Logger:         &logger.Logger{Stdout: io.Discard, Stderr: io.Discard},
	}

	rootVars, err := compiler.FastGetVariables(nil, nil)
	if err != nil {
		return nil, false, "", fmt.Errorf("task: migration: static variable resolution failed: %w", err)
	}
	for name, t := range merged.Tasks.All(nil) {
		if _, err := compiler.FastGetVariables(t, &task.Call{Task: name}); err != nil {
			return nil, false, "", fmt.Errorf("task: migration: static variable resolution failed for task %q: %w", name, err)
		}
	}

	result := make(map[string]any, rootVars.Len())
	for k, v := range rootVars.All() {
		if v.Sh != nil {
			continue // dynamic variables are intentionally not executed
		}
		result[k] = v.Value
	}
	return result, false, "", nil
}
