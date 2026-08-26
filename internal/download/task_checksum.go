package download

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"odm/internal/transport"
)

// fetchChecksumSpec downloads the sidecar at opts.ChecksumURL and parses it
// into an "algo:hex" spec. Accepted forms: "algo:<hash>", "<64-hex>" (sha256),
// "<40-hex>" (sha1), "<32-hex>" (md5), optionally followed by whitespace and
// the filename (sha256sum output format).
func (t *Task) fetchChecksumSpec(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := t.client.NewGetRequest(ctx, t.opts.ChecksumURL)
	if err != nil {
		return "", err
	}
	resp, err := t.client.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer transport.SkipBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: status %d", t.opts.ChecksumURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return parseChecksumSidecar(string(body))
}
func parseChecksumSidecar(s string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	tok := fields[0]
	algo, hexStr, ok := strings.Cut(tok, ":")
	if !ok {
		switch len(tok) {
		case 64:
			algo, hexStr = "sha256", tok
		case 40:
			algo, hexStr = "sha1", tok
		case 32:
			algo, hexStr = "md5", tok
		default:
			return "", fmt.Errorf("unrecognised digest form %q", tok)
		}
	}
	algo = strings.ToLower(algo)
	switch algo {
	case "md5", "sha1", "sha256":
	default:
		return "", fmt.Errorf("unsupported algorithm %q", algo)
	}
	return algo + ":" + strings.ToLower(hexStr), nil
}

// verifyChecksum runs the --checksum verification against the real output file
// (t.outPath — the name the server chose via Content-Disposition or the -o
// override, not a URL-derived guess). No-op when no checksum was requested.
// verifyChecksum runs the --checksum verification against the real output file
// (t.outPath — the name the server chose via Content-Disposition or the -o
// override, not a URL-derived guess). No-op when no checksum was requested.
func (t *Task) verifyChecksum() error {
	if t.opts.Checksum == "" {
		return nil
	}
	algo, hexStr, ok := strings.Cut(t.opts.Checksum, ":")
	if !ok || hexStr == "" {
		return fmt.Errorf("checksum: bad spec %q", t.opts.Checksum)
	}
	return verifyChecksum(t.outPath, algo, hexStr)
}

// finish flushes and persist/removes the control file; an error from the
// caller already set the state.
