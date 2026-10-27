// Package transcript finds and reads the harnesses' JSONL transcripts, plain
// or compressed. Codex rolls an old session into a compressed copy of it,
// rollout-<time>-<id>.jsonl.zst, and removes the .jsonl (on one workstation,
// 2026-10-27: 2,935 of 3,066 Codex sessions, 7 GB, every one a single zstd
// frame with its size recorded). No harness on that machine writes a gzip
// copy.
//
// A transcript is one session whichever file holds it: its identity is its
// plain path (Plain), and every offset is a position in its JSONL bytes (the
// decompressed stream of a compressed copy), so a cursor recorded while the
// session was a .jsonl still holds once it is a .jsonl.zst. Reads stream: a
// compressed copy is never held whole in memory.
package transcript

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Ext ends a transcript's name.
const Ext = ".jsonl"

// Zst is the ending a compressed copy adds to it.
const Zst = ".zst"

// maxWindow bounds what one compressed copy may make the decoder hold: its
// window (Codex writes 2 MiB windows). A copy that asks for more is not read.
const maxWindow = 128 << 20

// Plain is a transcript path less a compression ending: the name the session
// has whichever file holds it.
func Plain(path string) string { return strings.TrimSuffix(path, Zst) }

// Compressed reports a compressed copy.
func Compressed(path string) bool { return strings.HasSuffix(path, Ext+Zst) }

// Name is a transcript's file name less its endings (".jsonl", ".jsonl.zst").
func Name(path string) string {
	return strings.TrimSuffix(filepath.Base(Plain(path)), Ext)
}

// Match reports whether a transcript (plain or compressed) matches a glob of
// plain transcripts (one ending in *.jsonl).
func Match(pattern, path string) (bool, error) {
	return filepath.Match(pattern, Plain(path))
}

// Glob is filepath.Glob for a pattern of plain transcripts (ending in .jsonl),
// with their compressed copies: one path per transcript, the plain file when
// both are there (the harness is still writing it, or compressing it).
func Glob(pattern string) ([]string, error) {
	plain, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(pattern, Ext) {
		return plain, nil
	}
	packed, err := filepath.Glob(pattern + Zst)
	if err != nil {
		return nil, err
	}
	if len(packed) == 0 {
		return plain, nil
	}
	have := make(map[string]bool, len(plain))
	for _, p := range plain {
		have[p] = true
	}
	out := plain
	for _, p := range packed {
		if !have[Plain(p)] {
			out = append(out, p)
		}
	}
	return out, nil
}

// Resolve is the file that holds a transcript now: path, or, when path names
// a plain transcript that is gone, its compressed copy when there is one (the
// harness compressed it after its path was recorded).
func Resolve(path string) string {
	if Compressed(path) || !strings.HasSuffix(path, Ext) {
		return path
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return path
	}
	if fi, err := os.Stat(path + Zst); err == nil && fi.Mode().IsRegular() {
		return path + Zst
	}
	return path
}

// reader is a compressed copy's decompressed stream.
type reader struct {
	io.Reader
	dec *zstd.Decoder
	fh  *os.File
}

func (r *reader) Close() error {
	r.dec.Close()
	return r.fh.Close()
}

// Open reads a transcript's JSONL bytes from the start (Resolve).
func Open(path string) (io.ReadCloser, error) { return OpenAt(path, 0) }

// OpenAt reads a transcript's JSONL bytes from offset off: a plain file is
// seeked, a compressed copy decompressed and the first off bytes passed over
// (a zstd frame cannot be entered in the middle).
func OpenAt(path string, off int64) (io.ReadCloser, error) {
	path = Resolve(path)
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !Compressed(path) {
		if off > 0 {
			if _, err := fh.Seek(off, io.SeekStart); err != nil {
				fh.Close()
				return nil, err
			}
		}
		return fh, nil
	}
	dec, err := zstd.NewReader(bufio.NewReaderSize(fh, 256<<10),
		zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(maxWindow))
	if err != nil {
		fh.Close()
		return nil, err
	}
	r := &reader{Reader: dec, dec: dec, fh: fh}
	if off > 0 {
		if n, err := io.CopyN(io.Discard, dec, off); err != nil {
			r.Close()
			if err == io.EOF {
				return nil, fmt.Errorf("%s: offset %d is past its end (%d)", path, off, n)
			}
			return nil, err
		}
	}
	return r, nil
}

// Head is a transcript's first n bytes (fewer when it is shorter); nil when it
// cannot be read.
func Head(path string, n int) []byte {
	r, err := Open(path)
	if err != nil {
		return nil
	}
	defer r.Close()
	b := make([]byte, n)
	m, _ := io.ReadFull(r, b)
	return b[:m]
}

