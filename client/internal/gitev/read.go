package gitev

import "context"

// Run runs one read-only git query in dir with the package's guarantees: a
// hard deadline (Timeout), its own process group, no optional locks, no lazy
// fetch of a partial clone's missing objects, and output cut at max bytes
// (truncated then reports it). A caller that reads more than the timeline
// does (the results step reads file history) uses it, so every git read in
// tacit goes through one hardened runner.
func (g *Git) Run(ctx context.Context, dir string, max int, args ...string) (out string, truncated bool, err error) {
	return g.run(ctx, dir, max, args...)
}

// RunStdin is Run with text on standard input, for `cat-file --batch`.
func (g *Git) RunStdin(ctx context.Context, dir string, max int, stdin string, args ...string) (out string, truncated bool, err error) {
	return g.runIn(ctx, dir, max, stdin, args...)
}

// Readable reports whether dir may be read at all. A git older than 2.44
// would fetch a partial clone's missing files into the owner's repo, so such
// a checkout is not read, and why says so.
func (g *Git) Readable(ctx context.Context, dir string) (ok bool, why string) {
	if !g.noLazyFetch(ctx) && g.isPartial(ctx, dir) {
		return false, "partial clone, and this git is older than 2.44 and would fetch the files the queries need"
	}
	return true, ""
}
