package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"
)

const alarmPlaybackTimeout = 3 * time.Second

type alarmPlayer func(context.Context, string, ...string) error

func playAlarm(ctx context.Context, output io.Writer) error {
	return playAlarmWith(ctx, output, runtime.GOOS, alarmPlaybackTimeout, runAlarmPlayer)
}

func playAlarmWith(ctx context.Context, output io.Writer, platform string, timeout time.Duration, play alarmPlayer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if platform != "darwin" {
		n, err := io.WriteString(output, "\a")
		if err != nil {
			return fmt.Errorf("ring terminal bell: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("ring terminal bell: %w", io.ErrShortWrite)
		}
		return nil
	}

	playCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := play(playCtx, "/usr/bin/afplay", "/System/Library/Sounds/Sosumi.aiff"); err != nil {
		if ctxErr := playCtx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("play alarm: %w", err)
	}
	return nil
}

func runAlarmPlayer(ctx context.Context, name string, args ...string) error {
	// Nil stdout and stderr send player output to the null device.
	return exec.CommandContext(ctx, name, args...).Run()
}
