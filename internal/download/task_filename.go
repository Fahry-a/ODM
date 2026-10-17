package download

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// uniqueName returns dir/name rewritten to base.N.ext with the lowest N≥1
// that doesn't exist yet ("f.tar.gz" → "f.1.tar.gz"): the counter goes before
// filepath.Ext's last extension, so compound extensions stay readable.
func uniqueName(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); err != nil {
			return candidate
		}
	}
}

// sizeOrUnknown renders size for log lines: "?" when unknown (<0).
func sizeOrUnknown(size int64) string {
	if size < 0 {
		return "?"
	}
	return strconv.FormatInt(size, 10)
}

// formatSegSize renders a segment byte count compactly for log lines
// (MiB/GiB…). Kept local — the UI package's formatter lives in internal/ui.
func formatSegSize(b int64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	val := float64(b)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	idx := -1
	for val >= unit && idx < len(units)-1 {
		val /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", val, units[idx])
}

// flattenFilename clamps a server-controlled filename (Content-Disposition or
// URL basename) to a single path component: no separators, no dot segments, no
// escape from Dir via filepath.Join. An explicit -o override is NOT passed
// through here — the user chose that name themselves.
//
//   - separators '/' and '\\' are replaced so "a/b" and "a\\b" stay inside Dir
//     (Windows-style separators matter when the same name lands on a Windows
//     share/FS later);
//   - "." and ".." collapse to nothing, so Join(dir, "..") can never escape;
//   - empty results fall back to download.bin.
func flattenFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == os.PathSeparator || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	// A trailing "..foo" is a valid filename; only exact dot segments are
	// dangerous. After separator flattening no path element boundary remains,
	// so Base() is belt-and-braces for exotic FS edge cases.
	if base := filepath.Base(name); base != name && base != "." && base != ".." {
		name = base
	}
	return name
}

// deriveFilename picks an output name from the URL path, or falls back to the
// --output override / "download.bin". Both sources are server-controlled
// (Content-Disposition feeds pr.Filename; the URL path feeds the basename), so
// the result is flattened to a single path component before it can reach
// filepath.Join — see flattenFilename.
func deriveFilename(finalURL, override string) string {
	if override != "" {
		return override
	}
	u := finalURL
	if i := strings.LastIndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		name := u[i+1:]
		if name != "" {
			return flattenFilename(name)
		}
	}
	return "download.bin"
}
