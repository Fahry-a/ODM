package ui

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"odm/internal/scheduler"
)

// ConfirmAsk reads a Y/n answer from stdin (used by the prompt).
// in and out are injected so the prompt is testable.
//
// On a typo ("sure", stray spaces, etc.) it re-prompts instead of erroring and
// aborting the whole batch (bug): an empty answer or y/yes confirms, n/no
// declines. End-of-file (Ctrl-D) with no answer at all is treated as a silent
// cancel — (false, nil) — so a non-interactive stdin ending the stream never
// surfaces a Go-level EOF as a raw error string. A genuine read error other
// than EOF is still returned so a broken pipe propagates.
func ConfirmAsk(in io.Reader, out io.Writer, prompt string) (bool, error) {
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, prompt)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return false, err
			}
			// EOF reached with no more tokens: silent cancel rather than a raw
			// io.EOF surfacing as "odm: EOF" to the user.
			return false, nil
		}
		switch strings.TrimSpace(strings.ToLower(sc.Text())) {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		// Unrecognised: loop and re-ask. A short nudge so the re-prompt makes
		// the expectation obvious without a noisy Go error.
		fmt.Fprintln(out, "Please answer [Y/n] or press Ctrl-D to cancel.")
	}
}

// ConfirmSingle renders the single-file prompt and asks for confirmation.
// profile/reason show the resolved engine ("smart" mode resolves it per file,
// mirroring the batch prompt's per-file tag); empty renders no Engine row.
// When useColor is true, labels are yellow, the filename is cyan, size is
// green, and the "Continue?" prompt is bold yellow.
func ConfirmSingle(in io.Reader, out io.Writer, filename, dest string, size int64, conns int, profile, reason string, useColor bool) (bool, error) {
	if useColor {
		fmt.Fprintf(out, "  %sFile       :%s %s%s%s\n", colorYellow, colorReset, colorCyan, filename, colorReset)
		fmt.Fprintf(out, "  %sSize       :%s %s%s%s\n", colorYellow, colorReset, colorGreen, FormatFileSize(size), colorReset)
		fmt.Fprintf(out, "  %sConnections:%s %s%d parallel%s\n", colorYellow, colorReset, colorMagenta, conns, colorReset)
		if p := profileSuffix(profile, reason); p != "" {
			engine := strings.TrimPrefix(p, " ")
			fmt.Fprintf(out, "  %sEngine     :%s %s%s%s\n", colorYellow, colorReset, colorCyan, engine, colorReset)
		}
		fmt.Fprintf(out, "  %sDestination:%s %s\n", colorYellow, colorReset, dest)
		fmt.Fprintln(out)
		return ConfirmAsk(in, out, fmt.Sprintf("%sContinue?%s [Y/n] ", colorYellow, colorReset))
	}
	fmt.Fprintln(out, "File       :", filename)
	fmt.Fprintln(out, "Size       :", FormatFileSize(size))
	fmt.Fprintln(out, "Connections:", conns, "parallel")
	if p := profileSuffix(profile, reason); p != "" {
		fmt.Fprintln(out, "Engine     :", strings.TrimPrefix(p, " "))
	}
	fmt.Fprintln(out, "Destination:", dest)
	fmt.Fprintln(out)
	return ConfirmAsk(in, out, "Continue? [Y/n] ")
}

