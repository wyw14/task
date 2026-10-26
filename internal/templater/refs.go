package templater

import (
	"strings"
	"text/template/parse"

	gotemplate "github.com/go-task/template"
)

// ExtractVarRefs statically parses a Go-template string and returns the
// data-root variable names it references, e.g. FOO for "{{.FOO}}" or
// "{{$.FOO.Bar}}". It never executes the template, so shell commands and
// dynamic variables are not evaluated.
//
// Template-local variables declared inside the string itself (such as the
// "$i"/"$v" in '{{range $i, $v := .ITEMS}}') are not reported: only
// references to the data root are returned. Function names (e.g. "default",
// "upper") are ignored.
//
// Names are returned in first-use order with duplicates removed. Strings
// that do not contain template actions (or that fail to parse) return nil.
func ExtractVarRefs(s string) []string {
	refs, _ := ExtractVarRefsErr(s)
	return refs
}

// ExtractVarRefsErr is like ExtractVarRefs, but also reports template parse
// errors. The analysis never executes the template, so a parse error is the
// only error it can produce.
func ExtractVarRefsErr(s string) ([]string, error) {
	// Optimization: skip if the string is not a template, mirroring Replace.
	if !strings.Contains(s, "{{") {
		return nil, nil
	}

	tmpl, err := gotemplate.New("varrefs").Funcs(templateFuncs).Parse(s)
	if err != nil {
		return nil, err
	}
	if tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return nil, nil
	}

	v := &refVisitor{
		locals:  make(map[string]struct{}),
		declPos: make(map[parse.Pos]struct{}),
		seen:    make(map[string]struct{}),
	}
	// A first walk registers variables declared by the template itself
	// (range/with/if assignments and standalone '$x := ...' assignments), so
	// uses of those locals are not mistaken for data-root variables.
	v.walk(tmpl.Tree.Root)

	// A second walk collects the data-root references. Both walks share the
	// local-variable set collected by the first one.
	v.collect = true
	v.walk(tmpl.Tree.Root)

	if len(v.refs) == 0 {
		return nil, nil
	}
	return v.refs, nil
}

type refVisitor struct {
	// locals holds template-local variable names ("$x") declared by pipes.
	locals map[string]struct{}
	// declPos holds the byte positions of declaration variable nodes, so the
	// declaration itself is not counted as a use.
	declPos map[parse.Pos]struct{}
	// seen de-duplicates referenced names.
	seen map[string]struct{}
	// refs preserves first-use order.
	refs []string
	// collect toggles between the registration pass and the collection pass.
	collect bool
}

// walk traverses a parse tree (the standard library does not export
// parse.Walk), applying v.visit to every node.
func (v *refVisitor) walk(node parse.Node) {
	if node == nil {
		return
	}
	v.visit(node)

	switch n := node.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				v.walk(child)
			}
		}
	case *parse.ActionNode:
		if n.Pipe != nil {
			v.walk(n.Pipe)
		}
	case *parse.PipeNode:
		for _, decl := range n.Decl {
			v.walk(decl)
		}
		for _, cmd := range n.Cmds {
			v.walk(cmd)
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			v.walk(arg)
		}
	case *parse.ChainNode:
		v.walk(n.Node)
	case *parse.IfNode:
		v.walk(n.Pipe)
		v.walk(n.List)
		v.walk(n.ElseList)
	case *parse.RangeNode:
		v.walk(n.Pipe)
		v.walk(n.List)
		v.walk(n.ElseList)
	case *parse.WithNode:
		v.walk(n.Pipe)
		v.walk(n.List)
		v.walk(n.ElseList)
	case *parse.TemplateNode:
		if n.Pipe != nil {
			v.walk(n.Pipe)
		}
	}
}

func (v *refVisitor) visit(node parse.Node) {
	switch n := node.(type) {
	case *parse.PipeNode:
		// Register pipe declarations ('{{$x := .A}}', 'range $i, $v := ...').
		for _, decl := range n.Decl {
			if len(decl.Ident) > 0 {
				v.locals[decl.Ident[0]] = struct{}{}
				v.declPos[decl.Pos] = struct{}{}
			}
		}
	case *parse.VariableNode:
		if !v.collect {
			return
		}
		if _, isDecl := v.declPos[n.Pos]; isDecl {
			return
		}
		// "$.FOO" references the FOO data-root variable. Plain "$x" (even when
		// chained, e.g. "$x.Bar") is a template-local variable.
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			v.add(n.Ident[1])
		}
	case *parse.FieldNode:
		if v.collect && len(n.Ident) > 0 {
			v.add(n.Ident[0])
		}
	}
}

func (v *refVisitor) add(name string) {
	if name == "" {
		return
	}
	if _, ok := v.seen[name]; ok {
		return
	}
	v.seen[name] = struct{}{}
	v.refs = append(v.refs, name)
}
