package varreport

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dominikbraun/graph"

	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/taskfile/ast"
)

// specialVars lists variables provided by Task itself (see
// Compiler.getSpecialVars and the cmd/task CLI variables).
var specialVars = []string{
	"TASK_EXE", "ROOT_TASKFILE", "ROOT_DIR", "USER_WORKING_DIR",
	"TASK_VERSION", "PATH_LIST_SEPARATOR", "FILE_PATH_SEPARATOR",
	"TASK", "TASK_DIR", "TASKFILE", "TASKFILE_DIR", "ALIAS",
	"CLI_ARGS", "CLI_ARGS_LIST", "CLI_FORCE", "CLI_SILENT",
	"CLI_VERBOSE", "CLI_OFFLINE", "CLI_ASSUME_YES",
	"MATCH", "ITEM", "KEY",
}

// Option configures Analyze.
type Option func(*analyzer)

// WithEnvironment provides the OS environment used to detect environment
// overrides. Analyze itself never reads the environment.
func WithEnvironment(env map[string]string) Option {
	return func(a *analyzer) { a.env = env }
}

// WithRemoteIncludes provides the remote includes the static reader skipped.
func WithRemoteIncludes(includes []RemoteInclude) Option {
	return func(a *analyzer) { a.remoteIncludes = includes }
}

// Analyze builds a Report from a parsed (but unmerged) Taskfile graph.
//
// It performs no I/O beyond the graph that was already read: it does not
// execute tasks or shell commands, does not evaluate dynamic "sh:"
// variables and does not download includes.
func Analyze(g *ast.TaskfileGraph, opts ...Option) (*Report, error) {
	if g == nil {
		return nil, fmt.Errorf("varreport: nil taskfile graph")
	}

	adj, err := g.AdjacencyMap()
	if err != nil {
		return nil, err
	}
	rootURI, err := rootVertex(g.Graph)
	if err != nil {
		return nil, err
	}

	a := &analyzer{
		graph:       g,
		adj:         adj,
		rootURI:     rootURI,
		env:         map[string]string{},
		rows:        map[string]*Variable{},
		scopeDefs:   map[string]map[string]bool{},
		scopeMeta:   map[string]scopeInfo{},
		scopeEdges:  map[string]map[string][]string{},
		edges:       map[string][]string{},
		directUsage: map[string][]*Usage{},
		prefixes:    map[string]map[string]bool{},
	}
	for _, opt := range opts {
		opt(a)
	}

	// Compute the namespace prefix(es) of every vertex by following include
	// edges from the root. Flattened includes contribute no prefix.
	a.collectPrefixes()

	uris := a.vertexURIs()

	// Pass 1: register every definition and scope membership.
	for _, uri := range uris {
		if tf := a.taskfile(uri); tf != nil {
			a.collectDefinitions(uri, tf)
		}
	}

	// Pass 2: extract reference edges from every definition.
	for _, uri := range uris {
		if tf := a.taskfile(uri); tf != nil {
			a.collectDefinitionRefs(uri, tf)
		}
	}

	// Pass 3: task field and include-path references.
	for _, uri := range uris {
		if tf := a.taskfile(uri); tf != nil {
			a.collectUsages(uri, tf)
		}
	}

	a.finalize()
	return a.buildReport(uris), nil
}

type analyzer struct {
	graph *ast.TaskfileGraph
	adj   map[string]map[string]graph.Edge[string]

	rootURI        string
	env            map[string]string
	remoteIncludes []RemoteInclude

	rows map[string]*Variable

	// scopeDefs: scope key -> set of defined variable names.
	scopeDefs map[string]map[string]bool
	// scopeMeta: scope key -> human scope information for cycle paths.
	scopeMeta map[string]scopeInfo
	// scopeEdges: scope key -> from -> referenced names defined in the scope.
	scopeEdges map[string]map[string][]string

	// edges: variable name -> names referenced from its definitions, across
	// all scopes (global static dependency graph).
	edges map[string][]string

	// directUsage: variable name -> direct task/file usages.
	directUsage map[string][]*Usage

	// prefixes: vertex URI -> set of accumulated namespace prefixes.
	prefixes map[string]map[string]bool

	// parseErrors holds non-fatal template parse errors.
	parseErrors []*ParseError
}

