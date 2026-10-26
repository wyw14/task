package varreport_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/internal/varreport"
	"github.com/go-task/task/v3/taskfile"
)

func analyzeFixture(t *testing.T, name string, opts ...varreport.Option) (*varreport.Report, *taskfile.Reader) {
	t.Helper()
	dir := filepath.Join("testdata", name)
	node, err := taskfile.NewRootNode(filepath.Join(dir, "Taskfile.yml"), "", false, 0)
	require.NoError(t, err)

	reader := taskfile.NewReader(taskfile.WithStaticAnalysis(true))
	g, err := reader.Read(context.Background(), node)
	require.NoError(t, err)

	skipped := make([]varreport.RemoteInclude, 0)
	for _, s := range reader.SkippedRemoteIncludes() {
		skipped = append(skipped, varreport.RemoteInclude{
			Source: s.Source, Namespace: s.Namespace, URI: s.URI,
		})
	}
	environ := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			environ[k] = v
		}
	}
	opts = append(opts,
		varreport.WithRemoteIncludes(skipped),
		varreport.WithEnvironment(environ),
	)

	report, err := varreport.Analyze(g, opts...)
	require.NoError(t, err)
	return report, reader
}

func TestMultiFileInclude(t *testing.T) {
	report, _ := analyzeFixture(t, "multi_include")

	require.Len(t, report.Files, 3, "root, lib and nested sub taskfiles analyzed")

	root := report.Find("ROOT_VAR")
	require.NotNil(t, root)
	assert.True(t, root.DefinedIn(varreport.ScopeTaskfileVars))
	assert.Contains(t, root.Definitions[0].URI, "multi_include"+string(filepath.Separator)+"Taskfile.yml")
	assert.True(t, root.UsedByTask("root-task"))
	assert.False(t, root.Unused)

	lib := report.Find("LIB_VAR")
	require.NotNil(t, lib)
	var libDef *varreport.Definition
	for _, d := range lib.Definitions {
		if d.Scope == varreport.ScopeTaskfileVars {
			libDef = d
		}
	}
	require.NotNil(t, libDef)
	assert.Contains(t, libDef.URI, filepath.Join("lib", "Taskfile.yml"))
	assert.True(t, lib.UsedByTask("lib:build"), "lib task is namespaced")
	assert.True(t, lib.UsedByTask("root-task"), "root task references the merged var")

	sub := report.Find("SUB_VAR")
	require.NotNil(t, sub)
	assert.True(t, sub.UsedByTask("lib:sub:clean"), "nested include namespace accumulates")
	assert.True(t, sub.UsedByTask("lib:build"))

	inc := report.Find("LIB_INCLUDE_VAR")
	require.NotNil(t, inc)
	assert.True(t, inc.DefinedIn(varreport.ScopeIncludeVars))
	assert.Equal(t, "lib", inc.Definitions[0].Namespace)
	assert.Contains(t, inc.References, "ROOT_VAR")

	shared := report.Find("SHARED")
	require.NotNil(t, shared)
	require.Len(t, shared.Definitions, 2, "same name in root and included file")
	require.NotNil(t, shared.Effective)
	assert.Equal(t, varreport.ScopeTaskfileVars, shared.Effective.Scope)
	assert.Contains(t, shared.Effective.URI, "multi_include"+string(filepath.Separator)+"Taskfile.yml")

	unused := report.Find("UNUSED_ROOT")
	require.NotNil(t, unused)
	assert.True(t, unused.Unused)
	assert.Empty(t, report.ParseErrors)
}

func TestEnvironmentOverride(t *testing.T) {
	t.Setenv("EXTERNAL_ENV", "from-os")
	t.Setenv("OVERRIDDEN", "from-os")

	report, _ := analyzeFixture(t, "env_override")

	overridden := report.Find("OVERRIDDEN")
	require.NotNil(t, overridden)
	assert.True(t, overridden.DefinedIn(varreport.ScopeTaskfileEnv))
	assert.True(t, overridden.DefinedIn(varreport.ScopeTaskfileVars))
	assert.True(t, overridden.DefinedIn(varreport.ScopeEnvironment))
	assert.True(t, overridden.EnvOverridden)
	assert.Equal(t, "from-os", overridden.EnvValue)
	require.NotNil(t, overridden.Effective)
	assert.Equal(t, varreport.ScopeTaskfileVars, overridden.Effective.Scope)
	assert.True(t, overridden.UsedByTask("show"))

	external := report.Find("EXTERNAL_ENV")
	require.NotNil(t, external)
	assert.True(t, external.DefinedIn(varreport.ScopeEnvironment))
	assert.False(t, external.Undefined)
	assert.True(t, external.UsedByTask("show"))

	onlyEnv := report.Find("ONLY_ENV_BLOCK")
	require.NotNil(t, onlyEnv)
	assert.True(t, onlyEnv.DefinedIn(varreport.ScopeTaskfileEnv))
	assert.True(t, onlyEnv.UsedByTask("show"))

	varOnly := report.Find("VAR_ONLY")
	require.NotNil(t, varOnly)
	assert.True(t, varOnly.UsedByTask("show"), "referenced from task env block template")

	localEnv := report.Find("TASK_LOCAL_ENV")
	require.NotNil(t, localEnv)
	assert.True(t, localEnv.DefinedIn(varreport.ScopeTaskEnv))
	assert.False(t, localEnv.Undefined)
	assert.Nil(t, localEnv.Effective, "task env does not define a template variable")
}