// Stat is a transcript's file (Resolve) and its size in JSONL bytes: the
// file's size, or a compressed copy's decompressed size.
func Stat(path string) (os.FileInfo, int64, error) {
	path = Resolve(path)
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if !Compressed(path) || !fi.Mode().IsRegular() {
		return fi, fi.Size(), nil
	}
	n, err := packedSize(path, fi)
	return fi, n, err
}

// Size is Stat's size.
func Size(path string) (int64, error) {
	_, n, err := Stat(path)
	return n, err
}

// sized remembers a compressed copy's size by its file's size and time: a
// run asks for it once per transcript, a hook process again.
var sized sync.Map // path -> sizedEntry

type sizedEntry struct {
	bytes int64
	mod   time.Time
	size  int64
}

func packedSize(path string, fi os.FileInfo) (int64, error) {
	if v, ok := sized.Load(path); ok {
		e := v.(sizedEntry)
		if e.bytes == fi.Size() && e.mod.Equal(fi.ModTime()) {
			return e.size, nil
		}
	}
	n, err := zstdSize(path)
	if err != nil {
		return 0, err
	}
	sized.Store(path, sizedEntry{bytes: fi.Size(), mod: fi.ModTime(), size: n})
	return n, nil
}

// zstdSize is the decompressed size of a zstd file, from its frame headers
// (RFC 8878): each frame's content size where the header records it, its
// blocks passed over by their headers; a frame that records none is
// decompressed and counted. Skippable frames hold no content.
func zstdSize(path string) (int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return 0, err
	}
	end := fi.Size()
	var total, pos int64
	var b [18]byte
	for pos < end {
		if _, err := fh.ReadAt(b[:4], pos); err != nil {
			return 0, fmt.Errorf("%s: frame at %d: %w", path, pos, err)
		}
		magic := binary.LittleEndian.Uint32(b[:4])
		if magic&0xFFFFFFF0 == 0x184D2A50 { // skippable frame
			if _, err := fh.ReadAt(b[:4], pos+4); err != nil {
				return 0, fmt.Errorf("%s: skippable frame at %d: %w", path, pos, err)
			}
			pos += 8 + int64(binary.LittleEndian.Uint32(b[:4]))
			continue
		}
		if magic != 0xFD2FB528 {
			return 0, fmt.Errorf("%s: no zstd frame at %d", path, pos)
		}
		start := pos
		if _, err := fh.ReadAt(b[:1], pos+4); err != nil {
			return 0, fmt.Errorf("%s: frame header at %d: %w", path, pos, err)
		}
		desc := b[0]
		fcsFlag, single, checksum, dictFlag := desc>>6, desc&0x20 != 0, desc&0x04 != 0, desc&0x03
		hdr := pos + 5
		if !single {
			hdr++ // window descriptor
		}
		hdr += [4]int64{0, 1, 2, 4}[dictFlag]
		fcsLen := [4]int{0, 2, 4, 8}[fcsFlag]
		if fcsFlag == 0 && single {
			fcsLen = 1
		}
		size := int64(-1)
		if fcsLen > 0 {
			if _, err := fh.ReadAt(b[:fcsLen], hdr); err != nil {
				return 0, fmt.Errorf("%s: frame header at %d: %w", path, pos, err)
			}
			switch fcsLen {
			case 1:
				size = int64(b[0])
			case 2:
				size = int64(binary.LittleEndian.Uint16(b[:2])) + 256
			case 4:
				size = int64(binary.LittleEndian.Uint32(b[:4]))
			case 8:
				size = int64(binary.LittleEndian.Uint64(b[:8]))
			}
		}
		pos = hdr + int64(fcsLen)
		for {
			if _, err := fh.ReadAt(b[:3], pos); err != nil {
				return 0, fmt.Errorf("%s: block at %d: %w", path, pos, err)
			}
			h := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
			last, typ, n := h&1 != 0, (h>>1)&3, int64(h>>3)
			pos += 3
			switch typ {
			case 0, 2: // raw, compressed: n bytes follow
				pos += n
			case 1: // RLE: one byte follows
				pos++
			default:
				return 0, fmt.Errorf("%s: reserved block type at %d", path, pos-3)
			}
			if last {
				break
			}
		}
		if checksum {
			pos += 4
		}
		if pos > end {
			return 0, fmt.Errorf("%s: frame at %d is cut short", path, start)
		}
		if size < 0 {
			n, err := frameCount(fh, start, pos-start)
			if err != nil {
				return 0, fmt.Errorf("%s: frame at %d: %w", path, start, err)
			}
			size = n
		}
		total += size
	}
	return total, nil
}

// frameCount decompresses one frame and counts its bytes.
func frameCount(fh *os.File, off, n int64) (int64, error) {
	dec, err := zstd.NewReader(io.NewSectionReader(fh, off, n),
		zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(maxWindow))
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	return io.Copy(io.Discard, dec)
}
