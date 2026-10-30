// Package evidence is tacit's exported reader of agent session history, for
// other Go programs on the machine: one stream of structured events per
// session (internal/events), where each harness keeps its sessions
// (internal/harnesses), and the rules tacit decides with who wrote a turn, how
// it redacts, which repository a path is in and how it runs a model. It is a
// thin layer of aliases and wrappers: every rule stays in its tacit package,
// so a reader of this package and tacit's own dreamer, owner corpus and
// reviewer decide the same way. SPEC.md ("Session events") is its contract.
//
// It does not import the dreamer or tacit's store packages.
package evidence

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/events"
	"github.com/agent-sh/tacit/client/internal/harnesses"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/repoinfer"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// --- Session events ---

// Event is one item of a session's history (events.Event).
type Event = events.Event

// Kind is what an event is; InputKind how a user turn was put in.
type (
	Kind      = events.Kind
	InputKind = events.InputKind
)

// The kinds of events.
const (
	KindOwnerText   = events.KindOwnerText
	KindAgentText   = events.KindAgentText
	KindHarnessText = events.KindHarnessText
	KindToolCall    = events.KindToolCall
	KindToolResult  = events.KindToolResult
	KindCompaction  = events.KindCompaction
)

// How a user turn was put in.
const (
	InputTyped      = events.InputTyped
	InputQueued     = events.InputQueued
	InputSlash      = events.InputSlash
	InputShell      = events.InputShell
	InputAnswer     = events.InputAnswer
	InputSuggestion = events.InputSuggestion
)

// ErrPastEnd is a cursor past the end of its transcript.
var ErrPastEnd = events.ErrPastEnd

// ReadFile reads the events of a Claude Code, Codex or pi transcript from
// byte offset from, calling emit for each in order, and returns the offset to
// resume from (events.ReadFile). ReadFileState also counts the lines that did
// not parse.
func ReadFile(path string, f Format, from int64, emit func(Event) error) (int64, error) {
	return events.ReadFile(path, f, from, emit)
}

// ReadHermes reads the events of a Hermes session after message id after,
// calling emit for each in order, and returns the id to resume after
// (events.ReadHermes). s must be a row of db.Sessions: its Owner, decided
// there, says whether its user rows can hold the owner's words.
func ReadHermes(ctx context.Context, db *HermesDB, s HermesSession, after int64, emit func(Event) error) (int64, error) {
	return events.ReadHermes(ctx, db, s, after, emit)
}

// State is a saved place in a session's history: its cursor (Offset) and
// what the reader kept from the items before it, so a read from it costs
// only what came after, while those items are the ones it was kept from
// (events.State). Unparsed counts the lines before the cursor that gave no
// events because they did not parse (Hermes: the rows whose tool calls did
// not). The zero State is the start; MarshalBinary and UnmarshalBinary save
// and load one, as do MarshalJSON and UnmarshalJSON. It is as private as its
// transcript: never send it.
type State = events.State

// ReadFileState is ReadFile from a State, returning the State after the last
// line read, with the count of lines that did not parse; limit > 0 stops it
// at a line boundary past limit bytes (events.ReadFileState).
func ReadFileState(path string, f Format, from State, limit int64, emit func(Event) error) (State, error) {
	return events.ReadFileState(path, f, from, limit, emit)
}

// ReadHermesState is ReadHermes from a State, returning the State after the
// last row read, with the count of rows whose tool calls did not parse
// (events.ReadHermesState).
func ReadHermesState(ctx context.Context, db *HermesDB, s HermesSession, from State, emit func(Event) error) (State, error) {
	return events.ReadHermesState(ctx, db, s, from, emit)
}

// RedactEvents masks secret-shaped text in events' Text, ToolInput and
// ToolOutput, and fails closed: on any failure it returns nil and the error
// (events.Redact).
func RedactEvents(ctx context.Context, evs []Event) ([]Event, error) {
	return events.Redact(ctx, evs)
}

// --- Where sessions are ---

// Source is one place a harness keeps sessions (harnesses.Source).
type Source = harnesses.Source

// Sources are this machine's session sources under home: Claude Code's
// transcripts and its subagents', Codex's sessions and archived sessions,
// pi's sessions and tasks, the Hermes store (harnesses.Sources).
func Sources(home string) []Source { return harnesses.Sources(home) }

