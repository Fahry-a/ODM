// Package update handles self-update and version-check logic for the odm binary.
// It detects the install method (AUR, self-installed via install.sh, or manual)
// and dispatches to the appropriate update path.
package update

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	repoOwner       = "Fahry-a"
	repoName        = "odm"
	githubAPI       = "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/releases/latest"
	hintCacheFile   = ".last-update-check"
	hintCacheDir    = ".config/odm"
	hintCacheMaxAge = 24 * time.Hour
	httpTimeout     = 5 * time.Second
)

// CheckLatest fetches the latest release version from GitHub API.
// Returns the bare version string (e.g. "1.8.0") and the tarball download URL.
func CheckLatest() (ver string, downloadURL string, err error) {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(githubAPI)
	if err != nil {
		return "", "", fmt.Errorf("GitHub API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("GitHub API: status %d", resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", fmt.Errorf("GitHub API: %w", err)
	}
	ver = strings.TrimPrefix(release.TagName, "v")
	if ver == "" {
		return "", "", fmt.Errorf("GitHub API: empty tag_name")
	}
	downloadURL = fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/odm_%s_%s_%s.tar.gz",
		repoOwner, repoName, ver, ver, runtime.GOOS, goarch())
	return ver, downloadURL, nil
}

// CompareVersions compares two semver strings (major.minor.patch).
// Returns -1 if a < b, 0 if equal, 1 if a > b.
func CompareVersions(a, b string) int {
	av, bv := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	return 0
}

func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "odm/")
	var parts [3]int
	for i, p := range strings.SplitN(s, ".", 3) {
		parts[i], _ = strconv.Atoi(p)
	}
	return parts
}

func goarch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	case "386":
		return "386"
	default:
		return runtime.GOARCH
	}
}

// InstallMethod describes how ODM was installed.
type InstallMethod int

const (
	// MethodSelf means installed via install.sh (~/.local/bin or /usr/local/bin).
	MethodSelf InstallMethod = iota
	// MethodAUR means installed via an AUR helper (yay/paru).
	MethodAUR
	// MethodManual means installed by the user manually (unknown path).
	MethodManual
)

func (m InstallMethod) String() string {
	switch m {
	case MethodSelf:
		return "self"
	case MethodAUR:
		return "aur"
	case MethodManual:
		return "manual"
	}
	return "unknown"
}

// DetectInstallMethod returns how the running binary was installed.
func DetectInstallMethod() InstallMethod {
	// Check AUR: pacman -Qi odm-bin succeeds
	if isAUR() {
		return MethodAUR
	}
	// Check self-installed: binary lives in well-known install.sh locations
	exe, err := os.Executable()
	if err != nil {
		return MethodManual
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	home, _ := os.UserHomeDir()
	selfDirs := []string{
		filepath.Join(home, ".local", "bin"),
		"/usr/local/bin",
	}
	for _, d := range selfDirs {
		if filepath.Clean(dir) == filepath.Clean(d) {
			return MethodSelf
		}
	}
	return MethodManual
}

func isAUR() bool {
	// pacman -Qi exits 0 if the package is installed
	cmd := exec.Command("pacman", "-Qi", "odm-bin")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// Run executes the self-update for the detected install method.
func Run(currentVersion string) error {
	method := DetectInstallMethod()
	latestVer, downloadURL, err := CheckLatest()
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	if CompareVersions(currentVersion, latestVer) >= 0 {
		fmt.Printf("already up to date (%s)\n", currentVersion)
		return nil
	}

	fmt.Printf("updating odm %s → %s\n", currentVersion, latestVer)

	switch method {
	case MethodAUR:
		return runAUR()
	case MethodSelf:
		return runSelf(latestVer, downloadURL)
	case MethodManual:
		return runManual(latestVer, downloadURL)
	}
	return nil
}

func runAUR() error {
	// detect which AUR helper is available
	var helper string
	for _, h := range []string{"yay", "paru"} {
		if _, err := exec.LookPath(h); err == nil {
			helper = h
			break
		}
	}
	if helper == "" {
		fmt.Println("no AUR helper found (yay/paru). Install one or update manually:")
		fmt.Println("  yay -S odm-bin")
		return nil
	}
	fmt.Printf("running: %s -S odm-bin\n", helper)
	cmd := exec.Command(helper, "-S", "odm-bin")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runSelf(latestVer, downloadURL string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find binary path: %w", err)
	}
	exe, _ = filepath.EvalSymlinks(exe)

	tmpDir, err := os.MkdirTemp("", "odm-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tarball := filepath.Join(tmpDir, "odm.tar.gz")

	// download
	fmt.Println("downloading...")
	if err := downloadFile(downloadURL, tarball); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// verify checksum
	checksumURL := strings.Replace(downloadURL, "odm_"+latestVer, "checksums", 1)
	checksumURL = checksumURL[:strings.LastIndex(checksumURL, "_")+1] + "checksums.txt"
	// simpler: reconstruct from release URL pattern
	checksumURL = fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/checksums.txt",
		repoOwner, repoName, latestVer)
	if err := verifyChecksum(tarball, checksumURL); err != nil {
		fmt.Printf("checksum warning: %v\n", err)
	}

	// extract
	fmt.Println("extracting...")
	if err := extractTarball(tarball, tmpDir); err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}

	newBin := filepath.Join(tmpDir, "odm")
	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("binary 'odm' not found in tarball")
	}

	// replace current binary
	fmt.Println("replacing binary...")
	if err := replaceBinary(exe, newBin); err != nil {
		return fmt.Errorf("replace failed: %w (you may need to run with sudo)", err)
	}

	fmt.Printf("odm updated to %s\n", latestVer)
	return nil
}

