package varreport

import (
	"fmt"
	"io"
	"strings"
)

// RenderText writes a human-readable version of the report.
func (r *Report) RenderText(w io.Writer) {
	fmt.Fprintf(w, "Taskfile static variable report\n")
	fmt.Fprintf(w, "Root: %s\n", r.RootURI)
	fmt.Fprintf(w, "Files analyzed: %d\n", len(r.Files))
	for _, f := range r.Files {
		fmt.Fprintf(w, "  - %s\n", f)
	}
	if len(r.SkippedRemote) > 0 {
		fmt.Fprintf(w, "Remote includes skipped (not downloaded): %d\n", len(r.SkippedRemote))
		for _, inc := range r.SkippedRemote {
			ns := inc.Namespace
			if ns == "" {
				ns = "(default)"
			}
			fmt.Fprintf(w, "  - %s -> %s (in %s)\n", ns, inc.URI, inc.Source)
		}
	}
	fmt.Fprintln(w)

	for _, v := range r.Variables {
		fmt.Fprintf(w, "Variable: %s\n", v.Name)

		if len(v.Definitions) > 0 {
			fmt.Fprintln(w, "  defined:")
			for _, d := range v.Definitions {
				fmt.Fprintf(w, "    - %s\n", formatDefinition(d))
			}
		}
		if v.Effective != nil {
			fmt.Fprintf(w, "  effective: %s\n", formatDefinition(v.Effective))
		}
		if resolved, ok := r.Resolved[v.Name]; ok {
			fmt.Fprintf(w, "  resolved (static, no shell): %q\n", resolved)
		}
		if v.EnvValue != "" || v.DefinedIn(ScopeEnvironment) {
			env := "environment: present"
			if v.EnvValue != "" {
				env = "environment: " + v.EnvValue
			}
			if v.EnvOverridden {
				env += " (overridden by Taskfile definition)"
			}
			fmt.Fprintf(w, "  %s\n", env)
		}
		if len(v.UsedBy) > 0 {
			fmt.Fprintln(w, "  referenced by:")
			for _, u := range v.UsedBy {
				target := u.Field
				if u.Task != "" {
					target = "task " + u.Task + " [" + u.Field + "]"
				}
				fmt.Fprintf(w, "    - %s (%s)\n", target, u.URI)
			}
		}
		if len(v.ReferencedByVars) > 0 {
			fmt.Fprintf(w, "  referenced by vars: %s\n", strings.Join(v.ReferencedByVars, ", "))
		}
		if len(v.References) > 0 {
			fmt.Fprintf(w, "  references vars: %s\n", strings.Join(v.References, ", "))
		}
		if v.Dynamic {
			fmt.Fprintln(w, "  shell-evaluated: yes (sh: definition)")
		}
		if len(v.DynamicVia) > 0 {
			fmt.Fprintf(w, "  shell-evaluated via: %s -> %s\n", v.Name, strings.Join(v.DynamicVia, " -> "))
		}
		if v.Undefined {
			fmt.Fprintln(w, "  status: referenced but undefined")
		} else if v.Unused {
			fmt.Fprintln(w, "  status: unused")
		} else {
			fmt.Fprintln(w, "  status: used")
		}
		for _, p := range v.CyclePaths {
			fmt.Fprintf(w, "  cycle: %s\n", p)
		}
		fmt.Fprintln(w)
	}

	if len(r.ParseErrors) > 0 {
		fmt.Fprintf(w, "Template parse errors: %d\n", len(r.ParseErrors))
		for _, pe := range r.ParseErrors {
			fmt.Fprintf(w, "  - %s [%s]: %s\n", pe.URI, pe.Field, pe.Err)
		}
	}
}

func formatDefinition(d *Definition) string {
	parts := []string{string(d.Scope)}
	switch {
	case d.URI != "" && d.Task != "":
		parts = append(parts, "task "+d.Task+" @ "+d.URI)
	case d.URI != "":
		parts = append(parts, "@ "+d.URI)
	}
	if d.Namespace != "" {
		parts = append(parts, "namespace "+d.Namespace)
	}
	if d.Dynamic {
		sh := d.Sh
		if len(sh) > 60 {
			sh = sh[:57] + "..."
		}
		parts = append(parts, "sh: "+sh)
	}
	if d.Ref != "" {
		parts = append(parts, "ref: "+d.Ref)
	}
	if d.Secret {
		parts = append(parts, "secret")
	}
	if d.Exported {
		parts = append(parts, "exported to shell")
	}
	return strings.Join(parts, " | ")
}