type scopeInfo struct {
	uri  string
	kind string // "file" or "task"
	task string
}

func fileScope(uri string) string { return "file:" + uri }

func taskScope(uri, task string) string { return "task:" + uri + "\x00" + task }

func (a *analyzer) taskfile(uri string) *ast.Taskfile {
	v, err := a.graph.Vertex(uri)
	if err != nil || v == nil {
		return nil
	}
	return v.Taskfile
}

// collectPrefixes walks the include graph from the root and records the
// namespace prefix accumulated along every path to each vertex.
func (a *analyzer) collectPrefixes() {
	type frame struct{ uri, prefix string }
	visited := map[string]map[string]bool{}
	queue := []frame{{a.rootURI, ""}}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if visited[f.uri] == nil {
			visited[f.uri] = map[string]bool{}
		}
		if visited[f.uri][f.prefix] {
			continue
		}
		visited[f.uri][f.prefix] = true

		if a.prefixes[f.uri] == nil {
			a.prefixes[f.uri] = map[string]bool{}
		}
		a.prefixes[f.uri][f.prefix] = true

		children := make([]string, 0)
		for child := range a.adj[f.uri] {
			children = append(children, child)
		}
		sort.Strings(children)
		for _, child := range children {
			edge := a.adj[f.uri][child]
			includes, ok := edge.Properties.Data.([]*ast.Include)
			if !ok {
				queue = append(queue, frame{child, f.prefix})
				continue
			}
			pieces := map[string]bool{}
			for _, inc := range includes {
				piece := ""
				if inc != nil && !inc.Flatten {
					piece = inc.Namespace + ":"
				}
				pieces[piece] = true
			}
			pieceList := mapKeys(pieces)
			sort.Strings(pieceList)
			for _, piece := range pieceList {
				queue = append(queue, frame{child, f.prefix + piece})
			}
		}
	}
}

