package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/go-task/task/v3/internal/env"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/varreport"
	"github.com/go-task/task/v3/taskfile"
)

// RunVariableReport produces a static variable reference report for the
// Taskfile at dir/entrypoint and writes it to w. It never executes tasks or
// "sh:" variables, and local static reading never downloads or clones
// includes: remote includes are listed as skipped.
func RunVariableReport(ctx context.Context, dir, entrypoint string, w io.Writer, asJSON bool) error {
	report, err := GenerateVariableReport(ctx, dir, entrypoint)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	report.RenderText(w)
	return nil
}

// GenerateVariableReport reads the Taskfile graph in static mode and analyzes
// every variable definition and reference.
func GenerateVariableReport(ctx context.Context, dir, entrypoint string) (*varreport.Report, error) {
	node, err := taskfile.NewRootNode(entrypoint, dir, false, 0)
	if err != nil {
		return nil, err
	}
	// Static analysis must never download or clone a remote Taskfile.
	if _, isRemote := node.(taskfile.RemoteNode); isRemote {
		return nil, fmt.Errorf(
			"task: variable report cannot analyze remote Taskfiles (downloads are disabled): %s",
			node.Location(),
		)
	}

	reader := taskfile.NewReader(taskfile.WithStaticAnalysis(true))
	graph, err := reader.Read(ctx, node)
	if err != nil {
		return nil, err
	}

	skipped := make([]varreport.RemoteInclude, 0)
	for _, s := range reader.SkippedRemoteIncludes() {
		skipped = append(skipped, varreport.RemoteInclude{
			Source:    s.Source,
			Namespace: s.Namespace,
			URI:       s.URI,
		})
	}

	environ := map[string]string{}
	for k, v := range env.GetEnviron().All() {
		if s, ok := v.Value.(string); ok {
			environ[k] = s
		}
	}

	// Analyze the unmerged graph so that each definition keeps its origin.
	report, err := varreport.Analyze(
		graph,
		varreport.WithEnvironment(environ),
		varreport.WithRemoteIncludes(skipped),
	)
	if err != nil {
		return nil, err
	}

	// Connect the compiler's static path: graph.Merge applies the same
	// include precedence a run uses, and FastGetVariables renders templates
	// with evaluateShVars=false. No shell command is ever executed and no
	// task is run; unresolved dynamic variables render as empty values.
	merged, err := graph.Merge()
	if err != nil {
		return nil, err
	}
	wd, _ := os.Getwd()
	compiler := &Compiler{
		Dir:            node.Dir(),
		Entrypoint:     node.Location(),
		UserWorkingDir: wd,
		TaskfileEnv:    merged.Env,
		TaskfileVars:   merged.Vars,
		Logger:         &logger.Logger{Stdout: io.Discard, Stderr: io.Discard},
	}
	resolved, err := compiler.FastGetVariables(nil, nil)
	if err != nil {
		return nil, err
	}

	report.Resolved = map[string]string{}
	for _, row := range report.Variables {
		v, ok := resolved.Get(row.Name)
		if !ok {
			continue
		}
		if v.Sh != nil && *v.Sh != "" {
			report.Resolved[row.Name] = "<dynamic: sh not evaluated>"
			continue
		}
		switch val := v.Value.(type) {
		case nil:
			// Unresolved or untyped: omit from the static preview.
		case string:
			report.Resolved[row.Name] = val
		default:
			report.Resolved[row.Name] = fmt.Sprint(val)
		}
	}

	return report, nil
}
