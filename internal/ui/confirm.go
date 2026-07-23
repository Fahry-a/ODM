package ui

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"odm/internal/scheduler"
)

// ConfirmAsk reads a Y/n answer from stdin (used by the §9 prompt).
// `in` and `out` are injected so the prompt is testable.
func ConfirmAsk(in io.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return false, err
		}
		return false, io.EOF
	}
	ans := strings.TrimSpace(strings.ToLower(sc.Text()))
	switch ans {
	case "", "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return false, fmt.Errorf("unrecognized answer %q", ans)
}

// ConfirmSingle renders the §9 single-file prompt and asks for confirmation.
func ConfirmSingle(in io.Reader, out io.Writer, filename, dest string, size int64, conns int) (bool, error) {
	fmt.Fprintln(out, "File       :", filename)
	fmt.Fprintln(out, "Size       :", FormatFileSize(size))
	fmt.Fprintln(out, "Connections:", conns, "parallel")
	fmt.Fprintln(out, "Destination:", dest)
	fmt.Fprintln(out)
	return ConfirmAsk(in, out, "Continue? [Y/n] ")
}

// ConfirmBatch renders the §9 batch prompt. `rows` are the per-file summaries
// (name, size). `connsPerFile` and `parallelFiles` describe the allocation.
func ConfirmBatch(in io.Reader, out io.Writer, rows []FileRow, connsPerFile, parallelFiles, totalFiles int) (bool, error) {
	var totalSize int64
	for _, r := range rows {
		totalSize += r.Size
	}
	fmt.Fprintf(out, "ODM will download %d files (total ~%s)\n", totalFiles, FormatFileSize(totalSize))
	if connsPerFile <= 1 {
		fmt.Fprintf(out, "Allocation: 1 connection/file, %d files running in parallel (rest queued automatically)\n", parallelFiles)
	} else {
		fmt.Fprintf(out, "Allocation: %d connections/file, %d files running in parallel (rest queued automatically)\n", connsPerFile, parallelFiles)
	}
	fmt.Fprintln(out)
	for i, r := range rows {
		fmt.Fprintf(out, "  [%d] %-26s %s\n", i+1, r.Name, FormatFileSize(r.Size))
	}
	fmt.Fprintln(out)
	return ConfirmAsk(in, out, "Continue? [Y/n] ")
}

// FileRow is one line of the batch prompt's file list.
type FileRow struct {
	Name string
	Size int64
}

// RowsFromPlan builds FileRows for the batch prompt from a Balancer plan. Since
// the plan doesn't carry per-file sizes (the probe happens later), callers pass
// the probed size map keyed by URL; missing entries report -1 (unknown).
func RowsFromPlan(plan *scheduler.Plan, sizes map[string]int64) []FileRow {
	out := make([]FileRow, 0, len(plan.Parallel)+len(plan.Queued))
	all := append(append([]scheduler.Allocation{}, plan.Parallel...), plan.Queued...)
	for _, a := range all {
		out = append(out, FileRow{Name: shortName(a.URL), Size: sizes[a.URL]})
	}
	return out
}

// shortName trims a URL down to its basename for the prompt list.
func shortName(u string) string {
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		return u[i+1:]
	}
	return u
}
