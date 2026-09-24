// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"errors"
	"io"
	"os"
	"sort"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// entry is one path the archive holds. A directory with ordinal -1 was not
// named by the archive and is here because something inside it was.
type entry struct {
	path       string
	name       string
	dir        bool
	size       int64
	sizeKnown  bool
	solid      bool
	encrypted  bool
	linkTarget string
	modTime    time.Time
	ordinal    int
}

// FS reads a RAR archive as a filesystem.
//
// ⛔ Random access costs what the format costs. A RAR entry is a STREAM: to
// reach byte n the decoder must produce the n bytes before it, and in a SOLID
// archive it must start from the beginning of the compression block, which may
// be an earlier file. A handle therefore keeps its decoder and its position:
// reading forwards is linear overall, reading BACKWARDS starts a fresh pass.
// Sequential use is what this is good at; a caller seeking about in a large
// entry is paying for the archive's own shape, not for this code.
type FS struct {
	open   func() (scanner, error)
	vols   *Volumes
	byPath map[string]*entry
	kids   map[string][]string
}

// Volumes is the volume set this archive was opened from, or nil when it was
// opened from a single reader. A caller can ask it what it found -- Count,
// Numbers -- to see a hole before an extraction runs into it.
func (f *FS) VolumeSet() *Volumes { return f.vols }

// Close releases nothing: each read opens and closes its own pass.
func (f *FS) Close() error { return nil }

func (f *FS) lookup(p string) (*entry, error) {
	e, ok := f.byPath[clean(p)]
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

// ListDir names what is directly inside p.
func (f *FS) ListDir(p string) ([]filesystem.DirEntry, error) {
	cp := clean(p)
	if cp != "" && cp != "." {
		e, err := f.lookup(cp)
		if err != nil {
			return nil, err
		}
		if !e.dir {
			return nil, ErrNotDirectory
		}
	} else {
		cp = "."
	}
	kids := append([]string(nil), f.kids[cp]...)
	sort.Strings(kids)
	out := make([]filesystem.DirEntry, 0, len(kids))
	for _, k := range kids {
		e := f.byPath[k]
		if e == nil {
			continue
		}
		var ftype uint8
		if e.dir {
			ftype = 2
		}
		out = append(out, filesystem.NewDirEntry(uint64(e.ordinal+1), e.name, ftype))
	}
	return out, nil
}

// Stat reports what the archive recorded about p.
func (f *FS) Stat(p string) (filesystem.Stat, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(uint16(e.mode()), uint64(e.size), uint64(e.ordinal+1)), nil
}

func (e *entry) mode() os.FileMode {
	switch {
	case e.dir:
		return os.ModeDir | 0o555
	case e.linkTarget != "":
		return os.ModeSymlink | 0o777
	default:
		return 0o444
	}
}

// ReadLink answers for an entry the archive recorded a link target for.
func (f *FS) ReadLink(p string) (string, error) {
	e, err := f.lookup(p)
	if err != nil {
		return "", err
	}
	if e.linkTarget == "" {
		return "", ErrNotSymlink
	}
	return e.linkTarget, nil
}

// ReadFile returns the whole entry.
//
// It is the contract's method and it holds the entry in memory, which for a
// video inside an archive is the whole video. Use OpenFile for anything whose
// size you did not choose.
func (f *FS) ReadFile(p string) ([]byte, error) {
	h, err := f.OpenFile(p)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	buf := make([]byte, h.Size())
	n, err := h.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// OpenFile hands back a handle on one entry.
func (f *FS) OpenFile(p string) (filesystem.File, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if e.dir {
		return nil, ErrNotRegular
	}
	return &handle{fs: f, e: e}, nil
}

// The mutating half of filesystem.Filesystem. An archive is read-only, as in
// the other read-only drivers of this org.
func (f *FS) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (f *FS) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (f *FS) DeleteFile(string) error                     { return ErrReadOnly }
func (f *FS) DeleteDir(string) error                      { return ErrReadOnly }
func (f *FS) Rename(string, string) error                 { return ErrReadOnly }

// handle is one open entry: a decoder parked at some offset in its stream.
type handle struct {
	fs  *FS
	e   *entry
	sc  scanner
	pos int64 // how far into the entry sc has been read
}

func (h *handle) Size() int64 { return h.e.size }

func (h *handle) Close() error {
	if h.sc != nil {
		err := h.sc.Close()
		h.sc = nil
		return err
	}
	return nil
}

// seek positions a decoder at off in the entry, starting a new pass when it has
// to. Going forwards discards; going backwards cannot, so it starts again.
func (h *handle) seek(off int64) error {
	if h.sc != nil && off >= h.pos {
		if _, err := io.CopyN(io.Discard, h.sc, off-h.pos); err != nil {
			return err
		}
		h.pos = off
		return nil
	}
	_ = h.Close()
	sc, err := h.fs.open()
	if err != nil {
		return err
	}
	for {
		hdr, err := sc.Next()
		if err != nil {
			sc.Close()
			if errors.Is(err, io.EOF) {
				return ErrNotFound
			}
			return err
		}
		if clean(hdr.Name) == h.e.path {
			break
		}
	}
	h.sc, h.pos = sc, 0
	if off > 0 {
		if _, err := io.CopyN(io.Discard, h.sc, off); err != nil {
			return err
		}
		h.pos = off
	}
	return nil
}

// ReadAt fills p from off, and refuses to invent what the archive did not give.
//
// A short read is reported as ErrShort rather than as a success with a shorter
// count, because a caller that trusts the count and stops looking is exactly
// how a zero-padded file passes for a whole one.
func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("rar: negative offset")
	}
	if h.e.sizeKnown && off >= h.e.size {
		return 0, io.EOF
	}
	if err := h.seek(off); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(h.sc, p)
	h.pos += int64(n)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// The end of the entry inside the buffer is ordinary at the tail of a
		// file. The end of the entry BEFORE its declared size is not.
		if h.e.sizeKnown && off+int64(n) < h.e.size {
			return n, ErrShort
		}
		return n, io.EOF
	default:
		return n, err
	}
}
