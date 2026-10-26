package templater

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractVarRefs(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		want []string
	}{
		{
			name: "plain string",
			tmpl: "echo hello",
			want: nil,
		},
		{
			name: "single field",
			tmpl: "echo {{.FOO}}",
			want: []string{"FOO"},
		},
		{
			name: "multiple fields, first-use order and dedup",
			tmpl: "{{.A}}-{{.B}}-{{.A}}-{{.C}}",
			want: []string{"A", "B", "C"},
		},
		{
			name: "dollar root notation",
			tmpl: "{{$.FOO}}/{{$.BAR.BAZ}}",
			want: []string{"FOO", "BAR"},
		},
		{
			name: "nested pipes with functions",
			tmpl: `{{.GREETING | default .FALLBACK | upper}}-{{trim .NAME}}`,
			want: []string{"GREETING", "FALLBACK", "NAME"},
		},
		{
			name: "if action with function",
			tmpl: `{{if eq .OS "windows"}}{{.WIN_PATH}}{{else}}{{.NIX_PATH}}{{end}}`,
			want: []string{"OS", "WIN_PATH", "NIX_PATH"},
		},
		{
			name: "range locals are not variables",
			tmpl: `{{range $i, $v := .ITEMS}}{{$i}}:{{$v}} {{$.ROOT}} {{.LABEL}}{{end}}`,
			want: []string{"ITEMS", "ROOT", "LABEL"},
		},
		{
			name: "with assignment local",
			tmpl: `{{with $x := .INNER}}{{$x.NAME}}{{end}}{{.AFTER}}`,
			want: []string{"INNER", "AFTER"},
		},
		{
			name: "standalone assignment local",
			tmpl: `{{$x := .FIRST}}{{$x}} {{$x.Second}} {{.SECOND}}`,
			want: []string{"FIRST", "SECOND"},
		},
		{
			name: "function names are not reported",
			tmpl: `{{OS}} {{ARCH}} {{default "x" .NAME}}`,
			want: []string{"NAME"},
		},
		{
			name: "nested template literal inside a string argument",
			tmpl: `{{.GREETING | default "{{.FALLBACK}}"}}`,
			want: []string{"GREETING"},
		},
		{
			name: "range without declaration still captures data fields",
			tmpl: `{{range .ITEMS}}{{.DISPLAY}}{{end}}`,
			want: []string{"ITEMS", "DISPLAY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractVarRefs(tt.tmpl))
		})
	}
}

func TestExtractVarRefsErr(t *testing.T) {
	t.Run("parse error is returned", func(t *testing.T) {
		refs, err := ExtractVarRefsErr("{{.BROKEN")
		require.Error(t, err)
		assert.Nil(t, refs)
	})

	t.Run("valid template has no error", func(t *testing.T) {
		refs, err := ExtractVarRefsErr("{{.A}}")
		require.NoError(t, err)
		assert.Equal(t, []string{"A"}, refs)
	})

	t.Run("execution never happens", func(t *testing.T) {
		// A function that would fail at execution time must not be called
		// during static extraction.
		refs, err := ExtractVarRefsErr(`{{fail "boom" .A}}`)
		require.NoError(t, err)
		assert.Equal(t, []string{"A"}, refs)
	})
}
