// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/nwaples/rardecode/v2"
)

// fake is an archive the test writes itself. The decoder is behind the scanner
// interface precisely so the shape of this package can be exercised without a
// binary fixture nobody can read: what is under test here is the index, the
// contract and the short-read guard, none of which is RAR decoding.
type fake struct {
	hdrs []*rardecode.FileHeader
	data [][]byte
	i    int
	r    io.Reader
}

func (f *fake) Next() (*rardecode.FileHeader, error) {
	if f.i >= len(f.hdrs) {
		return nil, io.EOF
	}
	h := f.hdrs[f.i]
	f.r = bytes.NewReader(f.data[f.i])
	f.i++
	return h, nil
}

func (f *fake) Read(p []byte) (int, error) {
	if f.r == nil {
		return 0, io.EOF
	}
	return f.r.Read(p)
}

func (f *fake) Close() error { return nil }

func archive(t *testing.T, entries ...any) *FS {
	t.Helper()
	mk := func() (scanner, error) {
		f := &fake{}
		for i := 0; i < len(entries); i += 2 {
			f.hdrs = append(f.hdrs, entries[i].(*rardecode.FileHeader))
			f.data = append(f.data, entries[i+1].([]byte))
		}
		return f, nil
	}
	fs, err := index(mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func file(name string, data []byte) (*rardecode.FileHeader, []byte) {
	return &rardecode.FileHeader{Name: name, UnPackedSize: int64(len(data))}, data
}

// TestIndexNamesTheDirectoriesTheArchiveDidNot: an archive is a list of paths,
// not a tree. Whether a parent gets an entry of its own is up to whatever wrote
// it, so a reader that registers only what it was told cannot list half of what
// it holds.
func TestIndexNamesTheDirectoriesTheArchiveDidNot(t *testing.T) {
	h1, d1 := file("film/2024/part one.mkv", []byte("aaaa"))
	h2, d2 := file("film/2024/part two.mkv", []byte("bbbbbb"))
	h3, d3 := file("notes.txt", []byte("hello"))
	fs := archive(t, h1, d1, h2, d2, h3, d3)

	top, err := fs.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range top {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "film" || names[1] != "notes.txt" {
		t.Errorf("the root lists %v, want [film notes.txt]", names)
	}

	// The middle directory was never named by the archive either.
	mid, err := fs.ListDir("film")
	if err != nil {
		t.Fatalf("film: %v", err)
	}
	if len(mid) != 1 || mid[0].Name() != "2024" || mid[0].FileType() != 2 {
		t.Errorf("film/ lists %d entries, want one directory named 2024", len(mid))
	}
	if _, err := fs.ListDir("film/2024"); err != nil {
		t.Errorf("film/2024: %v", err)
	}
	if _, err := fs.ListDir("notes.txt"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("listing a file gave %v, want ErrNotDirectory", err)
	}
	if _, err := fs.Stat("nothing/here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat of an absent path gave %v, want ErrNotFound", err)
	}
}

// TestReadAtGoesForwardsAndBackwards covers the thing that costs: the decoder
// is a stream, so reading on is cheap and reading back starts again. Both must
// give the same bytes.
func TestReadAtGoesForwardsAndBackwards(t *testing.T) {
	const body = "0123456789abcdef"
	h, d := file("a/b.bin", []byte(body))
	fs := archive(t, h, d)

	fh, err := fs.OpenFile("a/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if fh.Size() != int64(len(body)) {
		t.Errorf("Size() = %d, want %d", fh.Size(), len(body))
	}
	for _, c := range []struct {
		off  int64
		want string
	}{
		{0, "0123"},
		{4, "4567"},  // forwards: the same pass carries on
		{12, "cdef"}, // still forwards
		{0, "0123"},  // backwards: a fresh pass, same answer
		{8, "89ab"},
	} {
		buf := make([]byte, len(c.want))
		n, err := fh.ReadAt(buf, c.off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt(%d): %v", c.off, err)
		}
		if got := string(buf[:n]); got != c.want {
			t.Errorf("ReadAt(%d) = %q, want %q", c.off, got, c.want)
		}
	}
	if _, err := fh.ReadAt(make([]byte, 4), int64(len(body))); !errors.Is(err, io.EOF) {
		t.Errorf("reading past the end gave %v, want io.EOF", err)
	}
}

// TestAShortEntryIsNamedNotCounted is the defect this package was written
// against, in miniature: an entry that ends before its declared size.
//
// Reported as a smaller count with a nil error, it is invisible -- the caller
// writes what it got, the file is short or, worse, padded to the declared
// length by whatever wrote it, and every size check afterwards agrees. So it
// has a name.
func TestAShortEntryIsNamedNotCounted(t *testing.T) {
	// The header promises sixteen bytes; the archive holds four.
	h := &rardecode.FileHeader{Name: "truncated.bin", UnPackedSize: 16}
	fs := archive(t, h, []byte("0123"))

	fh, err := fs.OpenFile("truncated.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	buf := make([]byte, 16)
	n, err := fh.ReadAt(buf, 0)
	if !errors.Is(err, ErrShort) {
		t.Errorf("a truncated entry gave (%d, %v), want ErrShort", n, err)
	}
	if n != 4 {
		t.Errorf("read %d bytes, want the 4 that were there", n)
	}

	// ReadFile carries it too: the convenience must not be the quiet path.
	if _, err := fs.ReadFile("truncated.bin"); !errors.Is(err, ErrShort) {
		t.Errorf("ReadFile gave %v, want ErrShort", err)
	}
}

// TestTheMutatingHalfRefuses: an archive is read-only, and says so by name.
func TestTheMutatingHalfRefuses(t *testing.T) {
	h, d := file("a.txt", []byte("x"))
	fs := archive(t, h, d)
	for name, err := range map[string]error{
		"WriteFile":  fs.WriteFile("a.txt", nil, 0),
		"MkDir":      fs.MkDir("d", 0),
		"DeleteFile": fs.DeleteFile("a.txt"),
		"DeleteDir":  fs.DeleteDir("d"),
		"Rename":     fs.Rename("a.txt", "b.txt"),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s gave %v, want ErrReadOnly", name, err)
		}
	}
}
