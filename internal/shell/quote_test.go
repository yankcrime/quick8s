package shell

import (
	"os/exec"
	"testing"
)

func TestQuotePreservesLiteralArgument(t *testing.T) {
	for _, value := range []string{"", "simple", "two words", "a'b\"c", "$(printf expanded)`printf expanded`$HOME", "one\ntwo\tthree", "\\*; & | <> café"} {
		t.Run(value, func(t *testing.T) {
			out, err := exec.Command("sh", "-c", "printf '%s' "+Quote(value)).Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != value {
				t.Fatalf("got %q, want %q", out, value)
			}
		})
	}
}
