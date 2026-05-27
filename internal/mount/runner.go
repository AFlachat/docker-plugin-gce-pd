package mount

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runner abstracts running an external command. The real implementation shells
// out; tests substitute a fake to assert on the exact argv and to script
// outputs/errors without touching the host.
type runner interface {
	// run executes name with args and returns combined stdout+stderr. A non-zero
	// exit yields an *exitError carrying the code and output.
	run(ctx context.Context, name string, args ...string) (string, error)
}

// execRunner runs commands for real via os/exec.
type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), &exitError{
				cmd:    name + " " + strings.Join(args, " "),
				code:   ee.ExitCode(),
				output: strings.TrimSpace(string(out)),
			}
		}
		// Command not found, context cancelled, etc.
		return string(out), fmt.Errorf("run %s: %w", name, err)
	}
	return string(out), nil
}

// exitError is returned when a command exits non-zero. Callers inspect code to
// distinguish "expected" non-zero exits (e.g. blkid returns 2 for an unformatted
// device) from real failures.
type exitError struct {
	cmd    string
	code   int
	output string
}

func (e *exitError) Error() string {
	if e.output != "" {
		return fmt.Sprintf("%q exited %d: %s", e.cmd, e.code, e.output)
	}
	return fmt.Sprintf("%q exited %d", e.cmd, e.code)
}

// exitCode extracts the exit code from an error, or -1 if it is not an
// *exitError.
func exitCode(err error) int {
	if ee, ok := err.(*exitError); ok {
		return ee.code
	}
	return -1
}
