//go:build darwin || linux

package term

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

func TestDetectPasteHandlesSplitAndMalformedOutput(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   error
	}{
		{name: "split announcement", chunks: []string{"banner\x1b[?", "200", "4h"}},
		{name: "unknown then announcement", chunks: []string{"\x1b[?9999h", "\x1b[?2004h"}},
		{name: "paste off only", chunks: []string{"\x1b[?2004l"}, want: ErrNoPaste},
		{name: "malformed", chunks: []string{"\x1b[?20:04h"}, want: ErrNoPaste},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &chunkReader{}
			for _, chunk := range test.chunks {
				reader.chunks = append(reader.chunks, []byte(chunk))
			}
			if err := detectPaste(reader); !errors.Is(err, test.want) {
				t.Fatalf("detectPaste = %v, want %v", err, test.want)
			}
		})
	}
}

func TestProbePasteContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ProbePasteContext(ctx, "/bin/sh"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProbePasteContext = %v, want context cancellation", err)
	}
}

func TestPasteWaitCancellationJoinsBlockedReader(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- waitForPaste(ctx, reader) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForPaste = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled paste reader did not stop")
	}
}

func TestProbePasteContextHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	if err := ProbePasteContext(ctx, "/bin/sh"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ProbePasteContext = %v, want deadline exceeded", err)
	}
}

func TestProbePasteSeesTheAnnouncement(t *testing.T) {
	path := writeFakeShell(t, "printf '\\033[?2004h'\n")
	if err := ProbePaste(path); err != nil {
		t.Fatalf("a program that writes CSI ? 2004 h failed ProbePaste: %v", err)
	}
}

func TestProbePasteSeesAnAnnouncementAfterOtherOutput(t *testing.T) {
	path := writeFakeShell(t, "printf 'welcome\\n\\033[?2004h'\n")
	if err := ProbePaste(path); err != nil {
		t.Fatalf("the mark after other output was missed: %v", err)
	}
}

func TestProbePasteRejectsAShellThatNeverAnnounces(t *testing.T) {
	path := writeFakeShell(t, "printf 'sh$ '\n")
	err := ProbePaste(path)
	if !errors.Is(err, ErrNoPaste) {
		t.Fatalf("ProbePaste = %v, want ErrNoPaste", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the shell %q: %v", path, err)
	}
}

func TestProbePasteIgnoresPasteOff(t *testing.T) {
	path := writeFakeShell(t, "printf '\\033[?2004l'\n")
	if err := ProbePaste(path); !errors.Is(err, ErrNoPaste) {
		t.Fatalf("CSI ? 2004 l alone was accepted: %v", err)
	}
}

func TestProbePasteReportsAMissingShell(t *testing.T) {
	err := ProbePaste(filepath.Join(t.TempDir(), "no-such-shell"))
	if err == nil {
		t.Fatal("a missing binary succeeded")
	}
	if errors.Is(err, ErrNoPaste) {
		t.Fatalf("a missing binary was reported as no paste marks: %v", err)
	}
}

func TestProbePasteSeesARealLineEditor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("ENV", "")
	t.Setenv("BASH_ENV", "")

	sh := interactiveShell(t)
	if err := ProbePaste(sh.path, sh.args...); err != nil {
		t.Fatalf("ProbePaste(%s) = %v, want the line editor's 2004 h", sh.path, err)
	}
}

func writeFakeShell(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
