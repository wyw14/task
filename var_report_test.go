package task_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
)

// TestVariableReportStaticNoExecution asserts that generating the variable
// report neither executes tasks nor evaluates "sh:" variables, while still
// producing definition/reference information through the compiler's static
// (fast) variable path.
func TestVariableReportStaticNoExecution(t *testing.T) {
	dir := filepath.Join("internal", "varreport", "testdata", "template_nesting")
	marker := filepath.Join(dir, "task-sh-marker.txt")
	require.NoError(t, os.RemoveAll(marker))

	report, err := task.GenerateVariableReport(context.Background(), dir, "")
	require.NoError(t, err)

	// The MARKER sh variable must never have run.
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "dynamic sh variables must not be evaluated")

	// Compiler static preview: plain static variables resolve, dynamic ones
	// are flagged instead of executed.
	assert.Equal(t, "world", report.Resolved["NAME"])
	assert.Contains(t, report.Resolved["NOW"], "not evaluated")

	now := report.Find("NOW")
	require.NotNil(t, now)
	assert.True(t, now.Dynamic)

	// A text report renders without running anything.
	var text bytes.Buffer
	require.NoError(t, task.RunVariableReport(context.Background(), dir, "", &text, false))
	assert.Contains(t, text.String(), "Variable: MESSAGE")
	assert.Contains(t, text.String(), "shell-evaluated: yes")
	assert.Contains(t, text.String(), "Root:")

	// JSON output is valid JSON.
	var js bytes.Buffer
	require.NoError(t, task.RunVariableReport(context.Background(), dir, "", &js, true))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(js.Bytes(), &decoded))
	assert.Equal(t, decoded["rootUri"], report.RootURI)
}

// TestVariableReportMultiIncludeResolved checks the merged static preview
// across a multi-file include graph and that nested task names are reported.
func TestVariableReportMultiIncludeResolved(t *testing.T) {
	dir := filepath.Join("internal", "varreport", "testdata", "multi_include")

	report, err := task.GenerateVariableReport(context.Background(), dir, "")
	require.NoError(t, err)

	assert.Equal(t, "root-value", report.Resolved["ROOT_VAR"])
	assert.Equal(t, "lib-value", report.Resolved["LIB_VAR"])

	lib := report.Find("LIB_VAR")
	require.NotNil(t, lib)
	assert.True(t, lib.UsedByTask("lib:build"))
}

// TestVariableReportRemoteRootRejected ensures static analysis never attempts
// to download a remote root Taskfile.
func TestVariableReportRemoteRootRejected(t *testing.T) {
	_, err := task.GenerateVariableReport(
		context.Background(),
		"",
		"https://example.invalid/Taskfile.yml",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote Taskfiles")
}
