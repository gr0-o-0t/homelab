package cmd

// batchFlags holds common flags for lifecycle commands that support
// batch operations (--all, --group, --build).
type batchFlags struct {
	all   bool
	group string
	build bool
}