func (a *analyzer) vertexURIs() []string {
	uris := make([]string, 0, len(a.adj))
	for uri := range a.adj {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

func (a *analyzer) fullTaskNames(uri, localName string) []string {
	prefixes := mapKeys(a.prefixes[uri])
	sort.Strings(prefixes)
	names := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		names = append(names, p+localName)
	}
	if len(names) == 0 {
		return []string{localName}
	}
	return names
}

func (a *analyzer) row(name string) *Variable {
	r, ok := a.rows[name]
	if !ok {
		r = &Variable{Name: name}
		a.rows[name] = r
	}
	return r
}

// addDefinition registers a variable definition in the given scope.
func (a *analyzer) addDefinition(name string, d *Definition, scope string) {
	r := a.row(name)
	r.Definitions = append(r.Definitions, d)
	if d.Dynamic {
		r.Dynamic = true
	}
	// Taskfile-level env blocks are exported to every task shell.
	if d.Exported && d.Scope == ScopeTaskfileEnv {
		a.addUsage(name, &Usage{URI: d.URI, Field: "env:exported"})
	}
	if a.scopeDefs[scope] == nil {
		a.scopeDefs[scope] = map[string]bool{}
	}
	a.scopeDefs[scope][name] = true
}

func (a *analyzer) addUsage(name string, u *Usage) {
	// A referenced name must have a row even when it has no definition, so
	// undefined references and special variables are reported too.
	a.row(name)
	for _, existing := range a.directUsage[name] {
		if existing.Task == u.Task && existing.URI == u.URI && existing.Field == u.Field {
			return
		}
	}
	a.directUsage[name] = append(a.directUsage[name], u)
}

// collectDefinitions registers all static definitions found in one vertex.
func (a *analyzer) collectDefinitions(uri string, tf *ast.Taskfile) {
	fk := fileScope(uri)
	a.scopeMeta[fk] = scopeInfo{uri: uri, kind: "file"}

	for k, v := range tf.Env.All() {
		a.addDefinition(k, defFromVar(uri, "", ScopeTaskfileEnv, v), fk)
	}
	for k, v := range tf.Vars.All() {
		a.addDefinition(k, defFromVar(uri, "", ScopeTaskfileVars, v), fk)
	}
	for namespace, inc := range tf.Includes.All() {
		if inc == nil || inc.Vars == nil {
			continue
		}
		for k, v := range inc.Vars.All() {
			d := defFromVar(uri, "", ScopeIncludeVars, v)
			d.Namespace = namespace
			a.addDefinition(k, d, fk)
		}
	}

	for name, t := range tf.Tasks.All(nil) {
		if t == nil {
			continue
		}
		tk := taskScope(uri, name)
		a.scopeMeta[tk] = scopeInfo{uri: uri, kind: "task", task: name}

		if t.Env != nil {
			for k, v := range t.Env.All() {
				a.addDefinition(k, defFromVar(uri, name, ScopeTaskEnv, v), tk)
			}
		}
		if t.Vars != nil {
			for k, v := range t.Vars.All() {
				a.addDefinition(k, defFromVar(uri, name, ScopeTaskVars, v), tk)
			}
		}
		for _, cmd := range t.Cmds {
			if cmd == nil || cmd.Vars == nil {
				continue
			}
			for k, v := range cmd.Vars.All() {
				a.addDefinition(k, defFromVar(uri, name, ScopeCallVars, v), tk)
			}
		}
		for _, dep := range t.Deps {
			if dep == nil || dep.Vars == nil {
				continue
			}
			for k, v := range dep.Vars.All() {
				a.addDefinition(k, defFromVar(uri, name, ScopeCallVars, v), tk)
			}
		}
	}
}

func defFromVar(uri, task string, scope ScopeKind, v ast.Var) *Definition {
	d := &Definition{
		Scope:    scope,
		URI:      uri,
		Task:     task,
		Secret:   v.Secret,
		Ref:      v.Ref,
		Exported: scope == ScopeTaskfileEnv || scope == ScopeTaskEnv,
	}
	if v.Sh != nil {
		d.Sh = *v.Sh
		d.Dynamic = *v.Sh != ""
	}
	return d
}

// collectDefinitionRefs extracts variable references from every definition
// expression and builds the global and per-scope dependency edges.
func (a *analyzer) collectDefinitionRefs(uri string, tf *ast.Taskfile) {
	fk := fileScope(uri)

	for k, v := range tf.Env.All() {
		a.addVarEdges(k, refsInVar(v), fk)
	}
	for k, v := range tf.Vars.All() {
		a.addVarEdges(k, refsInVar(v), fk)
	}
	for _, inc := range tf.Includes.All() {
		if inc == nil || inc.Vars == nil {
			continue
		}
		for k, v := range inc.Vars.All() {
			a.addVarEdges(k, refsInVar(v), fk)
		}
	}

	for name, t := range tf.Tasks.All(nil) {
		if t == nil {
			continue
		}
		tk := taskScope(uri, name)

		if t.Env != nil {
			for k, v := range t.Env.All() {
				refs := refsInVar(v)
				// Task env values are rendered with the task's template
				// variables but do not themselves define a template variable,
				// so they only contribute global edges (no scope cycles).
				a.addVarEdges(k, refs, "")
				for _, ref := range refs {
					a.addTaskUsage(uri, t, ref, "task.env:"+k)
				}
			}
		}
		if t.Vars != nil {
			for k, v := range t.Vars.All() {
				a.addVarEdges(k, refsInVar(v), tk)
			}
		}
		for _, cmd := range t.Cmds {
			if cmd == nil || cmd.Vars == nil {
				continue
			}
			for k, v := range cmd.Vars.All() {
				a.addVarEdges(k, refsInVar(v), tk)
			}
		}
		for _, dep := range t.Deps {
			if dep == nil || dep.Vars == nil {
				continue
			}
			for k, v := range dep.Vars.All() {
				a.addVarEdges(k, refsInVar(v), tk)
			}
		}
	}
}

// addVarEdges records that variable "from" references the given names.
// Definition-to-definition edges inside the same scope are used for cycle
// detection; all edges feed the global dependency graph. An empty scope
// skips cycle tracking.
func (a *analyzer) addVarEdges(from string, refs []string, scope string) {
	if len(refs) == 0 {
		return
	}
	a.edges[from] = appendUnique(a.edges[from], refs...)
	if scope != "" && a.scopeDefs[scope] != nil {
		if a.scopeEdges[scope] == nil {
			a.scopeEdges[scope] = map[string][]string{}
		}
		var inScope []string
		for _, ref := range refs {
			if a.scopeDefs[scope][ref] {
				inScope = append(inScope, ref)
			}
		}
		a.scopeEdges[scope][from] = appendUnique(a.scopeEdges[scope][from], inScope...)
	}
}

// addTaskUsage records a usage attributed to every full task name under
// which the task is reachable through nested includes.
func (a *analyzer) addTaskUsage(uri string, t *ast.Task, ref, field string) {
	for _, fn := range a.fullTaskNames(uri, t.Task) {
		a.addUsage(ref, &Usage{Task: fn, URI: uri, Field: field})
	}
}

// collectUsages scans task fields and include paths for template references.
// The scanned fields mirror the properties that variables.go's compiledTask
// renders through the templater (Label, Desc, Prompt, Summary, Sources,
// Generates, Dir, Dotenv, Method, Prefix, Run, If, Cmds, Deps, Preconditions,
// Status, Requires), so every template reference a task run would evaluate is
// covered without evaluating it.
func (a *analyzer) collectUsages(uri string, tf *ast.Taskfile) {
	for namespace, inc := range tf.Includes.All() {
		if inc == nil {
			continue
		}
		field := "include:" + namespace
		a.scanFileString(uri, field, inc.Taskfile)
		a.scanFileString(uri, field+".dir", inc.Dir)
	}

	for name, t := range tf.Tasks.All(nil) {
		if t == nil {
			continue
		}
		fullNames := a.fullTaskNames(uri, name)

		// A task-level env block is exported to that task's shell.
		if t.Env != nil {
			for k := range t.Env.All() {
				for _, fn := range fullNames {
					a.addUsage(k, &Usage{Task: fn, URI: uri, Field: "env:exported"})
				}
			}
		}

		addForAll := func(ref string, field string) {
			for _, fn := range fullNames {
				a.addUsage(ref, &Usage{Task: fn, URI: uri, Field: field})
			}
		}
		scan := func(s, field string, locals map[string]bool) {
			for _, ref := range a.scanRefs(uri, field, s) {
				if !locals[ref] {
					addForAll(ref, field)
				}
			}
		}
		scanStrs := func(ss []string, field string) {
			for _, s := range ss {
				scan(s, field, nil)
			}
		}

		scan(t.Label, "label", nil)
		scan(t.Desc, "desc", nil)
		scan(t.Summary, "summary", nil)
		scan(t.Dir, "dir", nil)
		scan(t.Prefix, "prefix", nil)
		scan(t.Method, "method", nil)
		scan(t.Run, "run", nil)
		scan(t.If, "if", nil)
		scanStrs(t.Prompt, "prompt")
		scanStrs(t.Dotenv, "dotenv")
		scanStrs(t.Status, "status")

		for _, g := range t.Sources {
			if g != nil {
				scan(g.Glob, "sources", nil)
			}
		}
		for _, g := range t.Generates {
			if g != nil {
				scan(g.Glob, "generates", nil)
			}
		}
		for _, p := range t.Preconditions {
			if p != nil {
				scan(p.Sh, "precondition.sh", nil)
				scan(p.Msg, "precondition.msg", nil)
			}
		}
		if t.Requires != nil {
			for _, req := range t.Requires.Vars {
				if req == nil {
					continue
				}
				addForAll(req.Name, "requires")
				if req.Enum != nil && req.Enum.Ref != "" {
					for _, ref := range a.scanRefs(uri, "requires.enum", "{{"+req.Enum.Ref+"}}") {
						addForAll(ref, "requires.enum")
					}
				}
			}
		}

		for _, cmd := range t.Cmds {
			if cmd == nil {
				continue
			}
			locals := forLocals(cmd.For)
			scan(cmd.Cmd, "cmds", locals)
			scan(cmd.If, "cmds.if", locals)
			scan(cmd.Task, "cmds.task", locals)
			a.scanFor(cmd.For, uri, addForAll, "cmds")
			if cmd.Vars != nil {
				for k, v := range cmd.Vars.All() {
					for _, ref := range refsInVar(v) {
						addForAll(ref, "cmds.vars:"+k)
					}
				}
			}
		}

		for _, dep := range t.Deps {
			if dep == nil {
				continue
			}
			locals := forLocals(dep.For)
			scan(dep.Task, "deps.task", locals)
			a.scanFor(dep.For, uri, addForAll, "deps")
			if dep.Vars != nil {
				for k, v := range dep.Vars.All() {
					for _, ref := range refsInVar(v) {
						addForAll(ref, "deps.vars:"+k)
					}
				}
			}
		}
	}
}

func (a *analyzer) scanFor(f *ast.For, uri string, add func(ref, field string), prefix string) {
	if f == nil {
		return
	}
	for _, ref := range a.scanRefs(uri, prefix+".for", f.From) {
		add(ref, prefix+".for")
	}
	for _, ref := range a.scanRefs(uri, prefix+".for.split", f.Split) {
		add(ref, prefix+".for.split")
	}
	if f.Var != "" {
		add(f.Var, prefix+".for.var")
	}
	for _, row := range f.Matrix.All() {
		if row.Ref != "" {
			for _, ref := range a.scanRefs(uri, prefix+".matrix", "{{"+row.Ref+"}}") {
				add(ref, prefix+".matrix")
			}
		}
	}
}

func (a *analyzer) scanFileString(uri, field, s string) {
	for _, ref := range a.scanRefs(uri, field, s) {
		a.addUsage(ref, &Usage{Task: "", URI: uri, Field: field})
	}
}

func (a *analyzer) scanRefs(uri, field, s string) []string {
	refs, err := templater.ExtractVarRefsErr(s)
	if err != nil {
		a.parseErrors = append(a.parseErrors, &ParseError{
			URI: uri, Field: field, Err: err.Error(),
		})
	}
	return refs
}

// refsInVar extracts data-root references from one ast.Var definition,
// covering static values (including nested lists/maps), "sh:" snippets and
// "ref:" expressions. Nothing is evaluated.
func refsInVar(v ast.Var) []string {
	var refs []string
	if v.Ref != "" {
		refs = append(refs, templater.ExtractVarRefs("{{"+v.Ref+"}}")...)
	}
	if v.Sh != nil && *v.Sh != "" {
		refs = append(refs, templater.ExtractVarRefs(*v.Sh)...)
	}
	refs = append(refs, refsInValue(v.Value)...)
	return dedup(refs)
}

// refsInValue walks scalar strings inside a YAML-decoded value.
func refsInValue(v any) []string {
	switch val := v.(type) {
	case string:
		return templater.ExtractVarRefs(val)
	case []any:
		var refs []string
		for _, item := range val {
			refs = append(refs, refsInValue(item)...)
		}
		return refs
	case []string:
		var refs []string
		for _, item := range val {
			refs = append(refs, templater.ExtractVarRefs(item)...)
		}
		return refs
	case map[string]any:
		var refs []string
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			refs = append(refs, refsInValue(val[k])...)
		}
		return refs
	default:
		return nil
	}
}

