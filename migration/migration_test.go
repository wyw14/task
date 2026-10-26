package migration

import (
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/go-task/task/v3/taskfile/ast"
)

func parseFixture(t *testing.T, uri, src string) (*yaml.Node, *ast.Taskfile) {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(src), &doc))
	var tf ast.Taskfile
	require.NoError(t, yaml.Unmarshal([]byte(src), &tf))
	tf.Location = uri
	return &doc, &tf
}

func findChange(t *testing.T, report *FileReport, path, action string) Change {
	t.Helper()
	for _, c := range report.Changes {
		if c.Path == path && c.Action == action {
			return c
		}
	}
	t.Fatalf("change %s/%s not found; got: %#v", path, action, report.Changes)
	return Change{}
}

func hasCode(list any, code string) bool {
	switch v := list.(type) {
	case []Warning:
		for _, w := range v {
			if w.Code == code {
				return true
			}
		}
	case []ManualAction:
		for _, m := range v {
			if m.Code == code {
				return true
			}
		}
	}
	return false
}

func TestMigrateV2ToV3(t *testing.T) {
	t.Parallel()

	src := `version: '2'
expansions: 3
tasks:
  build:
    cmds:
      - echo build
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
	require.NoError(t, err)

	// version: '2' -> '3'
	versionChange := findChange(t, report, "version", ActionSet)
	assert.Equal(t, "2", versionChange.OldValue)
	assert.Equal(t, "3", versionChange.NewValue)
	assert.Equal(t, 1, versionChange.Line)

	// expansions removed with a warning because it used a non-default value
	expansionsChange := findChange(t, report, "expansions", ActionRemove)
	assert.Equal(t, "3", expansionsChange.OldValue)
	assert.True(t, hasCode(report.Warnings, codeExpansionsRemoved),
		"non-default expansions must warn; got %#v", report.Warnings)

	// method: timestamp added to preserve v2 behaviour
	methodChange := findChange(t, report, "method", ActionAdd)
	assert.Equal(t, "timestamp", methodChange.NewValue)
	assert.True(t, hasCode(report.Warnings, codeMethodDefaultChanged))
	assert.True(t, hasCode(report.Warnings, codeCLIVarsGlobal))

	// The migrated in-memory copy is on v3 with the method set.
	migrated := report.Migrated()
	require.NotNil(t, migrated)
	assert.True(t, migrated.Version.Equal(ast.V3))
	assert.Equal(t, "timestamp", migrated.Method)

	// The original inputs are untouched: no writeback to in-memory objects.
	root := doc.Content[0]
	_, versionVal := findPair(root, "version")
	assert.Equal(t, "2", versionVal.Value, "original YAML node must not be mutated")
	_, expansionsVal := findPair(root, "expansions")
	require.NotNil(t, expansionsVal, "original YAML node must keep expansions")
	assert.Equal(t, "3", expansionsVal.Value)
	assert.True(t, tf.Version.LessThan(ast.V3), "original AST must stay on v2")
}

func TestMigrateV2DefaultExpansionsWarnsOnlyOnce(t *testing.T) {
	t.Parallel()

	src := `version: '2'
expansions: 2
tasks:
  build: echo build
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
	require.NoError(t, err)

	findChange(t, report, "expansions", ActionRemove)
	assert.False(t, hasCode(report.Warnings, codeExpansionsRemoved),
		"default expansions value should not produce a behaviour warning")
}

func TestMigrateV3IsNoop(t *testing.T) {
	t.Parallel()

	src := `version: '3'
tasks:
  build:
    cmds:
      - echo build
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
	require.NoError(t, err)

	assert.Empty(t, report.Changes)
	assert.Empty(t, report.Warnings)
	assert.Empty(t, report.ManualActions)
	assert.True(t, report.Migrated().Version.Equal(ast.V3))
}

func TestMigrateExtensionFields(t *testing.T) {
	t.Parallel()

	src := `version: '2'
future_feature: true
includes:
  sub:
    taskfile: ./sub/Taskfile.yml
    bogus: 1
tasks:
  build:
    cmds:
      - echo build
    magik: true
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
	require.NoError(t, err)

	codes := make([]string, 0, len(report.ManualActions))
	for _, m := range report.ManualActions {
		codes = append(codes, m.Code)
	}
	assert.Contains(t, codes, codeUnknownRootField)
	assert.Contains(t, codes, codeUnknownTaskField)
	assert.Contains(t, codes, codeUnknownIncludeField)

	// "expansions" is a known (but removed) v2 field and must never be
	// reported as an extension field.
	for _, m := range report.ManualActions {
		assert.NotContains(t, m.Message, "expansions")
	}

	// Migration still applies the safe changes.
	findChange(t, report, "version", ActionSet)
}

