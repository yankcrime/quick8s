package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"quick8s/internal/cluster"
	"quick8s/internal/lifecycle"
)

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "plan", Short: "Inspect managed infrastructure without changing it", Args: cobra.NoArgs}
	file := registerFileFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cmd.SilenceUsage = true
		spec, err := cluster.Load(*file)
		if err != nil {
			return err
		}
		if spec.Managed == nil {
			return fmt.Errorf("plan currently supports provider-backed clusters only")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		result, err := lifecycle.Describe(ctx, *file, spec)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
		return err
	}
	return cmd
}
