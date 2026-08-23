package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type failAfterWriter struct {
	writesBeforeFailure int
}

func (writer *failAfterWriter) Write(data []byte) (int, error) {
	if writer.writesBeforeFailure == 0 {
		return 0, errors.New("write failed")
	}
	writer.writesBeforeFailure--
	return len(data), nil
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "nano-harness dev"},
		{name: "help", args: []string{"--help"}, wantCode: 0, wantStdout: "usage:"},
		{name: "missing", wantCode: 2, wantStderr: "usage:"},
		{name: "unknown", args: []string{"serve"}, wantCode: 2, wantStderr: "unknown command \"serve\""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			gotCode := run(test.args, &stdout, &stderr)

			if gotCode != test.wantCode {
				t.Fatalf("run() code = %d, want %d", gotCode, test.wantCode)
			}
			if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Errorf("run() stdout = %q, want substring %q", stdout.String(), test.wantStdout)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Errorf("run() stderr = %q, want substring %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestRun_WriteFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		stdout errorWriter
		stderr errorWriter
	}{
		{name: "missing command", stderr: errorWriter{}},
		{name: "version", args: []string{"version"}, stdout: errorWriter{}},
		{name: "help", args: []string{"help"}, stdout: errorWriter{}},
		{name: "unknown command", args: []string{"serve"}, stderr: errorWriter{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := run(test.args, test.stdout, test.stderr); got != 1 {
				t.Fatalf("run() code = %d, want 1", got)
			}
		})
	}
}

func TestRun_UsageWriteFailureAfterDiagnostic(t *testing.T) {
	t.Parallel()

	stderr := &failAfterWriter{writesBeforeFailure: 1}
	if got := run([]string{"serve"}, io.Discard, stderr); got != 1 {
		t.Fatalf("run() code = %d, want 1", got)
	}
}

func TestMainFunction(t *testing.T) {
	originalArgs := os.Args
	originalExit := exitProcess
	t.Cleanup(func() {
		os.Args = originalArgs
		exitProcess = originalExit
	})

	os.Args = []string{"nano-harness", "version"}
	exitCode := -1
	exitProcess = func(code int) {
		exitCode = code
	}
	main()

	if exitCode != 0 {
		t.Fatalf("main() exit code = %d, want 0", exitCode)
	}
}
