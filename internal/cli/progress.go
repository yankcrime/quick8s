package cli

import (
	"sync"
	"time"

	"github.com/spf13/cobra"

	"quick8s/internal/k3s"
)

// heartbeatInterval is how long a slow step may go without printing anything
// before it reports that it's still waiting. The K3s installer goes quiet
// while systemd waits for the service to report ready, which is routinely
// tens of seconds and can hang outright - silence shouldn't look like either.
var heartbeatInterval = 10 * time.Second

// progressPrinter prints each progress line to stderr, indented beneath the
// step's title. Lines may arrive from other goroutines (SSH output is copied
// in the background), so writes are serialized, and the time of the latest
// write is kept for the heartbeat.
type progressPrinter struct {
	cmd  *cobra.Command
	mu   sync.Mutex
	last time.Time
}

func (p *progressPrinter) print(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = time.Now()
	p.cmd.PrintErrf("  %s\n", line)
}

// indented returns a k3s.Progress for quick steps that need no heartbeat.
func indented(cmd *cobra.Command) k3s.Progress {
	p := &progressPrinter{cmd: cmd}
	return p.print
}

// slowStep prints title, runs fn with indented progress, and prints a
// heartbeat whenever fn goes heartbeatInterval without reporting anything.
// On success it reports how long the step took.
func slowStep(cmd *cobra.Command, title string, fn func(k3s.Progress) error) error {
	cmd.PrintErrln(title)
	start := time.Now()
	p := &progressPrinter{cmd: cmd, last: start}

	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				p.mu.Lock()
				if now.Sub(p.last) >= heartbeatInterval {
					p.last = now
					cmd.PrintErrf("  still waiting... (%s elapsed)\n", now.Sub(start).Round(time.Second))
				}
				p.mu.Unlock()
			}
		}
	}()

	err := fn(p.print)
	close(stop)
	<-stopped
	if err != nil {
		return err
	}
	p.print("done in " + time.Since(start).Round(time.Second).String())
	return nil
}
