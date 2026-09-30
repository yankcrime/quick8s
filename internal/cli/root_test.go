package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestVersionOutput(t *testing.T) {
	failure := errors.New("output closed")
	for _, fail := range []bool{false, true} {
		var out, stderr bytes.Buffer
		var writer io.Writer = &out
		if fail {
			writer = failedWriter{failure}
		}
		cmd := NewRootCmd()
		cmd.SetOut(writer)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"version"})
		err := cmd.Execute()
		if fail {
			if !errors.Is(err, failure) {
				t.Fatalf("lost output error: %v", err)
			}
		} else if err != nil || out.String() != Version+"\n" {
			t.Fatalf("output = %q, error = %v", out.String(), err)
		}
		if strings.Contains(stderr.String(), "Error:") {
			t.Fatalf("Cobra printed an error owned by main: %q", stderr.String())
		}
	}
}

func TestArgumentErrorIsReturnedWithoutPrinting(t *testing.T) {
	cmd := NewRootCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"bootstrap", "host", "--worker", "worker"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "can't combine") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if strings.Contains(stderr.String(), "can't combine") {
		t.Fatalf("Cobra duplicated the returned error: %q", stderr.String())
	}
}