// forLocals returns the loop-local variable names introduced by a for block.
func forLocals(f *ast.For) map[string]bool {
	locals := map[string]bool{}
	if f == nil {
		return locals
	}
	if f.As == "" {
		locals["ITEM"] = true
	} else {
		for _, name := range strings.Split(f.As, ",") {
			if name = strings.TrimSpace(strings.TrimPrefix(name, "$")); name != "" {
				locals[name] = true
			}
		}
	}
	// KEY is set when iterating over maps; always reserve it inside a loop.
	locals["KEY"] = true
	return locals
}

// precedenceRank mirrors Compiler.getVariables precedence (later Set wins)
// as observed from the merged root Taskfile. task.env never enters the
// template scope, so it ranks below everything. Note: when compiling an
// included task, include.vars and the included file's own vars are applied
// again after the merged Taskfile vars (see Compiler.getVariables); the
// report exposes every definition, while this single global winner reflects
// the merged/root view.
func precedenceRank(d *Definition, rootURI string) int {
	switch d.Scope {
	case ScopeTaskEnv:
		return -1
	case ScopeEnvironment:
		return 0
	case ScopeSpecial:
		return 1
	case ScopeTaskfileEnv:
		return 2
	case ScopeTaskfileVars:
		return 3
	case ScopeIncludeVars:
		return 4
	case ScopeCallVars:
		return 6
	case ScopeTaskVars:
		return 7
	default:
		return -1
	}
}

