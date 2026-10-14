// Command install-smoke exercises real release installers against local artifacts.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, r)
			os.Exit(1)
		}
	}()
	archives := flag.String("archives", "../dist", "release archive directory")
	version := flag.String("version", "v0.1.0", "release tag")
	flag.Parse()
	cache, err := os.UserCacheDir()
	check(err)
	base := filepath.Join(cache, "tacit", "install-tests")
	check(os.MkdirAll(base, 0700))
	root, err := os.MkdirTemp(base, "release-")
	check(err)
	defer os.RemoveAll(root)
	dir, err := filepath.Abs(*archives)
	check(err)
	sums := filepath.Join(dir, "SHA256SUMS")
	original, err := os.ReadFile(sums)
	check(err)
	// Serve a modified copy on the negative path without changing build artifacts.
	var corrupt atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			http.Redirect(w, r, "/tag/"+*version, http.StatusFound)
			return
		}
		if r.URL.Path == "/tag/"+*version {
			fmt.Fprint(w, "release")
			return
		}
		prefix := "/download/" + *version + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		if name == "SHA256SUMS" && corrupt.Load() {
			fmt.Fprint(w, strings.Repeat("0", 64)+"  tacit-"+*version+"-"+runtime.GOOS+"-"+runtime.GOARCH+extension()+"\n")
			return
		}
		if name == "SHA256SUMS" {
			w.Write(original)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(name)))
	}))
	defer server.Close()
	binDir := filepath.Join(root, "bin with spaces")
	run := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "../scripts/install.ps1", "-Version", "latest", "-BinDir", binDir, "-ReleaseBaseUrl", server.URL)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "../scripts/install.sh", "--bin-dir", binDir)
			cmd.Env = append(os.Environ(), "TACIT_RELEASE_BASE_URL="+server.URL)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("installer: %w: %s", err, out)
		}
		return nil
	}
	check(run())
	check(run())
	exe := "tacit"
	redactor := "scrump"
	if runtime.GOOS == "windows" {
		exe += ".exe"
		redactor += ".exe"
	}
	tacit := filepath.Join(binDir, exe)
	out, err := exec.Command(tacit, "--version").CombinedOutput()
	check(err)
	if strings.TrimSpace(string(out)) != "tacit "+*version {
		check(fmt.Errorf("installed version: %s", out))
	}
	hash := func(p string) [32]byte { b, e := os.ReadFile(p); check(e); return sha256.Sum256(b) }
	a, b := hash(tacit), hash(filepath.Join(binDir, redactor))
	corrupt.Store(true)
	if run() == nil {
		check(fmt.Errorf("corrupt checksum was accepted"))
	}
	if hash(tacit) != a || hash(filepath.Join(binDir, redactor)) != b {
		check(fmt.Errorf("checksum failure changed installed executables"))
	}
	fmt.Printf("%s/%s: install, reinstall and latest resolution passed; corrupt archive refused; existing executables preserved\n", runtime.GOOS, runtime.GOARCH)
}
func extension() string {
	if runtime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
