package results

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/gitev"
)

// Commit is one commit that touched a file.
type Commit struct {
	SHA  string // full
	Date string // committer date in UTC, YYYY-MM-DD
	Unix int64  // committer time
}

// Short is the 12 characters facts quote.
func (c Commit) Short() string {
	if len(c.SHA) > 12 {
		return c.SHA[:12]
	}
	return c.SHA
}

// Version is one file as one commit left it.
type Version struct {
	Commit Commit
	Obs    []Obs
}

// Limits of one repo's read. They bound time and memory, not the answer:
// a bound that cuts a read is reported.
const (
	logCommits   = 6000      // newest commits whose touched files are listed
	maxVersions  = 40        // versions of one file read
	maxBlob      = 3 << 20   // a larger file is a dump, not a results table
	maxMdBlob    = 1 << 20   //
	headBudget   = 192 << 20 // bytes of newest versions read per repo
	historyBytes = 256 << 20 // bytes of older versions read per repo
	batchBytes   = 48 << 20  // one cat-file call
)

var noisePath = regexp.MustCompile(`(^|/)(node_modules|vendor|third_party|target|dist|build|analytics|telemetry|\.git|\.github)/|(^|/)(package(-lock)?|tsconfig[^/]*|composer|Pipfile)\.json$|\.(lock|min\.js|map)$|-lock\.json$`)

// Reader reads the history of the results files of one checkout.
//
// What a read could not do is recorded in two kinds. Failed: a git call that
// errored, timed out or returned something that does not parse; nothing read
// from this repo can be trusted to be all of it, so the caller keeps whatever
// the store already holds and writes nothing. Partial: a bound the step sets
// itself (the log was cut at its size cap, the byte budget ran out); the rows
// read are right but files are missing, so the caller writes what it read and
// retires nothing.
type Reader struct {
	G   *gitev.Git
	Dir string
	Ref string // full sha of the commit read
	// HeadBudget overrides headBudget (bytes of newest versions); 0 keeps it.
	HeadBudget int
	Notes      []string
	Failed     []string
	Partial    []string
}

func (r *Reader) note(format string, a ...any) { r.Notes = append(r.Notes, fmt.Sprintf(format, a...)) }

func (r *Reader) fail(format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	r.Failed = append(r.Failed, m)
	r.Notes = append(r.Notes, m)
}

func (r *Reader) partial(format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	r.Partial = append(r.Partial, m)
	r.Notes = append(r.Notes, m)
}

// Touched lists the files of the newest commits with the commits that
// touched them, newest first. Only files of a kind the step reads, outside
// vendored and generated directories, touched at least twice: a file written
// once has no older value to replace.
func (r *Reader) Touched(ctx context.Context) map[string][]Commit {
	out, trunc, err := r.G.Run(ctx, r.Dir, 160<<20, "log", "--no-renames", "--no-merges", "-n", strconv.Itoa(logCommits), "--format=@@%H %ct", "--name-only", r.Ref)
	if err != nil {
		r.fail("file history unreadable: %v", shortErr(err))
		return nil
	}
	if trunc {
		r.partial("file history cut at its size cap before the oldest commits were listed")
	}
	all := map[string][]Commit{}
	var cur Commit
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			f := strings.Fields(l[2:])
			cur = Commit{}
			if len(f) == 2 {
				secs, _ := strconv.ParseInt(f[1], 10, 64)
				cur = Commit{SHA: f[0], Date: time.Unix(secs, 0).UTC().Format("2006-01-02"), Unix: secs}
			}
		case l != "" && cur.SHA != "":
			if Kind(l) != "" && !noisePath.MatchString(l) {
				all[l] = append(all[l], cur)
			}
		}
	}
	for p, cs := range all {
		if len(cs) < 2 {
			delete(all, p)
		}
	}
	return all
}

type blobReq struct {
	Rev  string // commit or ref
	Path string
}

func (b blobReq) spec() string { return b.Rev + ":" + b.Path }

