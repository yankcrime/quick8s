// Package cli wires up the quick8s Cobra command tree. Commands here only
// parse flags and call into internal/node and internal/k3s for the real work.
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCmd builds the top-level quick8s command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "quick8s",
		Short:         "Bootstrap K3s Kubernetes clusters over SSH",
		SilenceErrors: true, // main owns error reporting.
	}

	root.AddCommand(newUpCmd())
	root.AddCommand(newDownCmd())
	root.AddCommand(newBootstrapCmd())
	root.AddCommand(newJoinCmd())
	root.AddCommand(newKubeconfigCmd())
	root.AddCommand(newTeardownCmd())
	root.AddCommand(newVersionCmd())

	return root
}
