// Package shell quotes literal arguments for the POSIX shell used over SSH.
package shell

import "strings"

// Quote preserves s as one literal shell argument, including empty strings.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
