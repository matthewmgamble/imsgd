package macos

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
)

// CheckFullDiskAccess reports whether this process can read path, which
// should be a TCC-protected file such as ~/Library/Messages/chat.db.
// It returns (false, nil) when access is denied by TCC and an error for
// other failures (e.g. the file does not exist).
func CheckFullDiskAccess(path string) (bool, error) {
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		return true, nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return false, nil
	}
	return false, err
}

// MessagesRunning reports whether Messages.app is running.
func MessagesRunning(ctx context.Context) bool {
	return exec.CommandContext(ctx, "/usr/bin/pgrep", "-xq", "Messages").Run() == nil
}

// CheckMessagesAutomation sends a harmless Apple event to Messages. The first
// call may trigger the macOS Automation consent prompt. Note: this launches
// Messages if it is not running.
func CheckMessagesAutomation(ctx context.Context) error {
	_, err := RunAppleScript(ctx, `tell application "Messages" to return count of accounts`)
	return err
}