func (a *analyzer) finalize() {
	specialSet := map[string]bool{}
	for _, n := range specialVars {
		specialSet[n] = true
	}

	names := make([]string, 0, len(a.rows))
	for name := range a.rows {
		names = append(names, name)
	}
	sort.Strings(names)

	// Synthetic environment and special-variable definitions.
	for _, name := range names {
		r := a.row(name)
		if specialSet[name] {
			r.Definitions = append(r.Definitions, &Definition{Scope: ScopeSpecial})
		}
		if value, ok := a.env[name]; ok {
			r.Definitions = append(r.Definitions, &Definition{Scope: ScopeEnvironment})
			secret := false
			for _, d := range r.Definitions {
				secret = secret || d.Secret
			}
			if secret {
				r.EnvValue = "*****"
			} else {
				r.EnvValue = value
			}
		}
	}

	// Reverse variable-to-variable references.
	for _, from := range names {
		r := a.row(from)
		r.References = appendUnique(r.References, a.edges[from]...)
		for _, to := range a.edges[from] {
			a.row(to).ReferencedByVars = appendUnique(a.row(to).ReferencedByVars, from)
		}
	}

	// Propagate direct task usages through the static dependency graph: a
	// task using MESSAGE which renders GREETING also uses GREETING.
	for _, start := range names {
		direct := a.directUsage[start]
		if len(direct) == 0 {
			continue
		}
		seen := map[string]bool{start: true}
		queue := append([]string{}, a.edges[start]...)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if seen[cur] {
				continue
			}
			seen[cur] = true
			r := a.row(cur)
			for _, u := range direct {
				if !containsUsage(r.UsedBy, u) {
					r.UsedBy = append(r.UsedBy, u)
				}
			}
			queue = append(queue, a.edges[cur]...)
		}
	}

	// Direct usages (including non-propagated ones) become UsedBy too.
	for _, name := range names {
		r := a.row(name)
		for _, u := range a.directUsage[name] {
			if !containsUsage(r.UsedBy, u) {
				r.UsedBy = append(r.UsedBy, u)
			}
		}
		sort.SliceStable(r.UsedBy, func(i, j int) bool {
			if r.UsedBy[i].Task != r.UsedBy[j].Task {
				return r.UsedBy[i].Task < r.UsedBy[j].Task
			}
			if r.UsedBy[i].Field != r.UsedBy[j].Field {
				return r.UsedBy[i].Field < r.UsedBy[j].Field
			}
			return r.UsedBy[i].URI < r.UsedBy[j].URI
		})
		sort.Strings(r.ReferencedByVars)
		sort.Strings(r.References)
	}

	// Effective definition per compiler precedence; unused/undefined flags.
	for _, name := range names {
		r := a.row(name)
		var best *Definition
		bestRank := -2
		for _, d := range r.Definitions {
			rank := precedenceRank(d, a.rootURI)
			// On equal precedence the parent/root Taskfile wins: when the
			// include graph is merged, parent vars are Set after child vars.
			if rank > bestRank ||
				(rank == bestRank && best != nil && rank == 3 && d.URI == a.rootURI && best.URI != a.rootURI) {
				bestRank, best = rank, d
			}
		}
		if bestRank >= 0 {
			r.Effective = best
		}
		if _, inEnv := a.env[name]; inEnv && bestRank > 0 {
			r.EnvOverridden = true
		}
		r.Undefined = len(r.Definitions) == 0
		// sh variables are evaluated eagerly by the compiler for every task
		// in scope, so a dynamic definition is never just "unused".
		r.Unused = !r.Undefined &&
			len(r.UsedBy) == 0 &&
			len(r.ReferencedByVars) == 0 &&
			!r.Dynamic
	}

	// Transitive shell dependency chains (cycle-safe BFS).
	dynamicSet := map[string]bool{}
	for _, name := range names {
		if a.row(name).Dynamic {
			dynamicSet[name] = true
		}
	}
	for _, start := range names {
		r := a.row(start)
		if r.Dynamic {
			continue
		}
		if chain, ok := a.reachesDynamic(start, dynamicSet); ok {
			r.DynamicVia = chain
		}
	}

	// Cycle detection per definition scope, with full annotated paths.
	for _, cyc := range a.detectCycles() {
		for _, name := range cyc.names {
			r := a.row(name)
			if !containsCycle(r.Cycles, cyc.names) {
				r.Cycles = append(r.Cycles, cyc.names)
				r.CyclePaths = append(r.CyclePaths, cyc.path)
			}
			r.Unused = false
		}
	}
}

