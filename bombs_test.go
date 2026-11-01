// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/nwaples/rardecode/v2"
)

// Memory bombs. Each archive below is a few dozen bytes to a few megabytes, and
// each was measured BEFORE the fix with TotalAlloc (figures in the comments).

const (
	oneMiB = 1 << 20

	// RAR 5 file flag and compression-word fields (rardecode archive50.go:45-51).
	flagUnknownSize = 0x0008
	compMethod3     = 3 << 7 // a compressing method; 0 would be stored
	compDictShift   = 10     // dictionary = 128 KiB << code
)

// rarEntry is one file block of a hand-built RAR 5 archive.
type rarEntry struct {
	name     string
	flags    uint64 // file flags (fileIsDir, flagUnknownSize ...)
	unpacked uint64
	comp     uint64 // compression-information word; 0 is stored
	packed   []byte
}

// rar5 assembles an archive from the writer's own block encoder, so the headers
// are exactly as valid as the ones the package writes and differ only in the
// field a case sets.
func rar5(entries ...rarEntry) []byte {
	out := append([]byte{}, signature[:]...)
	out = appendBlock(out, blockMain, 0, 0, appendVint(nil, 0))
	for _, e := range entries {
		var b []byte
		b = appendVint(b, e.flags)
		b = appendVint(b, e.unpacked)
		b = appendVint(b, attributes(e.flags&fileIsDir != 0, 0o644))
		b = appendVint(b, e.comp)
		b = appendVint(b, hostOSUnix)
		b = appendVint(b, uint64(len(e.name)))
		b = append(b, e.name...)
		var bf uint64
		var ds int64
		if len(e.packed) > 0 {
			bf, ds = blockHasData, int64(len(e.packed))
		}
		out = appendBlock(out, blockFile, bf, ds, b)
		out = append(out, e.packed...)
	}
	return appendBlock(out, blockEnd, 0, 0, appendVint(nil, 0))
}

func allocated(f func()) uint64 {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func openRaw(raw []byte, lim Limits, opts ...Option) (filesystem.Filesystem, error) {
	return OpenReaderLimits(bytes.NewReader(raw), int64(len(raw)), lim, opts...)
}

// Before: a 56-byte archive naming a 4 GiB dictionary made Open allocate
// 4,294,987,600 bytes, because the decoder allocates the whole window when the
// file header is read (rardecode decode_reader.go:83) and its own default
// ceiling is 4 GiB.
func TestBombDictionaryIsRefusedBeforeItIsAllocated(t *testing.T) {
	raw := rar5(rarEntry{name: "a", unpacked: 4 << 30, comp: compMethod3 | 15<<compDictShift, packed: []byte("0123456789")})
	var err error
	n := allocated(func() { _, err = openRaw(raw, Limits{}) })
	if !errors.Is(err, rardecode.ErrDictionaryTooLarge) {
		t.Fatalf("err = %v, want ErrDictionaryTooLarge", err)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes for a %d-byte archive", n, len(raw))
	}
}

func TestDictionaryLimitIsExact(t *testing.T) {
	at := int64(128<<10) << 3 // dictionary code 3 = 1 MiB
	arc := func(code uint64) []byte {
		return rar5(rarEntry{name: "a", unpacked: uint64(128<<10) << code, comp: compMethod3 | code<<compDictShift, packed: []byte("0123456789")})
	}
	if _, err := openRaw(arc(3), Limits{MaxDictionary: at}); err != nil {
		t.Errorf("a dictionary of exactly the limit: %v", err)
	}
	if _, err := openRaw(arc(4), Limits{MaxDictionary: at}); !errors.Is(err, rardecode.ErrDictionaryTooLarge) {
		t.Errorf("one dictionary step over: %v, want ErrDictionaryTooLarge", err)
	}
	// An Option the caller passes comes after the default and wins, in both directions.
	if _, err := openRaw(arc(4), Limits{}, rardecode.MaxDictionarySize(at)); !errors.Is(err, rardecode.ErrDictionaryTooLarge) {
		t.Errorf("a caller's MaxDictionarySize must apply: %v", err)
	}
	if _, err := openRaw(arc(4), Limits{MaxDictionary: at}, rardecode.MaxDictionarySize(at*2)); err != nil {
		t.Errorf("a caller's MaxDictionarySize must be able to raise the limit: %v", err)
	}
}

func TestDictionaryDefaultAppliesToOpenAsWell(t *testing.T) {
	// Open goes through the same defaults as OpenReader; the file is a stored
	// archive written by this package, so only the wiring is under test.
	raw := rar5(rarEntry{name: "a", unpacked: 4 << 30, comp: compMethod3 | 15<<compDictShift, packed: []byte("0123456789")})
	p := filepath.Join(t.TempDir(), "bomb.rar")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); !errors.Is(err, rardecode.ErrDictionaryTooLarge) {
		t.Errorf("Open: %v, want ErrDictionaryTooLarge", err)
	}
	if _, err := OpenLimits(p, Limits{MaxDictionary: 1 << 20}); !errors.Is(err, rardecode.ErrDictionaryTooLarge) {
		t.Errorf("OpenLimits: %v, want ErrDictionaryTooLarge", err)
	}
}

