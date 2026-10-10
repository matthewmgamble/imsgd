package macos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// AppleScriptError is returned when osascript exits non-zero.
type AppleScriptError struct {
	// Number is the AppleScript error number (e.g. -1743 for "not authorized
	// to send Apple events"), or 0 if it could not be parsed.
	Number  int
	Message string
}

func (e *AppleScriptError) Error() string {
	if e.Number != 0 {
		return fmt.Sprintf("applescript error %d: %s", e.Number, e.Message)
	}
	return "applescript error: " + e.Message
}

// Well-known AppleScript error numbers.
const (
	ErrNumNotAuthorized = -1743 // Automation permission denied
	ErrNumTimedOut      = -1712 // AppleEvent timed out
)

// RunAppleScript executes script with osascript. The script is passed on
// stdin and args are delivered to the script's `on run argv` handler, so
// caller-supplied values are never interpolated into script source.
func RunAppleScript(ctx context.Context, script string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	// "-" makes osascript read the program from stdin while still accepting
	// trailing arguments.
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", append([]string{"-"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("osascript: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", parseAppleScriptError(stderr.String())
		}
		return "", fmt.Errorf("osascript: %w", err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// parseAppleScriptError extracts the message and number from osascript
// stderr, which looks like: "-:123:145: execution error: Messages got an
// error: Not authorized to send Apple events to Messages. (-1743)".
func parseAppleScriptError(stderr string) *AppleScriptError {
	msg := strings.TrimSpace(stderr)
	if i := strings.Index(msg, "execution error: "); i >= 0 {
		msg = msg[i+len("execution error: "):]
	}
	e := &AppleScriptError{Message: msg}
	if open := strings.LastIndex(msg, "("); open >= 0 && strings.HasSuffix(msg, ")") {
		var n int
		if _, err := fmt.Sscanf(msg[open:], "(%d)", &n); err == nil {
			e.Number = n
			e.Message = strings.TrimSpace(msg[:open])
		}
	}
	return e
}
