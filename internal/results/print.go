package results

import (
	"strings"

	"github.com/Ladicle/tabwriter"

	"github.com/go-task/task/v3/internal/logger"
)

// PrintReport renders a run result summary via the logger. It is opt-in and
// only ever called explicitly after a run: it does not share any output path
// with regular task logging, so querying results cannot change the existing
// log format.
func PrintReport(l *logger.Logger, report Report) {
	if l == nil {
		return
	}

	w := tabwriter.NewWriter(l.Stdout, 0, 8, 2, ' ', 0)
	l.SummaryOutf(logger.Cyan, "task: Run result summary:\n")
	l.FOutf(w, logger.Default, "task: \tTASK\tSTATUS\tEXIT\tRETRIES\tCACHE\tDEPENDENCIES\n")
	for _, result := range report.Tasks {
		l.FOutf(w, logger.Default, "task: \t")
		l.FOutf(w, logger.Green, "%s\t", result.Name)
		l.FOutf(w, statusColor(result.Status), "%s\t", result.Status)
		l.FOutf(w, logger.Default, "%d\t%d\t%s\t%s\n",
			result.ExitCode,
			result.Retries,
			result.CacheSource,
			formatDeps(result.Deps),
		)
	}
	if report.Origin.Location != "" {
		l.FOutf(w, logger.Default, "task: taskfile: %s (%s)\n", report.Origin.Location, report.Origin.Source)
	}
	_ = w.Flush()
}

func statusColor(status Status) logger.Color {
	switch status {
	case StatusSucceeded, StatusUpToDate:
		return logger.Green
	case StatusSkipped:
		return logger.Yellow
	case StatusFailed, StatusCanceled:
		return logger.Red
	default:
		return logger.Default
	}
}

func formatDeps(deps []DepSummary) string {
	if len(deps) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(deps))
	for _, dep := range deps {
		parts = append(parts, dep.Name+"="+string(dep.Status))
	}
	return strings.Join(parts, ", ")
}
