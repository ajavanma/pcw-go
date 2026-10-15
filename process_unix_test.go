//go:build linux || darwin

package pcw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessGroupCleanup(t *testing.T) {
	for _, mode := range []string{"descendant", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			client := fakeClient(t, mode, func(c *Config) { c.Dir = dir })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.Doctor(ctx); done <- err }()
			deadline := time.Now().Add(5 * time.Second)
			var pid int
			for {
				data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
				if err == nil {
					pid, _ = strconv.Atoi(string(data))
					if pid > 0 {
						break
					}
				}
				if time.Now().After(deadline) {
					cancel()
					<-done
					t.Fatal("child did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			if mode == "descendant" {
				cancel()
				errorKind(t, <-done, ErrorCanceled)
			} else {
				errorKind(t, <-done, ErrorProtocol)
			}
			// A Linux orphan may remain as an already-dead zombie until the init process
			// reaps it. That is not a running process or a retained worker pipe.
			deadline = time.Now().Add(3 * time.Second)
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					break
				}
				data, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
				closing := strings.LastIndex(string(data), ")")
				if closing >= 0 && strings.HasPrefix(string(data)[closing+1:], " Z") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("descendant %d still running", pid)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
