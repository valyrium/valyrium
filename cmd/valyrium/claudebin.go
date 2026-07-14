package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// resolveClaudeBin turns the configured claude-CLI reference into a concrete,
// runnable path, or fails with an error that names what went wrong.
//
// The launchd/systemd service that runs the gateway starts with a minimal
// PATH that usually omits wherever the user installed `claude`, so a bare
// "claude" that resolves fine in an interactive shell fails to spawn once the
// service takes over — and today that surfaces only on the first chat request,
// as an opaque spawn error, while /healthz keeps reporting healthy. Resolving
// at startup instead — preferring PATH, then probing the usual install
// locations, and returning an absolute path — moves that failure to boot,
// where it can say exactly where it looked and how to fix it.
func resolveClaudeBin(bin string) (string, error) {
	// An explicit path (one containing a separator) is taken at its word: the
	// user pointed us somewhere specific, so honour it and only complain if it
	// isn't actually a runnable file.
	if strings.ContainsRune(bin, os.PathSeparator) {
		if isExecutable(bin) {
			return bin, nil
		}
		return "", fmt.Errorf("configured claude CLI %q is not an executable file", bin)
	}

	// A bare name: prefer PATH, exactly as exec.Command would have.
	if path, err := exec.LookPath(bin); err == nil {
		return path, nil
	}

	// Not on PATH — probe the common install locations a service's stripped-down
	// PATH tends to miss.
	dirs := candidateBinDirs()
	for _, dir := range dirs {
		candidate := filepath.Join(dir, bin)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf(
		"claude CLI %q not found on PATH or in {%s}; install it or set CLAUDE_GATEWAY_BIN to its full path",
		bin, strings.Join(dirs, ", "))
}

// candidateBinDirs lists the directories probed for the claude CLI when it is
// not on PATH. Keep this in sync with the service block's PATH in
// .goreleaser.yml so the two agree on where claude might live.
func candidateBinDirs() []string {
	dirs := make([]string, 0, 4)
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".claude", "local"),
		)
	}
	return append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
}

// isExecutable reports whether path is a regular file the process may run. The
// executable-bit check is meaningful only on Unix; on Windows, runnability is
// decided by the extension, so existence as a non-directory is the best signal
// we have here (exec.LookPath already handles the .exe search on PATH).
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
