//go:build unix

package platform

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrivateAppendRejectsLinksSpecialFilesAndSharedDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenPrivateDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symlink", "hardlink", "fifo", "../outside"} {
		f, err := OpenPrivateAppend(root, name)
		if err == nil {
			f.Close()
			t.Fatalf("unsafe log accepted: %s", name)
		}
	}
	raw, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(raw) != "unchanged" || info.Mode().Perm() != 0644 {
		t.Fatal("target modified")
	}
	f, err := OpenPrivateAppend(root, "safe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err = os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenPrivateDirectory(dir); err == nil {
		f.Close()
		t.Fatal("shared directory accepted")
	}
}

func TestPrivateAppendAndRetentionStayOnPinnedDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "audit")
	moved := filepath.Join(parent, "original")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenPrivateDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "receipt")
	if err := os.WriteFile(target, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivateAppend(root, "receipt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	raw, _ := os.ReadFile(filepath.Join(moved, "receipt"))
	if string(raw) != "original" {
		t.Fatal("append redirected")
	}
	if err := RemovePrivateEntry(root, "receipt"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(target)
	if string(raw) != "replacement" {
		t.Fatal("replacement directory modified")
	}
	if _, err := os.Stat(filepath.Join(moved, "receipt")); !os.IsNotExist(err) {
		t.Fatal("retention redirected")
	}
}