// ConfirmBatch renders the batch prompt. `rows` are the per-file summaries
// (name, size). `connsPerFile` and `parallelFiles` describe the allocation.
func ConfirmBatch(in io.Reader, out io.Writer, rows []FileRow, connsPerFile, parallelFiles, totalFiles int, useColor bool) (bool, error) {
	var totalSize int64
	for _, r := range rows {
		totalSize += r.Size
	}
	if useColor {
		fmt.Fprintf(out, "  %sODM will download%s %s%d files%s (total ~%s%s%s)\n",
			colorGreen, colorReset,
			colorYellow, totalFiles, colorReset,
			colorCyan, FormatFileSize(totalSize), colorReset)
		if connsPerFile <= 1 {
			fmt.Fprintf(out, "  %sAllocation:%s 1 connection/file, %s%d files%s running in parallel (rest queued)\n",
				colorYellow, colorReset, colorMagenta, parallelFiles, colorReset)
		} else {
			fmt.Fprintf(out, "  %sAllocation:%s %s%d connections/file%s, %s%d files%s running in parallel (rest queued)\n",
				colorYellow, colorReset,
				colorMagenta, connsPerFile, colorReset,
				colorMagenta, parallelFiles, colorReset)
		}
		fmt.Fprintln(out)
		for i, r := range rows {
			sizeStr := FormatFileSize(r.Size)
			// Right-align the sizes so the column reads like a table, and pad
			// the name to a fixed width so the sizes line up regardless of how
			// long the filename is.
			fmt.Fprintf(out, "    %s[%d]%s %s %s%s%s%s\n",
				colorGreen, i+1, colorReset,
				padToCells(truncateNameTo(r.Name, 40), 40),
				colorCyan, strings.Repeat(" ", 14-displayWidth(sizeStr))+sizeStr, colorReset,
				profileSuffix(r.Profile, r.Reason))
		}
		fmt.Fprintln(out)
		return ConfirmAsk(in, out, fmt.Sprintf("%sContinue?%s [Y/n] ", colorYellow, colorReset))
	}
	fmt.Fprintf(out, "ODM will download %d files (total ~%s)\n", totalFiles, FormatFileSize(totalSize))
	if connsPerFile <= 1 {
		fmt.Fprintf(out, "Allocation: 1 connection/file, %d files running in parallel (rest queued automatically)\n", parallelFiles)
	} else {
		fmt.Fprintf(out, "Allocation: %d connections/file, %d files running in parallel (rest queued automatically)\n", connsPerFile, parallelFiles)
	}
	fmt.Fprintln(out)
	for i, r := range rows {
		sizeStr := FormatFileSize(r.Size)
		fmt.Fprintf(out, "  [%d] %s %s%s\n",
			i+1,
			padToCells(truncateNameTo(r.Name, 40), 40),
			strings.Repeat(" ", 14-displayWidth(sizeStr))+sizeStr,
			profileSuffix(r.Profile, r.Reason))
	}
	fmt.Fprintln(out)
	return ConfirmAsk(in, out, "Continue? [Y/n] ")
}

// FileRow is one line of the batch prompt's file list.
type FileRow struct {
	Name    string
	Size    int64
	Profile string // engine profile (odm/aria2c/both); "" = not known yet
	Reason  string // why smart chose this profile ("" = none)
}

// RowsFromPlan builds FileRows for the batch prompt from a Balancer plan. Since
// the plan doesn't carry per-file sizes (the probe happens later), callers pass
// the probed size map keyed by URL; missing entries report -1 (unknown).
// profiles (keyed by URL) carries the concrete engine per file — for the smart
// profile it's the decision made after probing, shown so the user sees which
// engine each file will actually use. reasons (keyed by URL) carries
// ChooseProfile's why, so the prompt can explain the decision. showProfiles
// gates the suffix: true only for smart, where the resolved engine is
// informative even when it's "odm".
func RowsFromPlan(plan *scheduler.Plan, sizes map[string]int64, profiles map[string]string, reasons map[string]string, showProfiles bool) []FileRow {
	out := make([]FileRow, 0, len(plan.Parallel)+len(plan.Queued))
	all := append(append([]scheduler.Allocation{}, plan.Parallel...), plan.Queued...)
	for _, a := range all {
		p := profiles[a.URL]
		if !showProfiles {
			p = "" // explicit profiles: no per-file tag
		}
		out = append(out, FileRow{
			Name:    shortName(a.URL),
			Size:    sizes[a.URL],
			Profile: p,
			Reason:  reasons[a.URL],
		})
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

// profileSuffix renders the per-file engine tag shown after the size in the
// batch prompt, e.g. " [aria2c]" or " [odm — no h2]". Empty (no profile
// resolved yet, or explicit-profile mode where the tag is noise) renders
// nothing. reason appends the smart decision's why so the user sees *why* a
// file landed on a given engine.
func profileSuffix(p, reason string) string {
	if p == "" {
		return ""
	}
	if reason != "" {
		return " [" + p + " — " + reason + "]"
	}
	return " [" + p + "]"
}
