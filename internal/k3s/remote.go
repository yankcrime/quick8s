package k3s

// These interfaces describe only the remote capabilities each operation uses.
// node.Client implements them; tests can supply an in-memory implementation.
type runner interface {
	Run(string) (string, error)
}

type rootRunner interface {
	RunAsRoot(string) (string, error)
}

type preflightRunner interface {
	runner
	rootRunner
}

type rootFileWriter interface {
	WriteFileAsRoot(string, []byte) error
}
