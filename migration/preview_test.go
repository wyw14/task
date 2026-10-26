package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3/taskfile"
	"github.com/go-task/task/v3/taskfile/ast"
)

func writeFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(relPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func rootNode(t *testing.T, dir string) taskfile.Node {
	t.Helper()
	node, err := taskfile.NewFileNode(filepath.Join(dir, "Taskfile.yml"), dir)
	require.NoError(t, err)
	return node
}

func hashPath(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	var paths []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}))
	files := make(map[string]string, len(paths))
	for _, path := range paths {
		rel, err := filepath.Rel(dir, path)
		require.NoError(t, err)
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		sum := sha256.Sum256(b)
		files[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
	}
	return files
}

func manualCodes(report *FileReport) []string {
	codes := make([]string, 0, len(report.ManualActions))
	for _, m := range report.ManualActions {
		codes = append(codes, m.Code)
	}
	sort.Strings(codes)
	return codes
}

const v2RootWithSub = `version: '2'

includes:
  sub: ./sub/Taskfile.yml

tasks:
  default:
    cmds:
      - task: sub:hi
`

const v3SubWithDotenv = `version: '3'
dotenv:
  - .env
tasks:
  hi: echo hi
`

func TestPreviewV2ToV3WithIncludeDotenvLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rootPath := writeFile(t, dir, "Taskfile.yml", v2RootWithSub)
	subPath := writeFile(t, dir, "sub/Taskfile.yml", v3SubWithDotenv)

	previewer := NewPreviewer()
	report, err := previewer.Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)

	// Root Taskfile first, included Taskfile second.
	require.Len(t, report.Files, 2)
	root := report.File(rootPath)
	require.NotNil(t, root)
	sub := report.File(subPath)
	require.NotNil(t, sub)

	assert.Equal(t, "2.0.0", root.FromVersion)
	assert.Equal(t, "3.0.0", root.TargetVersion)
	findChange(t, root, "version", ActionSet)
	assert.True(t, root.Migrated().Version.Equal(ast.V3))

	// Include source line is preserved on the report.
	require.Len(t, root.Includes, 1)
	assert.Equal(t, 4, root.Includes[0].Line)
	assert.Equal(t, "sub", root.Includes[0].Namespace)

	// The non-migratable dotenv include points at the include declaration.
	codes := manualCodes(root)
	assert.Contains(t, codes, codeIncludedDotenv)
	var dotenv ManualAction
	for _, m := range root.ManualActions {
		if m.Code == codeIncludedDotenv {
			dotenv = m
		}
	}
	assert.Equal(t, rootPath, dotenv.Taskfile)
	assert.Equal(t, 4, dotenv.Line, "manual action must point at the include source line")
	assert.Contains(t, dotenv.Message, fmt.Sprintf("%q", subPath))

	// Validation is skipped (not failed) when the only blocking issue is the
	// reported dotenv include.
	assert.True(t, report.ValidationSkipped)
	assert.NotEmpty(t, report.ValidationNote)

	// The included v3 Taskfile itself requires no changes.
	assert.Empty(t, sub.Changes)
}

func TestPreviewNeverWritesBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rootPath := writeFile(t, dir, "Taskfile.yml", v2RootWithSub)
	subPath := writeFile(t, dir, "sub/Taskfile.yml", v3SubWithDotenv)
	before := snapshotTree(t, dir)
	rootHash := hashPath(t, rootPath)
	subHash := hashPath(t, subPath)

	_, err := NewPreviewer().Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)

	// No file content changes and no new or deleted files.
	after := snapshotTree(t, dir)
	assert.Equal(t, before, after)
	assert.Equal(t, rootHash, hashPath(t, rootPath), "root Taskfile must not be written back")
	assert.Equal(t, subHash, hashPath(t, subPath), "included Taskfile must not be written back")
}

func TestPreviewNeverExecutesCommands(t *testing.T) {
	t.Parallel()

	src := `version: '2'
vars:
  NAME: world
  GREETING: 'hello {{.NAME}}'
  SENTINEL:
    sh: "echo ran > sentinel.txt"
tasks:
  default:
    cmds:
      - echo "{{.GREETING}}"
`
	dir := t.TempDir()
	writeFile(t, dir, "Taskfile.yml", src)

	report, err := NewPreviewer().Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)

	// The dynamic variable command must not have been executed.
	_, statErr := os.Stat(filepath.Join(dir, "sentinel.txt"))
	assert.True(t, os.IsNotExist(statErr), "dynamic variable command must not run during preview")

	// Static variables are still resolved through the compiler fast path.
	assert.Equal(t, "world", report.StaticVars["NAME"])
	assert.Equal(t, "hello world", report.StaticVars["GREETING"])
	_, hasSentinel := report.StaticVars["SENTINEL"]
	assert.False(t, hasSentinel, "unresolved dynamic variables must be omitted")
}

