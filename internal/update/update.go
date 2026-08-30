// Package update handles self-update and version-check logic for the odm binary.
// It detects the install method (AUR, self-installed via install.sh, or manual)
// and dispatches to the appropriate update path.
package update

import (
	"encoding/json"
	"fmt"
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
	hintCacheMaxAge  = 24 * time.Hour
	installScriptURL = "https://odm.orynix.id/install.sh"
)

// CheckLatest fetches the latest release version from GitHub API.
// Returns the bare version string (e.g. "1.8.0") and the tarball download URL.
// Uses curl to avoid TLS/DNS issues with Go's pure-Go resolver on Android/Termux.
func CheckLatest() (ver string, downloadURL string, err error) {
	cmd := exec.Command("curl", "-fsSL", githubAPI)
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("GitHub API (curl): %w", err)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(out, &release); err != nil {
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
	// Check if pacman exists first — exec.LookPath can SIGSYS on Android/Termux
	// because faccessat(AT_SYMLINK_NOFOLLOW) is seccomp-blocked. Stat the
	// common locations instead; if none exist, pacman isn't here anyway.
	hasPacman := false
	for _, p := range []string{"/usr/bin/pacman", "/usr/local/bin/pacman"} {
		if _, err := os.Stat(p); err == nil {
			hasPacman = true
			break
		}
	}
	if !hasPacman {
		return false
	}
	cmd := exec.Command("pacman", "-Qi", "odm-bin")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// Run executes the self-update for the detected install method.
func Run(currentVersion string) error {
	method := DetectInstallMethod()
	latestVer, _, err := CheckLatest()
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
		return runInstallScript()
	case MethodManual:
		return runManual(latestVer)
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

func runInstallScript() error {
	// Resolve temp dir: prefer $TMPDIR (set by Termux), then os.TempDir(),
	// then fall back to ~/.odm-tmp. Termux's /tmp is a symlink that may not
	// exist or be writable.
	tmpDir := os.Getenv("TMPDIR")
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		home, _ := os.UserHomeDir()
		tmpDir = filepath.Join(home, ".odm-tmp")
		_ = os.MkdirAll(tmpDir, 0700)
	}

	tmpFile, err := os.CreateTemp(tmpDir, "odm-install-*.sh")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Download install.sh via curl (uses system DNS/certs, works on all platforms).
	fmt.Println("downloading install script...")
	cmd := exec.Command("curl", "-fsSL", "-o", tmpPath, installScriptURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("downloading install script: %w", err)
	}

	// Execute install.sh --update with ODM_UPDATE=1 for env var fallback.
	// Connect stdin/stdout/stderr so the user sees output and can interact
	// with any prompts (e.g., sudo password).
	cmd = exec.Command("bash", tmpPath, "--update")
	cmd.Env = append(os.Environ(), "ODM_UPDATE=1")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install script failed: %w", err)
	}
	return nil
}

func runManual(latestVer string) error {
	fmt.Printf("new version available: %s\n", latestVer)
	return runInstallScript()
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

	// write cache (best-effort)
	if cachePath != "" {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err == nil {
			content := fmt.Sprintf("%d\n%s", time.Now().Unix(), latestVer)
			_ = os.WriteFile(cachePath, []byte(content), 0644)
		}
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
