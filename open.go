// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"errors"
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
	vols, err := FindVolumes(name)
	if err != nil {
		return nil, err
	}
	first := vols.First()
	all := append([]Option{rardecode.FileSystem(vols)}, opts...)
	return index(func() (scanner, error) {
		return rardecode.OpenReader(first, all...)
	}, vols)
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
	return index(func() (scanner, error) {
		rr, err := rardecode.NewReader(io.NewSectionReader(r, 0, size), opts...)
		if err != nil {
			return nil, err
		}
		return noCloseScanner{rr}, nil
	}, nil)
}

// index makes one pass over the archive to learn what it holds, and keeps the
// means of starting another pass for every later read.
func index(open func() (scanner, error), vols *Volumes) (*FS, error) {
	sc, err := open()
	if err != nil {
		return nil, err
	}
	defer sc.Close()

	fs := &FS{open: open, vols: vols, byPath: map[string]*entry{}, kids: map[string][]string{}}
	for ord := 0; ; ord++ {
		h, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		fs.add(h, ord)
	}
	return fs, nil
}

// add records one header, and every directory above it that the archive did not
// name. An archive is a list of paths, not a tree: whether a parent gets an
// entry of its own is up to whatever wrote it, so a reader that only registers
// what it was told cannot list half the directories it holds.
func (f *FS) add(h *rardecode.FileHeader, ord int) {
	p := clean(h.Name)
	if p == "." {
		return
	}
	f.byPath[p] = &entry{
		path:       p,
		name:       path.Base(p),
		dir:        h.IsDir,
		size:       h.UnPackedSize,
		sizeKnown:  !h.UnKnownSize,
		solid:      h.Solid,
		encrypted:  h.Encrypted,
		linkTarget: h.LinkTarget,
		modTime:    h.ModificationTime,
		ordinal:    ord,
	}
	for parent := path.Dir(p); ; parent = path.Dir(parent) {
		if _, seen := f.byPath[parent]; !seen && parent != "." {
			f.byPath[parent] = &entry{path: parent, name: path.Base(parent), dir: true, sizeKnown: true, ordinal: -1}
		}
		f.kids[parent] = appendOnce(f.kids[parent], p)
		if parent == "." {
			break
		}
		p = parent
	}
}

func appendOnce(list []string, s string) []string {
	for _, e := range list {
		if e == s {
			return list
		}
	}
	return append(list, s)
}

// clean turns an archive's own spelling of a path into the one this driver
// answers to: '/' separators, no leading slash, no "." or ".." components.
func clean(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	return path.Clean("/" + name)[1:]
}
