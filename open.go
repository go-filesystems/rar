// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	filesystem "github.com/go-filesystems/interface"
	"github.com/nwaples/rardecode/v2"
)

// scanner is one pass over an archive: the decoder is sequential, so every
// random access starts a new one.
type scanner interface {
	Next() (*rardecode.FileHeader, error)
	io.Reader
	Close() error
}

// noCloseScanner adapts *rardecode.Reader, which has nothing to close.
type noCloseScanner struct{ *rardecode.Reader }

func (noCloseScanner) Close() error { return nil }

// Option is a decoder option, passed through to the underlying reader.
type Option = rardecode.Option

// Password supplies the passphrase for an encrypted archive.
func Password(pass string) Option { return rardecode.Password(pass) }

// Open reads the archive at name, resolving the whole volume set it belongs to.
//
// The set is found by volume NUMBER within name's directory (see [Volumes]), so
// files carrying anything extra in their names -- an id a downloader inserted,
// a "(1)" a copy left behind -- are still reached, and nothing is renamed.
func Open(name string, opts ...Option) (*FS, error) {
	return OpenLimits(name, Limits{}, opts...)
}

// OpenLimits is Open with explicit limits; see [Limits].
func OpenLimits(name string, lim Limits, opts ...Option) (*FS, error) {
	lim = lim.resolved()
	vols, err := FindVolumes(name)
	if err != nil {
		return nil, err
	}
	first := vols.First()
	all := append([]Option{rardecode.FileSystem(vols), rardecode.MaxDictionarySize(lim.MaxDictionary)}, opts...)
	return indexLimits(func() (scanner, error) {
		return rardecode.OpenReader(first, all...)
	}, vols, lim)
}

// OpenReader reads a SINGLE-VOLUME archive from r, and is the shape every
// driver in go-filesystems answers to so github.com/go-filesystems/detect can
// open them all through one function type.
//
// A multi-volume set cannot be opened this way and nothing pretends otherwise:
// one io.ReaderAt is one file, and the rest of the set is in other files that
// only a name or a path can reach. Such an archive reports
// ErrVolumeMissing from the first entry that continues past this volume --
// which is better than the alternative this package exists to prevent, where
// the remainder is padded with zeros and called a success. Use [Open] for a
// set.
func OpenReader(r io.ReaderAt, size int64, opts ...Option) (filesystem.Filesystem, error) {
	return OpenReaderLimits(r, size, Limits{}, opts...)
}

// OpenReaderLimits is OpenReader with explicit limits; see [Limits].
func OpenReaderLimits(r io.ReaderAt, size int64, lim Limits, opts ...Option) (filesystem.Filesystem, error) {
	lim = lim.resolved()
	all := append([]Option{rardecode.MaxDictionarySize(lim.MaxDictionary)}, opts...)
	return indexLimits(func() (scanner, error) {
		rr, err := rardecode.NewReader(io.NewSectionReader(r, 0, size), all...)
		if err != nil {
			return nil, err
		}
		return noCloseScanner{rr}, nil
	}, nil, lim)
}

// index makes one pass over the archive to learn what it holds, and keeps the
// means of starting another pass for every later read.
func index(open func() (scanner, error), vols *Volumes) (*FS, error) {
	return indexLimits(open, vols, Limits{}.resolved())
}

func indexLimits(open func() (scanner, error), vols *Volumes, lim Limits) (*FS, error) {
	sc, err := open()
	if err != nil {
		return nil, err
	}
	defer sc.Close()

	fs := &FS{open: open, vols: vols, byPath: map[string]*entry{}, kids: map[string][]string{}, lim: lim, maxPathBytes: maxPathBytes}
	for ord := 0; ; ord++ {
		h, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := fs.add(h, ord); err != nil {
			return nil, err
		}
	}
	return fs, nil
}

// add records one header, and every directory above it that the archive did not
// name. An archive is a list of paths, not a tree: whether a parent gets an
// entry of its own is up to whatever wrote it, so a reader that only registers
// what it was told cannot list half the directories it holds.
//
// Every key it stores is charged against the limits first. Before they existed
// a 4 MB archive of 2000 entries, each 1000 directories deep, held 895 MB of
// path prefixes (and took 39 s to index), and 50,000 files in one directory
// took 14 s because the directory's child list was searched linearly on every
// insert.
func (f *FS) add(h *rardecode.FileHeader, ord int) error {
	p := clean(h.Name)
	if p == "." {
		return nil
	}
	size := h.UnPackedSize
	if h.UnKnownSize {
		// The field is meaningless when the archive says it does not know. It
		// used to be passed on as it was, and -1 reported as a Size of 2^64-1.
		size = 0
	} else if size < 0 {
		// A RAR 5 size is a 64-bit variable-length number that is read as signed;
		// one at or past 2^63 is not a size.
		return fmt.Errorf("%w: %q declares an unpacked size of %d", ErrCorrupt, p, size)
	}
	_, existed := f.byPath[p]
	if !existed {
		if err := f.charge(p); err != nil {
			return err
		}
	}
	f.byPath[p] = &entry{
		path:       p,
		name:       path.Base(p),
		dir:        h.IsDir,
		size:       size,
		sizeKnown:  !h.UnKnownSize,
		solid:      h.Solid,
		encrypted:  h.Encrypted,
		linkTarget: h.LinkTarget,
		modTime:    h.ModificationTime,
		ordinal:    ord,
	}
	if existed {
		// Already linked to its parents, by an earlier header or as an implied
		// directory: every key in byPath has been through the loop below.
		return nil
	}
	for child := p; ; {
		parent := path.Dir(child)
		f.kids[parent] = append(f.kids[parent], child)
		if parent == "." {
			return nil
		}
		if _, seen := f.byPath[parent]; seen {
			return nil // and so are all its ancestors
		}
		if err := f.charge(parent); err != nil {
			return err
		}
		f.byPath[parent] = &entry{path: parent, name: path.Base(parent), dir: true, sizeKnown: true, ordinal: -1}
		child = parent
	}
}

// charge accounts for one new key of byPath.
func (f *FS) charge(p string) error {
	if len(p) > maxPathLen {
		return fmt.Errorf("%w: a path of %d bytes, the limit is %d", ErrTooLarge, len(p), maxPathLen)
	}
	if len(f.byPath) >= f.lim.MaxEntries {
		return fmt.Errorf("%w: more than %d entries and directories", ErrTooLarge, f.lim.MaxEntries)
	}
	if f.pathBytes+len(p) > f.maxPathBytes {
		return fmt.Errorf("%w: paths together exceed %d bytes", ErrTooLarge, f.maxPathBytes)
	}
	f.pathBytes += len(p)
	return nil
}

// clean turns an archive's own spelling of a path into the one this driver
// answers to: '/' separators, no leading slash, no "." or ".." components.
func clean(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	return path.Clean("/" + name)[1:]
}
