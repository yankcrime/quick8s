package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// confirm prompts on stderr and reads a yes/no answer from stdin.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	cmd.PrintErrf("%s [y/N]: ", prompt)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
