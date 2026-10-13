// Command dist builds portable CLI/browser-app archives without a desktop SDK.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	out := flag.String("out", "../dist", "archive output directory")
	version := flag.String("version", "dev", "archive version label")
	flag.Parse()
	if strings.ContainsAny(*version, "/\\\n\r") {
		fail(fmt.Errorf("version must be a filename label"))
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	var sums strings.Builder
	for _, target := range []struct{ os, arch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}} {
		dir, err := os.MkdirTemp(*out, ".build-")
		if err != nil {
			fail(err)
		}
		name := "tacit"
		if target.os == "windows" {
			name += ".exe"
		}
		bin := filepath.Join(dir, name)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-trimpath", "-ldflags=-s -w", "-o", bin, "./cmd/tacit")
		cmd.Env = append(os.Environ(), "GOOS="+target.os, "GOARCH="+target.arch, "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		cancel()
		if err != nil {
			os.RemoveAll(dir)
			fail(err)
		}
		stem := "tacit-" + *version + "-" + target.os + "-" + target.arch
		ext := ".tar.gz"
		if target.os == "windows" {
			ext = ".zip"
		}
		path := filepath.Join(*out, stem+ext)
		files := map[string]string{name: bin, "LICENSE": "../LICENSE", "NOTICE": "../NOTICE", "README.md": "../README.md"}
		for _, font := range []string{"OFL-AtkinsonHyperlegibleNext.txt", "OFL-Literata.txt"} {
			files["client/cmd/tacit/reviewui/fonts/"+font] = "cmd/tacit/reviewui/fonts/" + font
		}
		if err := archive(path, stem, files, target.os == "windows"); err != nil {
			os.RemoveAll(dir)
			fail(err)
		}
		os.RemoveAll(dir)
		b, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(b), filepath.Base(path))
		fmt.Println(path)
	}
	if err := os.WriteFile(filepath.Join(*out, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		fail(err)
	}
}

func archive(path, stem string, files map[string]string, windows bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if windows {
		z := zip.NewWriter(f)
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			source := files[name]
			b, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			h := &zip.FileHeader{Name: stem + "/" + name, Method: zip.Deflate}
			h.SetMode(0o644)
			w, err := z.CreateHeader(h)
			if err != nil {
				return err
			}
			if _, err = w.Write(b); err != nil {
				return err
			}
		}
		return z.Close()
	}
	g := gzip.NewWriter(f)
	t := tar.NewWriter(g)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		source := files[name]
		st, err := os.Stat(source)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: stem + "/" + name, Mode: 0o644, Size: st.Size()}
		if name == "tacit" {
			h.Mode = 0o755
		}
		if err := t.WriteHeader(h); err != nil {
			return err
		}
		r, err := os.Open(source)
		if err != nil {
			return err
		}
		_, err = io.Copy(t, r)
		r.Close()
		if err != nil {
			return err
		}
	}
	if err := t.Close(); err != nil {
		return err
	}
	return g.Close()
}

func fail(err error) { fmt.Fprintln(os.Stderr, "dist:", err); os.Exit(1) }
