package send

import (
	"context"
	"log/slog"
	"os/exec"
	"time"

	"github.com/matthewmgamble/imsgd/internal/macos"
)

// KeepMessagesRunning relaunches Messages (in the background, without
// stealing focus) whenever it is not running, checking every interval.
// Sending requires Messages to be open.
func KeepMessagesRunning(ctx context.Context, interval time.Duration) {
	check := func() {
		if macos.MessagesRunning(ctx) {
			return
		}
		slog.Info("Messages is not running; launching it")
		if err := exec.CommandContext(ctx, "/usr/bin/open", "-g", "-a", "Messages").Run(); err != nil {
			slog.Warn("failed to launch Messages", "err", err)
		}
	}
	check()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