type cycleResult struct {
	names []string
	path  string
}

func (a *analyzer) detectCycles() []cycleResult {
	var results []cycleResult
	seenCycle := map[string]bool{}

	scopeKeys := make([]string, 0, len(a.scopeEdges))
	for k := range a.scopeEdges {
		scopeKeys = append(scopeKeys, k)
	}
	sort.Strings(scopeKeys)

	for _, sk := range scopeKeys {
		edges := a.scopeEdges[sk]
		color := map[string]int{} // 0 white, 1 gray, 2 black
		var stack []string

		var dfs func(u string)
		dfs = func(u string) {
			color[u] = 1
			stack = append(stack, u)
			neighbours := append([]string{}, edges[u]...)
			sort.Strings(neighbours)
			for _, v := range neighbours {
				switch color[v] {
				case 0:
					dfs(v)
				case 1:
					if idx := slices.Index(stack, v); idx >= 0 {
						loop := append(append([]string{}, stack[idx:]...), v)
						if key := canonicalCycleKey(loop); !seenCycle[key] {
							seenCycle[key] = true
							results = append(results, cycleResult{
								names: loop,
								path:  a.annotateCycle(sk, loop),
							})
						}
					}
				}
			}
			stack = stack[:len(stack)-1]
			color[u] = 2
		}

		nodes := make([]string, 0)
		for u := range edges {
			nodes = append(nodes, u)
		}
		sort.Strings(nodes)
		for _, u := range nodes {
			if color[u] == 0 {
				dfs(u)
			}
		}
	}
	return results
}