// Before: a 55-byte stored archive claiming 2 GiB made ReadFile allocate
// 2,147,489,296 bytes before finding the data was 10.
func TestBombReadFileDeclaredSize(t *testing.T) {
	raw := rar5(rarEntry{name: "a", unpacked: 2 << 30, packed: []byte("0123456789")})
	fsys, err := openRaw(raw, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var rerr error
	n := allocated(func() { _, rerr = fsys.ReadFile("a") })
	if !errors.Is(rerr, ErrTooLarge) {
		t.Fatalf("ReadFile = %v, want ErrTooLarge", rerr)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes", n)
	}
	// Streaming is not subject to the ceiling, and reports the lie as a short read.
	h, err := fsys.(*FS).OpenFile("a")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.ReadAt(make([]byte, 64), 0); !errors.Is(err, ErrShort) && !errors.Is(err, rardecode.ErrShortFile) {
		t.Errorf("ReadAt = %v, want a short-read error", err)
	}
}

func TestReadFileCeilingIsExact(t *testing.T) {
	data := []byte("0123456789")
	raw := rar5(rarEntry{name: "a", unpacked: 10, packed: data})
	for _, tc := range []struct {
		max int64
		ok  bool
	}{{10, true}, {9, false}} {
		fsys, err := openRaw(raw, Limits{MaxFileSize: tc.max})
		if err != nil {
			t.Fatal(err)
		}
		got, err := fsys.ReadFile("a")
		if tc.ok && (err != nil || !bytes.Equal(got, data)) {
			t.Errorf("MaxFileSize=%d: %q, %v", tc.max, got, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("MaxFileSize=%d: %v, want ErrTooLarge", tc.max, err)
		}
	}
}

// An entry that does not know its size has no declaration to check, so the
// limit is enforced on what the decoder yields.
func TestReadFileCeilingOnAnUnknownSize(t *testing.T) {
	data := []byte("0123456789")
	raw := rar5(rarEntry{name: "a", flags: flagUnknownSize, unpacked: 0, packed: data})
	for _, tc := range []struct {
		max int64
		ok  bool
	}{{10, true}, {9, false}} {
		fsys, err := openRaw(raw, Limits{MaxFileSize: tc.max})
		if err != nil {
			t.Fatal(err)
		}
		st, err := fsys.Stat("a")
		if err != nil || st.Size() != 0 {
			t.Fatalf("Stat = %v, %v: an unknown size must not be reported as -1 (2^64-1)", st, err)
		}
		got, err := fsys.ReadFile("a")
		if tc.ok && (err != nil || !bytes.Equal(got, data)) {
			t.Errorf("MaxFileSize=%d: %q, %v", tc.max, got, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("MaxFileSize=%d: %v, want ErrTooLarge", tc.max, err)
		}
	}
}

// Before: an unpacked size at or past 2^63 read as negative, and ReadFile
// panicked: "makeslice: len out of range". Found by reading the allocation sites,
// confirmed by a probe.
func TestNegativeSizeIsCorruptNotAPanic(t *testing.T) {
	raw := rar5(rarEntry{name: "a", unpacked: 1 << 63, packed: []byte("0123456789")})
	_, err := openRaw(raw, Limits{})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	// The largest size that is a size is accepted at Open and refused at ReadFile.
	raw = rar5(rarEntry{name: "a", unpacked: 1<<63 - 1, packed: []byte("0123456789")})
	fsys, err := openRaw(raw, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadFile("a"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadFile = %v, want ErrTooLarge", err)
	}
}

func fileEntries(n int) []rarEntry {
	es := make([]rarEntry, n)
	for i := range es {
		es[i] = rarEntry{name: fmt.Sprintf("f%d", i)}
	}
	return es
}

func TestEntryLimitIsExact(t *testing.T) {
	const limit = 50
	if _, err := openRaw(rar5(fileEntries(limit)...), Limits{MaxEntries: limit}); err != nil {
		t.Errorf("%d entries with MaxEntries=%d: %v", limit, limit, err)
	}
	if _, err := openRaw(rar5(fileEntries(limit+1)...), Limits{MaxEntries: limit}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("%d entries: %v, want ErrTooLarge", limit+1, err)
	}
	// Directories an archive implies count: one entry "a/b/c" is three paths.
	if _, err := openRaw(rar5(rarEntry{name: "a/b/c"}), Limits{MaxEntries: 3}); err != nil {
		t.Errorf("three paths with MaxEntries=3: %v", err)
	}
	if _, err := openRaw(rar5(rarEntry{name: "a/b/c"}), Limits{MaxEntries: 2}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("three paths with MaxEntries=2: %v, want ErrTooLarge", err)
	}
}

func TestPathLengthIsExact(t *testing.T) {
	if _, err := openRaw(rar5(rarEntry{name: strings.Repeat("x", maxPathLen)}), Limits{}); err != nil {
		t.Errorf("a %d-byte path: %v", maxPathLen, err)
	}
	if _, err := openRaw(rar5(rarEntry{name: strings.Repeat("x", maxPathLen+1)}), Limits{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a %d-byte path: %v, want ErrTooLarge", maxPathLen+1, err)
	}
	// And an implied parent is charged too: the long path is the parent's.
	long := strings.Repeat("x", maxPathLen+1)
	if _, err := openRaw(rar5(rarEntry{name: long + "/y"}), Limits{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a path whose parent is too long: %v, want ErrTooLarge", err)
	}
}

func TestPathBudgetIsExact(t *testing.T) {
	// Paths "f0".."f3" are two bytes each: a budget of 8 holds four, 7 does not.
	for _, tc := range []struct {
		budget int
		ok     bool
	}{{8, true}, {7, false}} {
		f := &FS{byPath: map[string]*entry{}, kids: map[string][]string{}, lim: Limits{}.resolved(), maxPathBytes: tc.budget}
		var err error
		for i := 0; i < 4 && err == nil; i++ {
			err = f.add(&rardecode.FileHeader{Name: fmt.Sprintf("f%d", i)}, i)
		}
		if tc.ok && err != nil {
			t.Errorf("budget %d: %v", tc.budget, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("budget %d: %v, want ErrTooLarge", tc.budget, err)
		}
	}
}

// Before: 2000 entries, each 1000 directories deep with its own first component
// (a 4 MB archive), allocated 894,868,952 bytes and took 39 s; the path budget
// now stops it.
func TestBombDeepPaths(t *testing.T) {
	deep := strings.Repeat("a/", 1000)
	es := make([]rarEntry, 2000)
	for i := range es {
		es[i] = rarEntry{name: fmt.Sprintf("%d/%s", i, deep)}
	}
	raw := rar5(es...)
	var err error
	n := allocated(func() { _, err = openRaw(raw, Limits{MaxEntries: 1 << 24}) })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if n > 400*oneMiB {
		t.Errorf("allocated %d bytes", n)
	}
}

// Before: every insert searched its directory's child list linearly, so 50,000
// files in one directory (a 1 MB archive) took 14.3 s to index. The structure
// is now linked once, when a path first appears.
func TestChildrenAreLinkedOnceAndKeepTheirOrder(t *testing.T) {
	es := append(fileEntries(2000), fileEntries(2000)...) // every name twice
	es = append(es, rarEntry{name: "d/x"}, rarEntry{name: "d/x"}, rarEntry{name: "d/y"}, rarEntry{name: "d", flags: fileIsDir})
	fsys, err := openRaw(rar5(es...), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsys.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(root) != 2001 { // 2000 distinct files and the directory d
		t.Errorf("the root lists %d entries, want 2001", len(root))
	}
	d, err := fsys.ListDir("d")
	if err != nil || len(d) != 2 || d[0].Name() != "x" || d[1].Name() != "y" {
		t.Errorf("ListDir(d) = %v, %v", d, err)
	}
}

// FuzzOpenReader opens arbitrary bytes as a RAR archive and reads what it finds,
// under small limits so that one input cannot make a run slow.
func FuzzOpenReader(f *testing.F) {
	f.Add(rar5(rarEntry{name: "a", unpacked: 3, packed: []byte("abc")}, rarEntry{name: "d/e", flags: fileIsDir}))
	f.Add(rar5(rarEntry{name: "c", unpacked: 100, comp: compMethod3 | 3<<compDictShift, packed: []byte("0123456789")}))
	f.Add(rar5(rarEntry{name: "u", flags: flagUnknownSize, packed: []byte("xyz")}))
	f.Add([]byte("Rar!\x1a\x07\x01\x00"))
	f.Add([]byte("Rar!\x1a\x07\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fsys, err := OpenReaderLimits(bytes.NewReader(data), int64(len(data)),
			Limits{MaxEntries: 1000, MaxFileSize: oneMiB, MaxDictionary: 4 * oneMiB})
		if err != nil {
			return
		}
		var walk func(dir string, depth int)
		walk = func(dir string, depth int) {
			ents, err := fsys.ListDir(dir)
			if err != nil || depth > 32 {
				return
			}
			for _, e := range ents {
				p := strings.TrimPrefix(dir+"/"+e.Name(), "/")
				st, err := fsys.Stat(p)
				if err != nil {
					t.Fatalf("Stat(%q) of a listed entry: %v", p, err)
				}
				if st.Mode()&modeDir == modeDir {
					walk(p, depth+1)
					continue
				}
				_, _ = fsys.ReadFile(p)
			}
		}
		walk("", 0)
	})
}