func TestTemplateNesting(t *testing.T) {
	report, _ := analyzeFixture(t, "template_nesting")

	now := report.Find("NOW")
	require.NotNil(t, now)
	assert.True(t, now.Dynamic)
	assert.Equal(t, "echo now", now.Definitions[0].Sh)

	secret := report.Find("SECRET_NOW")
	require.NotNil(t, secret)
	assert.True(t, secret.Dynamic)
	assert.True(t, secret.Definitions[0].Secret)

	timeMsg := report.Find("TIME_MSG")
	require.NotNil(t, timeMsg)
	require.NotEmpty(t, timeMsg.DynamicVia)
	assert.Equal(t, "NOW", timeMsg.DynamicVia[len(timeMsg.DynamicVia)-1])
	assert.True(t, timeMsg.UsedByTask("nested"))

	message := report.Find("MESSAGE")
	require.NotNil(t, message)
	assert.Contains(t, message.References, "GREETING")
	assert.Contains(t, message.References, "LIST_VAR")
	assert.True(t, message.UsedByTask("nested"))

	greeting := report.Find("GREETING")
	require.NotNil(t, greeting)
	assert.Contains(t, greeting.References, "NAME")
	assert.True(t, greeting.UsedByTask("nested"),
		"transitive: nested uses MESSAGE which renders GREETING")

	viaRef := report.Find("VIA_REF")
	require.NotNil(t, viaRef)
	assert.Contains(t, viaRef.References, "MESSAGE")
	assert.Equal(t, ".MESSAGE", viaRef.Definitions[0].Ref)

	listVar := report.Find("LIST_VAR")
	require.NotNil(t, listVar)
	assert.Contains(t, listVar.References, "NAME")
	assert.Contains(t, listVar.References, "GREETING", "templates inside nested lists are extracted")

	items := report.Find("ITEMS_VAR")
	require.NotNil(t, items)
	assert.True(t, items.DefinedIn(varreport.ScopeTaskVars))
	assert.True(t, items.UsedByTask("nested"), "for.var references the task variable")

	// Loop-local variables (ITEM/KEY) are not Taskfile definitions: they
	// must not be reported as undefined when the loop exposes them, while a
	// plain use of ITEM outside a loop resolves to Task's special variable.
	item := report.Find("ITEM")
	require.NotNil(t, item)
	assert.True(t, item.DefinedIn(varreport.ScopeSpecial))
	assert.False(t, item.Undefined)
	assert.Nil(t, report.Find("KEY"), "KEY is only a for-loop local in this fixture")

	unused := report.Find("UNUSED_TPL")
	require.NotNil(t, unused)
	assert.True(t, unused.Unused, "referencing NAME does not make UNUSED_TPL used")

	name := report.Find("NAME")
	require.NotNil(t, name)
	assert.False(t, name.Unused)
	assert.Empty(t, report.ParseErrors)
}

func TestCyclesHaveFullPath(t *testing.T) {
	report, _ := analyzeFixture(t, "cycles")

	want := []string{"CYCA", "CYCB", "CYCC", "CYCA"}
	for _, name := range []string{"CYCA", "CYCB", "CYCC"} {
		row := report.Find(name)
		require.NotNil(t, row, name)
		found := false
		for _, cyc := range row.Cycles {
			if slices.Equal(cyc, want) {
				found = true
			}
		}
		assert.True(t, found, "%s should carry the full cycle %v", name, want)
		require.NotEmpty(t, row.CyclePaths)
		path := row.CyclePaths[0]
		assert.True(t, strings.Contains(path, "CYCA") &&
			strings.Contains(path, "CYCB") &&
			strings.Contains(path, "CYCC"))
		assert.Contains(t, path, filepath.Join("cycles", "Taskfile.yml"))
		assert.False(t, row.Unused)
	}

	self := report.Find("SELF")
	require.NotNil(t, self)
	require.NotEmpty(t, self.Cycles)
	assert.Equal(t, []string{"SELF", "SELF"}, self.Cycles[0])

	taskCycle := report.Find("LOOPA")
	require.NotNil(t, taskCycle)
	found := false
	for _, cyc := range taskCycle.Cycles {
		if slices.Equal(cyc, []string{"LOOPA", "LOOPB", "LOOPA"}) {
			found = true
		}
	}
	assert.True(t, found)
	assert.Contains(t, taskCycle.CyclePaths[0], "task t")
}

func TestRemoteIncludesAreSkippedNotDownloaded(t *testing.T) {
	report, reader := analyzeFixture(t, "remote_skip")

	skipped := reader.SkippedRemoteIncludes()
	require.Len(t, skipped, 1)
	assert.Equal(t, "web", skipped[0].Namespace)
	assert.Contains(t, skipped[0].URI, "example.invalid")
	assert.Contains(t, skipped[0].Source, "remote_skip")

	require.Len(t, report.Files, 2, "remote branch is absent from the graph")
	require.Len(t, report.SkippedRemote, 1)

	local := report.Find("LOCAL_VAR")
	require.NotNil(t, local)
	assert.True(t, local.UsedByTask("local:local-task"))
	assert.True(t, local.UsedByTask("hi"))
}

func TestNilGraph(t *testing.T) {
	_, err := varreport.Analyze(nil)
	assert.Error(t, err)
}