func (a *analyzer) annotateCycle(scope string, loop []string) string {
	meta := a.scopeMeta[scope]
	scopeLabel := meta.kind
	if meta.kind == "task" {
		scopeLabel = "task " + meta.task
	}
	parts := make([]string, len(loop))
	for i, name := range loop {
		parts[i] = fmt.Sprintf("%s (%s)", name, scopeLabel)
	}
	return strings.Join(parts, " -> ") + "  [" + meta.uri + "]"
}

// reachesDynamic returns the shortest static chain from start to a directly
// shell-evaluated variable, excluding start and including the dynamic end.
func (a *analyzer) reachesDynamic(start string, dynamicSet map[string]bool) ([]string, bool) {
	prev := map[string]string{start: ""}
	visited := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range a.edges[cur] {
			if visited[next] {
				continue
			}
			visited[next] = true
			prev[next] = cur
			if dynamicSet[next] {
				var path []string
				for x := next; x != "" && x != start; x = prev[x] {
					path = append([]string{x}, path...)
				}
				return path, true
			}
			queue = append(queue, next)
		}
	}
	return nil, false
}

func (a *analyzer) buildReport(uris []string) *Report {
	names := make([]string, 0, len(a.rows))
	for name := range a.rows {
		names = append(names, name)
	}
	sort.Strings(names)

	r := &Report{
		RootURI:     a.rootURI,
		Files:       uris,
		ParseErrors: a.parseErrors,
	}
	for _, name := range names {
		r.Variables = append(r.Variables, a.rows[name])
	}
	for i := range a.remoteIncludes {
		inc := a.remoteIncludes[i]
		r.SkippedRemote = append(r.SkippedRemote, &inc)
	}
	return r
}

