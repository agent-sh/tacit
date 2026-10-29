package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/store"
)

func installCurator(goos, home, exe string, args []string) int {
	fs := flag.NewFlagSet("install curate", flag.ContinueOnError)
	env := fs.String("env-file", defaultDreamEnv(goos, home), "provider KEY=VALUE file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	envFile, err := filepath.Abs(*env)
	if err != nil {
		return 2
	}
	s, err := store.Default()
	if err == nil && s.Exists() && s.ReadOnly(context.Background()) {
		fmt.Fprintln(os.Stderr, "install:", store.ErrReadOnly)
		return 1
	}
	if goos == "windows" {
		path, err := installWindowsJob(home, exe, "curate", envFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		printWindowsJob(path, "curate")
		return 0
	}
	if goos == "darwin" {
		label := "ai.tacit.curate"
		text := dreamPlist(exe, envFile, os.Getenv("TACIT_STORE"), home, servicePATH(home, exe, lookPath))
		text = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text, "ai.tacit.dream", "ai.tacit.curate"), "<string>dream</string>", "<string>maintain</string>"), "<integer>14400</integer>", "<integer>86400</integer>")
		text = strings.ReplaceAll(text, "dream.log", "curate.log")
		path, err := installLaunchAgent(home, label, text)
		if err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
		fmt.Println("wrote", path)
		for _, l := range launchctlLines(label, path) {
			fmt.Println(l)
		}
		return 0
	}
	dir := systemdUserDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	unit := "[Unit]\nDescription=tacit daily curation and verification\n[Service]\nType=oneshot\nEnvironment=PATH=" + unitQuote(servicePATH(home, exe, lookPath), true) + "\nExecStart=" + unitQuote(exe, false) + " maintain --env-file " + unitQuote(envFile, false) + "\n"
	for _, key := range []string{"TACIT_STORE", "TACIT_SETTINGS"} {
		if value := os.Getenv(key); value != "" {
			unit += "Environment=" + unitQuote(key+"="+value, true) + "\n"
		}
	}
	if err := writeFileMode(filepath.Join(dir, "tacit-curate.service"), []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	timer := "[Unit]\nDescription=tacit curator daily\n[Timer]\nOnCalendar=*-*-* 05:30:00\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	if err := writeFileMode(filepath.Join(dir, "tacit-curate.timer"), []byte(timer), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Println("start it: systemctl --user daemon-reload && systemctl --user enable --now tacit-curate.timer")
	return 0
}
