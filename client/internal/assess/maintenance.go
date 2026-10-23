package assess

// What counts as the owner changing their mind about a folder they said
// was dead, done or paused (the owner, 2026-10-23: "activity is not merely
// commit. the commit might be just a dependabot or security fix"): real
// work on it after the day they said so. Code decides it from what each
// piece of activity is, never from a commit message's words: a session
// that worked in the checkout is work; a commit is work when its diff
// changes more than maintenance files (dependency manifests and lockfiles,
// CI and workflow configuration, version and changelog files), and a commit
// a bot authored is maintenance whatever it changes. A merge's diff is its
// first parent's, so merging a bot's dependency bump is maintenance too.

import (
	"path"
	"strings"
)

// maintenanceNames are the base names of the files a maintenance commit
// changes: dependency manifests and lockfiles, version and changelog files,
// tool and runtime version pins.
var maintenanceNames = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`
		package.json package-lock.json npm-shrinkwrap.json yarn.lock pnpm-lock.yaml pnpm-workspace.yaml bun.lock bun.lockb deno.lock .npmrc .yarnrc .yarnrc.yml
		go.mod go.sum go.work go.work.sum
		cargo.toml cargo.lock rust-toolchain rust-toolchain.toml
		pyproject.toml poetry.lock pipfile pipfile.lock uv.lock pdm.lock setup.cfg
		gemfile gemfile.lock composer.json composer.lock mix.exs mix.lock pubspec.yaml pubspec.lock package.resolved podfile.lock
		gradle.lockfile flake.lock
		.nvmrc .node-version .python-version .ruby-version .tool-versions .terraform.lock.hcl
		version version.txt .release-please-manifest.json release-please-config.json
		dependabot.yml dependabot.yaml renovate.json renovate.json5 .renovaterc .renovaterc.json
		.gitlab-ci.yml .travis.yml azure-pipelines.yml appveyor.yml jenkinsfile .pre-commit-config.yaml codecov.yml .codecov.yml`) {
		m[n] = true
	}
	return m
}()

// maintenanceDirs are the directories whose files are CI and workflow
// configuration.
var maintenanceDirs = []string{".github/", ".circleci/", ".buildkite/", ".gitlab/", ".azure-pipelines/"}

// MaintenancePath reports a file whose change is maintenance: a dependency
// manifest or lockfile (requirements*.txt, constraints*.txt included), CI or
// workflow configuration, a version or changelog file. A plugin's
// plugin.json or marketplace.json counts only inside a hidden directory
// (.claude-plugin/plugin.json: its version), not as a source file.
func MaintenancePath(p string) bool {
	p = strings.TrimPrefix(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), "./")
	if p == "" {
		return false
	}
	for _, d := range maintenanceDirs {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	base := path.Base(p)
	if base == "plugin.json" || base == "marketplace.json" {
		dir := path.Dir(p)
		return dir != "." && strings.HasPrefix(path.Base(dir), ".")
	}
	if maintenanceNames[base] {
		return true
	}
	stem, ext := base, path.Ext(base)
	switch ext {
	case ".md", ".txt", ".rst", ".adoc":
		stem = strings.TrimSuffix(base, ext)
	case "":
	default:
		stem = ""
	}
	switch stem {
	case "changelog", "changes", "history", "news", "release-notes", "release_notes", "releases":
		return true
	}
	if ext := path.Ext(base); ext == ".txt" || ext == ".in" {
		stem := strings.TrimSuffix(base, ext)
		if strings.HasPrefix(stem, "requirements") || strings.HasPrefix(stem, "constraints") {
			return true
		}
	}
	return false
}

// BotAuthor reports a commit author that is a bot: an account or name with
// the forge's bot suffix ("[bot]"), or the dependency and CI bots that
// commit under a plain name.
func BotAuthor(names ...string) bool {
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if strings.Contains(n, "[bot]") {
			return true
		}
		switch strings.TrimSuffix(strings.SplitN(n, "@", 2)[0], "-bot") {
		case "dependabot", "dependabot-preview", "renovate", "github-actions", "greenkeeper", "snyk-bot", "snyk":
			return true
		}
	}
	return false
}

// RealFiles are the files of a diff whose change is more than maintenance,
// in order; none: the commit is maintenance.
func RealFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if !MaintenancePath(f) {
			out = append(out, f)
		}
	}
	return out
}

// Work is one piece of the owner's activity on a folder after a day, as
// code classified it: Kind is "session" (a session worked in its checkout or
// folder), "commit" (a commit here or on the forge) or "catalog" (another
// machine's catalog's day); Real reports work, not maintenance; Why says
// what code read (the files that are not maintenance, "a bot's commit",
// "only maintenance files: go.mod, go.sum").
type Work struct {
	Day, Kind, Cite, Why string
	Real                 bool
}

// Line is how a why or the review item shows it.
func (w Work) Line() string {
	s := w.Day + " " + w.Kind
	if w.Cite != "" {
		s += " " + w.Cite
	}
	if w.Why != "" {
		s += " (" + w.Why + ")"
	}
	return s
}

// Changes is what code found of the owner's activity on a folder after a
// day: Real the work, Maintenance the rest; Unknown are days code could not
// classify (another machine's commit no forge or checkout here shows), which
// prove nothing either way.
type Changes struct {
	Real, Maintenance, Unknown []Work
}

// Changed reports real work after the day: the owner changed their mind.
func (ch Changes) Changed() bool { return len(ch.Real) > 0 }

// Summary is the classification in one clause, at most n items of each.
func (ch Changes) Summary(n int) string {
	list := func(ws []Work) string {
		var parts []string
		for i, w := range ws {
			if i == n {
				parts = append(parts, "...")
				break
			}
			parts = append(parts, w.Line())
		}
		return strings.Join(parts, ", ")
	}
	var parts []string
	if len(ch.Real) > 0 {
		parts = append(parts, "real work: "+list(ch.Real))
	}
	if len(ch.Maintenance) > 0 {
		parts = append(parts, "maintenance only: "+list(ch.Maintenance))
	}
	if len(ch.Unknown) > 0 {
		parts = append(parts, "not classifiable here: "+list(ch.Unknown))
	}
	if len(parts) == 0 {
		return "no activity of the owner's on it after that day"
	}
	return strings.Join(parts, "; ")
}