// TopLevel is srcs less the subagents' globs: what tacit's dreamer reads.
func TopLevel(srcs []Source) []Source { return harnesses.TopLevel(srcs) }

// Files lists a source's transcripts, sorted, one path per session
// (events.Files).
func Files(src Source) ([]string, error) { return events.Files(src) }

// --- Transcripts ---

// Glob is filepath.Glob for plain transcripts, with their compressed copies
// (transcript.Glob).
func Glob(pattern string) ([]string, error) { return transcript.Glob(pattern) }

// Open reads a transcript's JSONL bytes, plain or compressed.
func Open(path string) (io.ReadCloser, error) { return transcript.Open(path) }

// OpenAt reads a transcript's JSONL bytes from offset off.
func OpenAt(path string, off int64) (io.ReadCloser, error) { return transcript.OpenAt(path, off) }

// Head is a transcript's first n bytes; nil when it cannot be read.
func Head(path string, n int) []byte { return transcript.Head(path, n) }

// Stat is a transcript's file and its size in JSONL bytes.
func Stat(path string) (os.FileInfo, int64, error) { return transcript.Stat(path) }

// Resolve is the file that holds a transcript now (its compressed copy once
// the plain file is gone).
func Resolve(path string) string { return transcript.Resolve(path) }

// Plain is a transcript's name whichever file holds it.
func Plain(path string) string { return transcript.Plain(path) }

// Name is a transcript's file name less its endings.
func Name(path string) string { return transcript.Name(path) }

// --- Who wrote it ---

// Format is a transcript dialect.
type Format = digest.Format

// The formats.
const (
	Claude = digest.Claude
	Codex  = digest.Codex
	Pi     = digest.Pi
	Hermes = digest.Hermes
)

// Who wrote a session's turns or a piece of one; Verdict is a session's,
// with its reason.
type (
	Who     = digest.Who
	Verdict = digest.Verdict
)

// Who wrote it.
const (
	Owner   = digest.Owner
	Agent   = digest.Agent
	Unknown = digest.Unknown
	Harness = digest.Harness
)

// Piece is a part of a user turn's text and who wrote it.
type Piece = digest.Piece

// ClaudeMarks are what a Claude Code line records about who wrote a user
// turn; ClaudeCommands follows a transcript for the slash commands Claude
// Code ran itself; CodexAsked is what a Codex agent's questions offered;
// TurnReader reads a transcript's lines (Parts, PartsCut) and Hermes rows
// (Hermes, HermesCut) for what the owner typed, and what was cut from it as
// not their words, as tacit's corpus does with its Pastes set.
type (
	ClaudeMarks    = digest.ClaudeMarks
	ClaudeCommands = digest.ClaudeCommands
	CodexAsked     = digest.CodexAsked
	TurnReader     = ownerwords.TurnReader
)

// SessionWho decides who a transcript's user turns come from, from its path
// and first 256 KiB (digest.SessionWho).
func SessionWho(path string, head []byte, f Format) Verdict { return digest.SessionWho(path, head, f) }

// ClaudeMarksOf reads the marks of a decoded Claude Code line.
func ClaudeMarksOf(o map[string]any) ClaudeMarks { return digest.ClaudeMarksOf(o) }

// ClaudeTyped reports a Claude Code user line a person typed, and why not.
func ClaudeTyped(m ClaudeMarks) (bool, string) { return digest.ClaudeTyped(m) }

// ClaudeQueuedLine reads a Claude Code line that delivers a message typed
// while the agent worked.
func ClaudeQueuedLine(line []byte) (texts []string, typed bool, why string, ok bool) {
	return digest.ClaudeQueuedLine(line)
}

// TurnPieces splits a user turn's text by who wrote each part.
func TurnPieces(text string) []Piece { return digest.TurnPieces(text) }

// OwnerPieces are the texts of ps that are the person's.
func OwnerPieces(ps []Piece) []string { return digest.OwnerPieces(ps) }

// HermesOwnerText is what the owner wrote in a Hermes user row, out of the
// gateway's frames, and whether none of it is theirs.
func HermesOwnerText(content, displayKind string, summary bool) (string, bool) {
	return digest.HermesOwnerText(content, displayKind, summary)
}

