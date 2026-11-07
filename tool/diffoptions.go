package tool

// diffOptions are the flags `storm diff` reads.
type diffOptions struct {
	allowDestructive bool
	concurrently     bool
	replay           bool
	down             bool
	dryRun           bool
}