func runManual(latestVer, downloadURL string) error {
	fmt.Printf("new version available: %s\n", latestVer)
	fmt.Println()
	fmt.Println("download from:")
	fmt.Printf("  %s\n", downloadURL)
	fmt.Println()
	fmt.Println("or use the install script:")
	fmt.Println("  curl -fsSL https://odm.orynix.id/install | sh")
	return nil
}

func replaceBinary(dst, src string) error {
	// try atomic rename first (works on same filesystem)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// fallback: copy + chmod
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func verifyChecksum(tarball, checksumURL string) error {
	resp, err := http.Get(checksumURL)
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("checksums.txt: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// compute sha256 of tarball
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", h.Sum(nil))

	// find matching line in checksums.txt
	basename := filepath.Base(tarball)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, basename) {
			parts := strings.Fields(line)
			if len(parts) >= 1 && parts[0] == actual {
				return nil // match
			}
			return fmt.Errorf("checksum mismatch: got %s, want %s", actual, parts[0])
		}
	}
	return nil // no matching line, skip
}

func extractTarball(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(hdr.Name)
		// only extract the binary and LICENSE
		if name != "odm" && name != "LICENSE" {
			continue
		}
		outPath := filepath.Join(dest, name)
		out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
	return nil
}

// Hint returns a version-hint string if a newer release exists, or "" if
// up-to-date or the check is throttled. The result is cached for 24h.
func Hint(currentVersion string) string {
	// check cache
	cachePath := hintCachePath()
	if data, err := os.ReadFile(cachePath); err == nil {
		lines := strings.SplitN(string(data), "\n", 2)
		if len(lines) == 2 {
			ts, _ := strconv.ParseInt(lines[0], 10, 64)
			cachedVer := lines[1]
			if time.Now().Unix()-ts < int64(hintCacheMaxAge.Seconds()) {
				latestVer := cachedVer
				if CompareVersions(currentVersion, latestVer) < 0 {
					return fmt.Sprintf("→ update available: v%s (odm update)", latestVer)
				}
				return ""
			}
		}
	}

	// fetch latest (with short timeout)
	latestVer, _, err := CheckLatest()
	if err != nil {
		return ""
	}

	// write cache
	if cachePath != "" {
		os.MkdirAll(filepath.Dir(cachePath), 0755)
		content := fmt.Sprintf("%d\n%s", time.Now().Unix(), latestVer)
		os.WriteFile(cachePath, []byte(content), 0644)
	}

	if CompareVersions(currentVersion, latestVer) < 0 {
		return fmt.Sprintf("→ update available: v%s (odm update)", latestVer)
	}
	return ""
}

func hintCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, hintCacheDir, hintCacheFile)
}