func TestPreviewV3ProducesNoChanges(t *testing.T) {
	t.Parallel()

	src := `version: '3'
tasks:
  build:
    cmds:
      - echo build
`
	dir := t.TempDir()
	writeFile(t, dir, "Taskfile.yml", src)

	report, err := NewPreviewer().Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)

	require.Len(t, report.Files, 1)
	assert.False(t, report.HasChanges())
	assert.False(t, report.HasWarnings())
	assert.False(t, report.HasManualActions())
	assert.False(t, report.ValidationSkipped)
}

func TestPreviewDetectsLegacyTaskvars(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "Taskfile.yml", `version: '2'
tasks:
  default: echo hi
`)
	writeFile(t, dir, "Taskvars.yml", "FOO: bar\n")

	report, err := NewPreviewer().Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)

	root := report.Files[0]
	assert.True(t, hasCode(root.ManualActions, codeTaskvarsAutoload))
}

func TestPreviewNestedIncludesWarning(t *testing.T) {
	t.Parallel()

	root := `version: '2'

includes:
  sub: ./sub/Taskfile.yml

tasks:
  default:
    cmds:
      - task: sub:lib:hello
`
	sub := `version: '2'
includes:
  lib: ./lib/Taskfile.yml
tasks:
  from-sub: echo sub
`
	lib := `version: '3'
tasks:
  hello: echo hello
`
	dir := t.TempDir()
	rootPath := writeFile(t, dir, "Taskfile.yml", root)
	writeFile(t, dir, "sub/Taskfile.yml", sub)
	writeFile(t, dir, "sub/lib/Taskfile.yml", lib)

	report, err := NewPreviewer().Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)

	rootReport := report.File(rootPath)
	require.NotNil(t, rootReport)
	require.True(t, hasCode(rootReport.Warnings, codeNestedIncludes),
		"nested include warning expected; got %#v", rootReport.Warnings)
	var nested Warning
	for _, w := range rootReport.Warnings {
		if w.Code == codeNestedIncludes {
			nested = w
		}
	}
	assert.Equal(t, 4, nested.Line, "warning must point at the include source line")
	assert.False(t, report.ValidationSkipped, "migrated nested include graph must merge and validate")
}

func TestPreviewFailureRollsBack(t *testing.T) {
	t.Parallel()

	root := `version: '2'
expansions: 3
includes:
  sub: ./sub/Taskfile.yml
tasks:
  boom:
    cmds:
      - echo boom
`
	sub := `version: '2'
tasks:
  hi: echo hi
`
	dir := t.TempDir()
	rootPath := writeFile(t, dir, "Taskfile.yml", root)
	subPath := writeFile(t, dir, "sub/Taskfile.yml", sub)
	before := snapshotTree(t, dir)

	failingRule := Rule(func(c *RuleContext) error {
		_, tasksNode := findPair(c.Root, "tasks")
		tasksNode = resolve(tasksNode)
		if key, _ := findPair(tasksNode, "boom"); key != nil {
			return assert.AnError
		}
		return nil
	})

	report, err := NewPreviewer(WithRule(failingRule)).
		Preview(context.Background(), rootNode(t, dir))
	require.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, report, "no report must be returned on failure")

	// Everything on disk stays byte-identical.
	assert.Equal(t, before, snapshotTree(t, dir))

	// Disk content is still the original v2 input.
	content, err := os.ReadFile(rootPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "version: '2'")
	assert.Contains(t, string(content), "expansions: 3")
	subContent, err := os.ReadFile(subPath)
	require.NoError(t, err)
	assert.Contains(t, string(subContent), "version: '2'")

	// A follow-up preview without the failing rule succeeds: no state was
	// mutated by the failed run.
	retry, err := NewPreviewer().Preview(context.Background(), rootNode(t, dir))
	require.NoError(t, err)
	assert.True(t, retry.HasChanges())
}
