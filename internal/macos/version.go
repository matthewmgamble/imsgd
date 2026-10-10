// Package macos wraps macOS-specific facilities: OS version detection,
// AppleScript execution and permission probes.
package macos

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Version is a macOS product version such as 26.5.2.
type Version struct {
	Major, Minor, Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AtLeast reports whether v >= major.minor.
func (v Version) AtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// ParseVersion parses "26.5.2", "15.1" or "11".
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return Version{}, fmt.Errorf("invalid macOS version %q", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("invalid macOS version %q: %w", s, err)
		}
		nums[i] = n
	}
	return Version{nums[0], nums[1], nums[2]}, nil
}

// OSVersion returns the running macOS version via sw_vers.
func OSVersion(ctx context.Context) (Version, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return Version{}, fmt.Errorf("sw_vers: %w", err)
	}
	return ParseVersion(string(out))
}
