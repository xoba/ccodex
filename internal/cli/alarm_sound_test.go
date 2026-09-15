package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestAlarmUsesMacOSSoundOnce(t *testing.T) {
	var output bytes.Buffer
	calls := 0
	play := func(ctx context.Context, name string, args ...string) error {
		calls++
		if name != "/usr/bin/afplay" || !reflect.DeepEqual(args, []string{"/System/Library/Sounds/Sosumi.aiff"}) {
			t.Fatalf("unexpected player invocation: %q %q", name, args)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > alarmPlaybackTimeout {
			t.Fatalf("playback must have a bounded deadline: %v, %v", deadline, ok)
		}
		return nil
	}
	if err := playAlarmWith(context.Background(), &output, "darwin", alarmPlaybackTimeout, play); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || output.Len() != 0 {
		t.Fatalf("wanted one sound and no terminal bell: calls=%d output=%q", calls, output.String())
	}
}

func TestAlarmPlayerFailureDoesNotRingAgain(t *testing.T) {
	var output bytes.Buffer
	want := errors.New("player failed")
	calls := 0
	err := playAlarmWith(context.Background(), &output, "darwin", alarmPlaybackTimeout, func(context.Context, string, ...string) error {
		calls++
		return want
	})
	if !errors.Is(err, want) || calls != 1 || output.Len() != 0 {
		t.Fatalf("unexpected failed playback: err=%v calls=%d output=%q", err, calls, output.String())
	}
}

func TestAlarmPlaybackCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			limit := alarmPlaybackTimeout
			want := context.Canceled
			if timeout {
				limit = 10 * time.Millisecond
				want = context.DeadlineExceeded
			}
			var output bytes.Buffer
			err := playAlarmWith(ctx, &output, "darwin", limit, func(ctx context.Context, _ string, _ ...string) error {
				if !timeout {
					cancel()
				}
				<-ctx.Done()
				return errors.New("player killed")
			})
			if !errors.Is(err, want) || output.Len() != 0 {
				t.Fatalf("cancellation not preserved: err=%v output=%q", err, output.String())
			}
		})
	}
}

func TestAlarmHonorsCanceledContext(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var output bytes.Buffer
			err := playAlarmWith(ctx, &output, platform, alarmPlaybackTimeout, func(context.Context, string, ...string) error {
				t.Fatal("canceled alarm invoked the player")
				return nil
			})
			if !errors.Is(err, context.Canceled) || output.Len() != 0 {
				t.Fatalf("canceled alarm produced output: err=%v output=%q", err, output.String())
			}
		})
	}
}

func TestAlarmTerminalFallback(t *testing.T) {
	for _, platform := range []string{"linux", "windows", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			var output bytes.Buffer
			err := playAlarmWith(context.Background(), &output, platform, alarmPlaybackTimeout, func(context.Context, string, ...string) error {
				t.Fatal("terminal fallback invoked a player")
				return nil
			})
			if err != nil || output.String() != "\a" {
				t.Fatalf("wanted exactly one BEL byte: err=%v output=%q", err, output.String())
			}
		})
	}
	if err := playAlarmWith(context.Background(), failingWriter{}, "linux", alarmPlaybackTimeout, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error not propagated: %v", err)
	}
}

func TestRunAlarmPlayerNonzeroExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = runAlarmPlayer(context.Background(), executable, "-test.run=^TestAlarmPlayerHelper$", "--", "alarm-player-fail")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("unexpected player result: %v", err)
	}
}

func TestRunAlarmPlayerStopsOnTimeout(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = runAlarmPlayer(ctx, executable, "-test.run=^TestAlarmPlayerHelper$", "--", "alarm-player-wait")
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("player did not stop on deadline: %v (context %v)", err, ctx.Err())
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("player cancellation took %v", elapsed)
	}
}

func TestAlarmPlayerHelper(t *testing.T) {
	switch os.Args[len(os.Args)-1] {
	case "alarm-player-fail":
		fmt.Fprintln(os.Stdout, "player stdout must be discarded")
		fmt.Fprintln(os.Stderr, "player stderr must be discarded")
		os.Exit(7)
	case "alarm-player-wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}