func TestMigrateAnchorsAndMergeKeys(t *testing.T) {
	t.Parallel()

	src := `version: '2'
tasks:
  base: &base
    cmds:
      - echo base
    magik: true
  real:
    <<: *base
    desc: real task
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
	require.NoError(t, err)

	// The unknown field inside the anchor target is reported exactly once,
	// even though the mapping is also pulled in through a merge alias.
	var unknown []ManualAction
	for _, m := range report.ManualActions {
		if m.Code == codeUnknownTaskField {
			unknown = append(unknown, m)
		}
	}
	require.Len(t, unknown, 1)
	assert.Contains(t, unknown[0].Message, `task "base"`)
	assert.Contains(t, unknown[0].Message, "magik")
	// It must point at the field declaration inside the anchor definition.
	assert.Equal(t, 6, unknown[0].Line)

	// Migration stays valid: the merge key still resolves after re-decoding.
	migrated := report.Migrated()
	realTask, ok := migrated.Tasks.Get("real")
	require.True(t, ok)
	assert.Equal(t, "real task", realTask.Desc)
	baseTask, ok := migrated.Tasks.Get("base")
	require.True(t, ok)
	require.Len(t, baseTask.Cmds, 1)

	// The original document must be untouched.
	_, originalVersion := findPair(doc.Content[0], "version")
	require.NotNil(t, originalVersion)
	assert.Equal(t, "2", originalVersion.Value)
}

func TestMigrateIncludeSourceLines(t *testing.T) {
	t.Parallel()

	src := `version: '2'

includes:
  sub: ./sub/Taskfile.yml
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
	require.NoError(t, err)

	require.Len(t, report.Includes, 1)
	include := report.Includes[0]
	assert.Equal(t, "sub", include.Namespace)
	assert.Equal(t, "./sub/Taskfile.yml", include.Taskfile)
	assert.Equal(t, 4, include.Line, "include source line must be preserved")
	assert.Equal(t, 3, include.Column)
}

func TestMigrateFailureRollsBack(t *testing.T) {
	t.Parallel()

	src := `version: '2'
expansions: 3
tasks:
  boom:
    cmds:
      - echo boom
`
	doc, tf := parseFixture(t, "Taskfile.yml", src)

	failingRule := Rule(func(c *RuleContext) error {
		_, tasksNode := findPair(c.Root, "tasks")
		tasksNode = resolve(tasksNode)
		if key, _ := findPair(tasksNode, "boom"); key != nil {
			return assert.AnError
		}
		return nil
	})

	report, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, []Rule{failingRule})
	require.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, report, "no partial migration must be returned on failure")

	// The original YAML document is unchanged.
	root := doc.Content[0]
	_, versionVal := findPair(root, "version")
	assert.Equal(t, "2", versionVal.Value)
	key, expansionsVal := findPair(root, "expansions")
	require.NotNil(t, key)
	assert.Equal(t, "3", expansionsVal.Value)

	// The original AST is unchanged.
	assert.True(t, tf.Version.LessThan(ast.V3))
	assert.Equal(t, "", tf.Method)
}

func TestMigrateUnsupportedVersions(t *testing.T) {
	t.Parallel()

	t.Run("v1 cannot be migrated", func(t *testing.T) {
		t.Parallel()
		doc, tf := parseFixture(t, "Taskfile.yml", "version: '1'\ntasks: {}\n")
		_, err := migrateDocument("Taskfile.yml", doc, tf, ast.V3, nil)
		require.Error(t, err)
		assert.True(t, tf.Version.LessThan(ast.V3))
	})

	t.Run("unsupported target", func(t *testing.T) {
		t.Parallel()
		doc, tf := parseFixture(t, "Taskfile.yml", "version: '2'\ntasks: {}\n")
		target := semver.MustParse("4")
		_, err := migrateDocument("Taskfile.yml", doc, tf, target, nil)
		require.Error(t, err)
	})

	t.Run("downgrade is unsupported", func(t *testing.T) {
		t.Parallel()
		doc, tf := parseFixture(t, "Taskfile.yml", "version: '3'\ntasks: {}\n")
		target := semver.MustParse("2")
		_, err := migrateDocument("Taskfile.yml", doc, tf, target, nil)
		require.Error(t, err)
	})
}