// UntypedWho is who wrote a user turn no person typed, whole: the harness
// when it is harness text, else another agent (digest.UntypedWho).
func UntypedWho(f Format, txt string) Who { return digest.UntypedWho(f, txt) }

// PiTask reports a pi subagent's session by its place, <session>/tasks/
// (digest.PiTask).
func PiTask(path string) bool { return digest.PiTask(path) }

// ClaudePastes are the texts the owner pasted into Claude Code prompts, from
// home's prompt history, which tacit's corpus cuts from their words; set them
// on a TurnReader to read a turn as the corpus does (ownerwords.ClaudePastes).
func ClaudePastes(home string) ([]string, error) { return ownerwords.ClaudePastes(home) }

// HarnessText reports text a harness injected into a user turn.
func HarnessText(f Format, txt string) bool { return digest.HarnessText(f, txt) }

// Compacted reports transcript lines that hold a compaction entry.
func Compacted(b []byte, f Format) bool { return digest.Compacted(b, f) }

// AgentCwd reports a working directory only agents start sessions in.
func AgentCwd(cwd string) bool { return digest.AgentCwd(cwd) }

// OwnerPart is what of a user turn's text is the owner's.
func OwnerPart(text string) (string, bool) { return ownerwords.OwnerPart(text) }

// PastedBlob reports a message that is mostly a pasted table or log.
func PastedBlob(t string) bool { return ownerwords.PastedBlob(t) }

// LineTime is a transcript line's own time; zero when it has none.
func LineTime(line []byte) time.Time { return ownerwords.LineTime(line) }

// --- Hermes ---

// HermesDB is a read-only connection to a Hermes store; HermesSession a
// session row, with Owner decided; HermesMessage a message row.
type (
	HermesDB      = hermes.DB
	HermesSession = hermes.Session
	HermesMessage = hermes.Message
)

// OpenHermes opens a Hermes store read-only, never writing it.
func OpenHermes(path string) (*HermesDB, error) { return hermes.Open(path) }

// DefaultHermesDB is the Hermes store tacit reads under home ("" for none).
func DefaultHermesDB(home string) string { return hermes.DefaultDB(home) }

// HermesContinued reports an end reason whose conversation goes on in a
// child session.
func HermesContinued(endReason string) bool { return hermes.Continued(endReason) }

// --- Repositories ---

// Repo is a repository as tacit names it; RepoCache resolves paths to
// repositories with a cache; RepoResolver names the repositories texts
// mention; RepoMains are this machine's main checkouts, which name a
// separate clone (RepoResolver.WithMains, RepoCache.Mains).
type (
	Repo         = repoid.Repo
	RepoCache    = repoid.Cache
	RepoResolver = repoinfer.Resolver
	RepoMains    = repoid.Mains
)

// ProjectsDir is the folder of checkouts for home: $TACIT_PROJECTS, else
// home/projects (repoid.ProjectsDir).
func ProjectsDir(home string) string { return repoid.ProjectsDir(home) }

// LoadRepoMains scans projects ("" for none) for home's main checkouts
// (repoid.LoadMains).
func LoadRepoMains(home, projects string) *RepoMains { return repoid.LoadMains(home, projects) }

// RepoOf is the repository a path is in (a real git root; a worktree is its
// main repository), and false when there is none (repoid.Of).
func RepoOf(path, home string) (Repo, bool) { return repoid.Of(path, home) }

// RepoOfMachine is RepoOf with a separate clone named for its main checkout
// in the projects directory (repoid.OfMachine).
func RepoOfMachine(path, home string) (Repo, bool) { return repoid.OfMachine(path, home) }

// NewRepoCache answers RepoOf for one home directory, remembering each
// directory it looked at (repoid.NewCache).
func NewRepoCache(home string) *RepoCache { return repoid.NewCache(home) }

// NewRepoResolver names the repositories of paths under home, outside its
// dot directories (repoinfer.NewResolver).
func NewRepoResolver(home string) *RepoResolver { return repoinfer.NewResolver(home) }

