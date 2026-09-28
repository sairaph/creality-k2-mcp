// Package daemon is the background camera/idle-heat daemon
// (dev_docs/plan-v0.1.0.md T11a, dev_docs/safety-architecture.md section 10
// D2): a single per-user process, reached over a local socket, that owns
// two entirely separate components: a camera Hub (one upstream
// camera.Session per printer, fanned out to any number of consumers) and
// an idle-heat Watchdog (D2's automatic turn-off after idle_heat_minutes).
// The two share nothing but the process and the idle self-exit check; the
// Watchdog is deliberately not built on top of camera code paths, per D2.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// appDirName matches internal/domain's own ~/.creality_k2_mcp, kept as a
// separate literal here (rather than importing internal/domain) so this
// package's only path dependency is the OS home directory.
const appDirName = ".creality_k2_mcp"

// daemonSubdir is where the daemon's own lock, socket, pid file and log
// live, under appDirName.
const daemonSubdir = "daemon"

// recordingsSubdir is where T11c's recordings live, under appDirName:
// ~/.creality_k2_mcp/recordings/<printer-id>/<timestamp>.mp4
// (dev_docs/plan-v0.1.0.md decision 11). Unlike the daemon socket, a
// recording file path is never bound by AF_UNIX's sun_path limit, so this
// always resolves under the real home directory, even on the rare machine
// where daemonDir had to fall back to a short path under os.TempDir for the
// socket alone.
const recordingsSubdir = "recordings"

// socketName is the base name socket.New(paths.Dir, socketName) uses: the
// socket ends up at paths.Dir/daemon.sock, the lock at paths.Dir/lock
// (github.com/sairaph/mcp-wizard/daemon/socket.New's own naming).
const socketName = "daemon"

// Paths locates every file the daemon and its clients need to agree on.
type Paths struct {
	Dir    string // ~/.creality_k2_mcp/daemon
	Lock   string // Dir/lock (mcp-wizard daemon/socket.Server's own lock file)
	Socket string // Dir/daemon.sock
	PID    string // Dir/daemon.pid
	Log    string // Dir/daemon.log
}

// maxSocketPathBytes is the practical ceiling for a Unix domain socket path.
// sun_path is 108 bytes on Linux and 104 on macOS and the BSDs, including
// the terminating NUL, and Windows' AF_UNIX support (used here per
// daemon.go's own doc comment: "works on Windows 10+") lays out the same
// fixed-size sockaddr_un struct, so the limit applies there too. Exceeding
// it fails at listen/connect time with a bare, undiagnosable "invalid
// argument", so the path is kept comfortably under the tightest real limit
// up front instead (matching interactive-terminal-mcp's own
// maxUnixSocketPath; dev_docs/review-backlog.md item 21).
const maxSocketPathBytes = 100

// FallbackRoot is where daemonDir's short-path fallback (below) is rooted.
// It defaults to the real os.TempDir and production code never changes it,
// so the fallback stays exactly what it always was: a stable, per-user
// directory under the real OS temp directory. It exists as a variable only
// so a test whose HOME/USERPROFILE resolves to a long path (a common case:
// testing.T.TempDir() embeds the full test name, which alone is often
// enough to push "<home>/.creality_k2_mcp/daemon/daemon.sock" past
// maxSocketPathBytes) can point the fallback at its own short, test-owned
// directory instead of silently creating creality_k2_mcp-<hash> directories
// in the real user's real temp directory (dev_docs/review-backlog.md item
// 43). internal/daemon/daemontest.Guard is the standard way test packages
// do this.
var FallbackRoot = os.TempDir

// DefaultPaths resolves Paths under the current user's home directory,
// falling back to a short per-user directory under os.TempDir when that
// would push the socket path past maxSocketPathBytes (see daemonDir). A
// deep or long-named home directory - common on Windows, where a profile
// often lives under a long "C:\Users\<domain account>\AppData\..." tree -
// can easily push "<home>/.creality_k2_mcp/daemon/daemon.sock" over the
// limit.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("daemon: resolve home directory: %w", err)
	}
	return PathsIn(daemonDir(home)), nil
}

// daemonDir picks the daemon's directory for a given home directory: the
// normal path under home when its socket file fits maxSocketPathBytes,
// otherwise a short path under os.TempDir, named from a hash of home so it
// is stable across runs of the same user and distinct across users sharing
// a machine (a bare, unhashed name under a shared temp directory could
// otherwise let two users collide on the same socket).
//
// The whole daemon directory moves, not just the socket file: mcp-wizard's
// daemon/socket.Server keeps its lock file next to the socket
// (socketDir/lock), so splitting them across two directories would only
// move the problem; the pid file and log follow along too, so every file
// the daemon owns still lives in one place.
func daemonDir(home string) string {
	preferred := filepath.Join(home, appDirName, daemonSubdir)
	if fitsSocketPath(preferred) {
		return preferred
	}
	return filepath.Join(FallbackRoot(), "creality_k2_mcp-"+shortHash(home))
}

// fitsSocketPath reports whether dir/<socketName>.sock fits within
// maxSocketPathBytes.
func fitsSocketPath(dir string) bool {
	return len(filepath.Join(dir, socketName+".sock")) <= maxSocketPathBytes
}

// shortHash derives a stable, filename-safe token from value.
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

// DefaultRecordingsDir resolves T11c's recordings directory under the
// current user's real home directory, independent of DefaultPaths' own
// short-path fallback (see recordingsSubdir).
func DefaultRecordingsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve home directory: %w", err)
	}
	return filepath.Join(home, appDirName, recordingsSubdir), nil
}

// PathsIn builds Paths rooted at dir, matching exactly how
// github.com/sairaph/mcp-wizard/daemon/socket.New(dir, socketName) names
// its own socket and lock files, so a client and the server always agree on
// where they are without either having to ask the other.
func PathsIn(dir string) Paths {
	return Paths{
		Dir:    dir,
		Lock:   filepath.Join(dir, "lock"),
		Socket: filepath.Join(dir, socketName+".sock"),
		PID:    filepath.Join(dir, "daemon.pid"),
		Log:    filepath.Join(dir, "daemon.log"),
	}
}
