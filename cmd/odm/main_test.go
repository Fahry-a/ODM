package main

import (
	"testing"

	"odm/internal/config"
)

// TestBatchChecksumWarning pins the batch-mode --checksum drop: a single hash
// cannot cover multiple files, so the value is cleared for multi-URL runs and
// the user gets a stderr warning — except in --quiet sessions, which must stay
// clean for cron/scripts. The warning lives in batchChecksumWarning (a pure
// function of *config.Options) so this branch is testable without driving run()
// into a real network download.
func TestBatchChecksumWarning(t *testing.T) {
	const want = "warning: --checksum ignored when downloading multiple URLs (one hash cannot cover multiple files)"

	cases := []struct {
		name     string
		urls     []string
		checksum string
		quiet    bool
		want     string
	}{
		{name: "batch with checksum warns", urls: []string{"https://a/x", "https://b/y"}, checksum: "sha256:deadbeef", want: want},
		{name: "batch without checksum stays silent", urls: []string{"https://a/x", "https://b/y"}, want: ""},
		{name: "single URL with checksum stays silent", urls: []string{"https://a/x"}, checksum: "sha256:deadbeef", want: ""},
		{name: "quiet batch suppresses warning", urls: []string{"https://a/x", "https://b/y"}, checksum: "sha256:deadbeef", quiet: true, want: ""},
		{name: "empty URL list stays silent", urls: nil, checksum: "sha256:deadbeef", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &config.Options{URLs: tc.urls, Checksum: tc.checksum, Quiet: tc.quiet}
			if got := batchChecksumWarning(o); got != tc.want {
				t.Fatalf("batchChecksumWarning = %q, want %q", got, tc.want)
			}
		})
	}
}
