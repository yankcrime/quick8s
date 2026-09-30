package k3s

import (
	"bytes"
	"fmt"
	"strings"
)

// Progress receives human-readable status lines as an operation runs. This
// package never prints; callers decide where status goes. A nil Progress
// discards everything.
type Progress func(string)

func (p Progress) printf(format string, args ...any) {
	if p != nil {
		p(fmt.Sprintf(format, args...))
	}
}

// installerOutput adapts the install script's streamed stdout into Progress
// lines, dropping the script's "[INFO]" prefix since every stdout line has it.
type installerOutput struct {
	progress Progress
	partial  []byte
}

func (w *installerOutput) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
}

// flush emits a final line the script didn't terminate with a newline.
func (w *installerOutput) flush() {
	w.emit(string(w.partial))
	w.partial = nil
}

func (w *installerOutput) emit(line string) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "[INFO]"))
	if line != "" {
		w.progress.printf("%s", line)
	}
}
