// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/nwaples/rardecode/v2"
)

// The member index.
//
// A sequential scan restarts at the first header for every handle, so reading
// n members costs n*n/2 header parses. For a NON-SOLID entry the decoder can
// instead open the member directly from the block list rardecode.List builds
// in one extra pass; that list is built once, lazily, at the first handle that
// needs it, and holds one header per entry -- the same count Limits.MaxEntries
// already bounded when the archive was indexed, so it grows with nothing the
// archive did not already pay for.
//
// Solid entries stay on the sequential path: their decoder state depends on
// every file before them, so they legitimately need the pass. Any failure of
// the indexed path (a missing volume, an unexpected name at the ordinal)
// falls back to that pass, so the error a caller sees is the one it always
// saw.

// memberIndex is the lazily built list of members.
type memberIndex struct {
	once  sync.Once
	files []*rardecode.File
}

// lookupMember returns the indexed member for e, or nil when e must be
// reached by a sequential pass.
func (f *FS) lookupMember(e *entry) *rardecode.File {
	if f.list == nil || e.solid || e.ordinal < 0 {
		return nil
	}
	f.members.once.Do(func() {
		if files, err := f.list(); err == nil {
			f.members.files = files
		}
	})
	if e.ordinal >= len(f.members.files) {
		return nil
	}
	m := f.members.files[e.ordinal]
	if clean(m.Name) != e.path {
		return nil
	}
	return m
}

// memberScanner adapts one opened member to scanner. It is already positioned
// at the member's first byte, so Next is never asked for.
type memberScanner struct{ io.ReadCloser }

func (memberScanner) Next() (*rardecode.FileHeader, error) { return nil, io.EOF }

// readerFS serves a single io.ReaderAt as the one volume the decoder names.
type readerFS struct {
	r    io.ReaderAt
	size int64
}

const readerVolume = "archive.rar"

func (r readerFS) Open(name string) (fs.File, error) {
	if name != readerVolume {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &sectionFile{SectionReader: io.NewSectionReader(r.r, 0, r.size), size: r.size}, nil
}

type sectionFile struct {
	*io.SectionReader
	size int64
}

func (s *sectionFile) Close() error               { return nil }
func (s *sectionFile) Stat() (fs.FileInfo, error) { return sectionInfo(s.size), nil }

type sectionInfo int64

func (s sectionInfo) Name() string       { return readerVolume }
func (s sectionInfo) Size() int64        { return int64(s) }
func (s sectionInfo) Mode() fs.FileMode  { return 0o444 }
func (s sectionInfo) ModTime() time.Time { return time.Time{} }
func (s sectionInfo) IsDir() bool        { return false }
func (s sectionInfo) Sys() any           { return nil }