// Find returns the row for a variable name, or nil.
func (r *Report) Find(name string) *Variable {
	for _, v := range r.Variables {
		if v.Name == name {
			return v
		}
	}
	return nil
}

// DefinedIn reports whether the variable has a definition in the given scope.
func (v *Variable) DefinedIn(scope ScopeKind) bool {
	for _, d := range v.Definitions {
		if d.Scope == scope {
			return true
		}
	}
	return false
}

// UsedByTask reports whether task (full name) references the variable.
func (v *Variable) UsedByTask(name string) bool {
	for _, u := range v.UsedBy {
		if u.Task == name {
			return true
		}
	}
	return false
}

func appendUnique(s []string, vals ...string) []string {
	for _, v := range vals {
		if !slices.Contains(s, v) {
			s = append(s, v)
		}
	}
	return s
}

func dedup(s []string) []string {
	return appendUnique(nil, s...)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func containsUsage(list []*Usage, u *Usage) bool {
	for _, existing := range list {
		if existing.Task == u.Task && existing.URI == u.URI && existing.Field == u.Field {
			return true
		}
	}
	return false
}

func containsCycle(list [][]string, cyc []string) bool {
	for _, existing := range list {
		if len(existing) != len(cyc) {
			continue
		}
		same := true
		for i := range existing {
			if existing[i] != cyc[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// canonicalCycleKey rotates the cycle (without its repeated endpoint) so the
// same loop found from different starts is only reported once.
func canonicalCycleKey(loop []string) string {
	if len(loop) <= 1 {
		return strings.Join(loop, ",")
	}
	nodes := loop[:len(loop)-1]
	minIdx := 0
	for i := 1; i < len(nodes); i++ {
		if nodes[i] < nodes[minIdx] {
			minIdx = i
		}
	}
	rotated := make([]string, 0, len(nodes)+1)
	for i := 0; i < len(nodes); i++ {
		rotated = append(rotated, nodes[(minIdx+i)%len(nodes)])
	}
	rotated = append(rotated, rotated[0])
	return strings.Join(rotated, ",")
}

func rootVertex(g graph.Graph[string, *ast.TaskfileVertex]) (string, error) {
	hashes, err := graph.TopologicalSort(g)
	if err == nil && len(hashes) > 0 {
		return hashes[0], nil
	}
	preds, err := g.PredecessorMap()
	if err != nil {
		return "", err
	}
	for uri := range preds {
		if len(preds[uri]) == 0 {
			return uri, nil
		}
	}
	return "", fmt.Errorf("varreport: could not determine root Taskfile")
}