// Blobs reads files at commits through cat-file --batch. A missing file or
// one over limit(path) is absent from the result. The total read stops at
// budget bytes.
func (r *Reader) Blobs(ctx context.Context, reqs []blobReq, limit func(string) int, budget int) map[blobReq][]byte {
	got := map[blobReq][]byte{}
	if len(reqs) == 0 {
		return got
	}
	// sizes first, so a batch is bounded and a dump is never pulled
	var in strings.Builder
	for _, q := range reqs {
		in.WriteString(q.spec() + "\n")
	}
	out, trunc, err := r.G.RunStdin(ctx, r.Dir, 64<<20, in.String(), "cat-file", "--batch-check")
	if err != nil || trunc {
		r.fail("object sizes unreadable: %v", shortErr(err))
		return got
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(reqs) {
		r.fail("object sizes: answer count differs")
		return got
	}
	type sized struct {
		q    blobReq
		size int
	}
	var todo []sized
	total := 0
	for i, l := range lines {
		f := strings.Fields(l)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		n, _ := strconv.Atoi(f[2])
		if n == 0 || n > limit(reqs[i].Path) {
			continue
		}
		if total+n > budget {
			r.partial("read budget of %d MB reached, the remaining files are not read", budget>>20)
			break
		}
		total += n
		todo = append(todo, sized{reqs[i], n})
	}
	for start := 0; start < len(todo); {
		end, sum := start, 0
		for end < len(todo) && (end == start || sum+todo[end].size <= batchBytes) {
			sum += todo[end].size
			end++
		}
		in.Reset()
		for _, s := range todo[start:end] {
			in.WriteString(s.q.spec() + "\n")
		}
		out, trunc, err := r.G.RunStdin(ctx, r.Dir, sum+len(todo[start:end])*128+4096, in.String(), "cat-file", "--batch")
		if err != nil || trunc {
			r.fail("file contents unreadable: %v", shortErr(err))
			return got
		}
		pos := 0
		for _, s := range todo[start:end] {
			nl := strings.IndexByte(out[pos:], '\n')
			if nl < 0 {
				r.fail("file contents: answer ended early")
				return got
			}
			hdr := strings.Fields(out[pos : pos+nl])
			pos += nl + 1
			if len(hdr) != 3 || hdr[1] != "blob" {
				r.fail("file contents: unexpected answer for %s", s.q.spec())
				return got
			}
			n, _ := strconv.Atoi(hdr[2])
			if n != s.size || pos+n > len(out) {
				r.fail("file contents: size of %s differs from what was announced", s.q.spec())
				return got
			}
			got[s.q] = []byte(out[pos : pos+n])
			pos += n + 1
		}
		start = end
	}
	return got
}

func blobLimit(p string) int {
	if Kind(p) == "md" {
		return maxMdBlob
	}
	return maxBlob
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// Series reads the files of the checkout that carry measured numbers and
// returns one Series per measured quantity and setup. Stats counts what was
// looked at.
func (r *Reader) Series(ctx context.Context, repo string, st *Stats) []*Series {
	touched := r.Touched(ctx)
	if len(r.Failed) > 0 {
		return nil
	}
	paths := make([]string, 0, len(touched))
	for p := range touched {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	st.Candidates = len(paths)

	// Phase 1: the newest version of every candidate. Files with no measured
	// number in it are dropped before any history is read.
	reqs := make([]blobReq, 0, len(paths))
	for _, p := range paths {
		reqs = append(reqs, blobReq{r.Ref, p})
	}
	budget := headBudget
	if r.HeadBudget > 0 {
		budget = r.HeadBudget
	}
	head := r.Blobs(ctx, reqs, blobLimit, budget)
	if len(r.Failed) > 0 {
		return nil
	}
	type live struct {
		path  string
		roles *Roles
		obs   []Obs
	}
	var lives []live
	for _, p := range paths {
		data, ok := head[blobReq{r.Ref, p}]
		if !ok {
			continue
		}
		obs, roles := Extract(p, data, nil)
		if len(obs) == 0 {
			continue
		}
		lives = append(lives, live{p, roles, obs})
		st.File(Kind(p))
	}
	// Phase 2: the older versions of those.
	var older []blobReq
	for _, l := range lives {
		cs := touched[l.path]
		if len(cs) > maxVersions {
			cs = cs[:maxVersions]
		}
		for _, c := range cs[1:] { // cs[0] produced the newest version, already read
			older = append(older, blobReq{c.SHA, l.path})
		}
	}
	blobs := r.Blobs(ctx, older, blobLimit, historyBytes)
	if len(r.Failed) > 0 {
		return nil
	}
	var out []*Series
	for _, l := range lives {
		cs := touched[l.path]
		truncated := false
		if len(cs) > maxVersions {
			cs, truncated = cs[:maxVersions], true
		}
		// oldest first
		vs := make([]Version, 0, len(cs))
		for i := len(cs) - 1; i >= 1; i-- {
			data, ok := blobs[blobReq{cs[i].SHA, l.path}]
			if !ok {
				continue
			}
			obs, _ := Extract(l.path, data, l.roles)
			vs = append(vs, Version{cs[i], obs})
		}
		vs = append(vs, Version{cs[0], l.obs})
		for _, s := range BuildSeries(repo, l.path, vs, truncated) {
			s.FileSha, s.FileDate = cs[0].Short(), cs[0].Date
			out = append(out, s)
		}
	}
	st.Series = len(out)
	for _, s := range out {
		if s.Recurs() {
			st.Recurring++
		}
	}
	return out
}