// RepoCounts tallies the repositories the paths in texts belong to, a path
// the agent works in counting more, each multiplied by weight
// (repoinfer.Counts).
func RepoCounts(texts []string, home string, weight int, into map[string]int) map[string]int {
	return repoinfer.Counts(texts, home, weight, into)
}

// DominantRepo is the repository with the most mentions when it has at
// least minCount of them and three quarters of all; "" when the activity is
// spread (repoinfer.Dominant).
func DominantRepo(counts map[string]int, minCount int) string {
	return repoinfer.Dominant(counts, minCount)
}

// --- Redaction (fails closed) ---

// RedactErr masks secret-shaped text with scrump's text profile; an error
// means the text must not be used.
func RedactErr(s string) (string, error) { return digest.RedactErr(s) }

// RedactAllContext redacts several texts in one scrump call, each masked as
// it would be alone, stopping when ctx ends; an error means none of them may
// be used.
func RedactAllContext(ctx context.Context, texts []string) ([]string, error) {
	return digest.RedactAllContext(ctx, texts)
}

// RedactRecordsContext is RedactAllContext with each text a record of its
// own in scrump's records mode (`scrump --records`), and no fallback: a
// scrump without records mode gives ErrNoRecords, for a caller that has
// its own way to keep texts apart.
func RedactRecordsContext(ctx context.Context, texts []string) ([]string, error) {
	return digest.RedactRecordsContext(ctx, texts)
}

// ErrNoRecords is RedactRecordsContext's error when the installed scrump
// has no records mode.
var ErrNoRecords = digest.ErrNoRecords

// RedactionReady proves scrump with the text profile works on this machine.
func RedactionReady() error { return digest.RedactionReady() }

// --- Model runs ---

// Route is a runner, model, effort and profile; Result a run's answer and
// its accounting; Trace collects the calls of a context, each a CallRecord.
type (
	Route      = modelrun.Route
	Result     = modelrun.Result
	Trace      = modelrun.Trace
	CallRecord = modelrun.CallRecord
)

// ErrInvalidResult is a model answer that does not parse.
var ErrInvalidResult = modelrun.ErrInvalidResult

// Run runs one model call through the role's route, in a private working
// directory (modelrun.Run).
func Run(ctx context.Context, role, model, prompt, sessionID string) (Result, error) {
	return modelrun.Run(ctx, role, model, prompt, sessionID)
}

// RunWithFallback is Run that falls back to the role's fallback route when
// the first fails in a way another route can answer (modelrun.RunWithFallback).
func RunWithFallback(ctx context.Context, role, model, prompt, sessionID string) (Result, error) {
	return modelrun.RunWithFallback(ctx, role, model, prompt, sessionID)
}

// WithRoute makes calls under ctx take route r.
func WithRoute(ctx context.Context, r Route) context.Context { return modelrun.WithRoute(ctx, r) }

// WithJSONSchema asks the Claude runner for output valid against schema.
func WithJSONSchema(ctx context.Context, schema string) context.Context {
	return modelrun.WithJSONSchema(ctx, schema)
}

// WithTrace records the calls under ctx in trace.
func WithTrace(ctx context.Context, trace *Trace) context.Context {
	return modelrun.WithTrace(ctx, trace)
}

// Preflight checks a route's arguments and configuration without a call.
func Preflight(route Route) error { return modelrun.Preflight(route) }

// NewSessionID is a fresh id for a model call's session.
func NewSessionID() string { return modelrun.NewSessionID() }

// Exclude lists sessions tacit's dreamer must never read: a model call whose
// prompt holds the owner's evidence would come back as the owner's work.
func Exclude(ids ...string) error { return modelrun.Exclude(ids...) }

// RunExcluded is RunWithFallback in a session the dreamer never reads: the
// session is named and listed (Exclude) before the call, and the id the
// runner reports listed after it. It fails closed: when the list cannot be
// written it makes no call.
func RunExcluded(ctx context.Context, role, model, prompt string) (Result, error) {
	sid := NewSessionID()
	if err := Exclude(sid); err != nil {
		return Result{}, err
	}
	res, err := RunWithFallback(ctx, role, model, prompt, sid)
	if res.SessionID != "" && res.SessionID != sid {
		if xerr := Exclude(res.SessionID); xerr != nil && err == nil {
			err = xerr
		}
	}
	return res, err
}
