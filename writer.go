// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"strings"
)

// signature is what a RAR 5 archive begins with.
//
// RAR 4's signature is the first SEVEN of these bytes followed by 0x00, so the
// two formats differ in the eighth byte alone. A reader that compares seven
// takes one for the other and then parses a completely different header layout,
// which is why this is spelled out in full rather than shared with a prefix.
var signature = [8]byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x01, 0x00}

// Block types. Only these three are written; a reader skips what it does not
// know by the header size, which is why an archive needs no others.
const (
	blockMain = 1 // main archive header, once, before every file
	blockFile = 2 // one file or directory
	blockEnd  = 5 // end of archive
)

// Common block header flags.
const (
	// blockHasExtra says an extra AREA follows the type-specific fields, and its
	// size field comes BEFORE the data size. This writer sets no extra records,
	// so it never sets this bit -- and that is the only reason the field order
	// below can leave the extra size out.
	blockHasExtra = 0x0001
	// blockHasData says the block is followed by DataSize bytes of packed data,
	// and puts that count in the common header. Without it a reader looks for
	// the next header immediately after this one, so a file block that carries
	// bytes and forgets this bit has its own contents parsed as a header.
	blockHasData = 0x0002
)

// File block flags.
const (
	fileIsDir    = 0x0001 // a directory: no data, and no data size
	fileHasMtime = 0x0002 // a uint32 Unix mtime follows the attributes
	fileHasCRC32 = 0x0004 // a uint32 CRC32 of the UNPACKED data follows
)

// hostOSUnix is the host OS this writer records, and it decides how the
// attributes field is read: a POSIX mode here, Windows attribute bits there.
const hostOSUnix = 1

// compressionInfoStored is the compression-information word for a stored entry.
//
// The word packs several fields: bits 0-5 the version of the compression
// algorithm (0 is RAR 5.0), bit 6 the solid flag, bits 7-9 the METHOD, bits
// 10-14 the dictionary size. Method 0 means stored, and with nothing compressed
// there is no dictionary and no solid predecessor, so every field is zero and so
// is the word.
//
// The method being three bits at offset SEVEN is the part worth stating: a
// method number written at the wrong offset is a method the reader does not
// have, and unrar reports that as an unsupported compression algorithm -- an
// error about the archive, naming nothing that would lead anyone back to here.
const compressionInfoStored = 0

// maxNameLen caps the stored name. The format gives a header 0x200000 bytes and
// a reader is entitled to refuse anything longer; a name past this would
// therefore produce an archive that nothing reads, which is worse than a
// refusal at the call that named it.
const maxNameLen = 0x10000

// Writer builds a RAR 5 archive holding stored entries.
//
// It needs an io.WriteSeeker, and that is the format's doing rather than a
// convenience. A file header carries a CRC32 of the entry's UNPACKED data, and
// the header is written before that data is read; the alternatives are to hold
// every entry in memory to checksum it first, or to leave the checksum out. The
// second is not the small compromise it looks like: with no checksum in the
// header there is nothing in a stored entry for a reader to verify, and `7zz t`
// answers "Everything is Ok" for an archive whose bytes have been altered. So
// the header goes out with the field zeroed, the data streams past a running
// CRC, and the header is written again in place -- it is the same length either
// way, because only a fixed four-byte field changed.
//
// Entries carry no timestamp. The Builder contract this shape answers to does
// not offer one, and a time this package invented would be a fact about when it
// ran rather than about the file; leaving the field out also makes the same
// input produce the same bytes.
//
// # Stored only
//
// See the package documentation: RAR's compression is proprietary and
// unpublished, so the stored method is not a first step towards the others. It
// is the whole of what can be written from a published specification.
type Writer struct {
	w       io.WriteSeeker
	started bool
	closed  bool
}

// NewWriter returns a Writer that builds an archive in w.
//
// Nothing is written until the first entry or Close: an archive with no entries
// is still a valid archive, and Close writes one.
func NewWriter(w io.WriteSeeker) *Writer { return &Writer{w: w} }

// appendVint appends n in the format's variable-length integer encoding: seven
// bits of the value per byte, the LEAST significant group first, and the high
// bit of a byte set when another byte follows.
func appendVint(b []byte, n uint64) []byte {
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return append(b, byte(n))
}

// readVint reads one vint from the front of b, returning it and the number of
// bytes it took. A count of zero means b ended inside the number, or the number
// needed more than the 64 bits it is read into -- the caller cannot tell those
// apart and does not need to: neither is a number.
func readVint(b []byte) (uint64, int) {
	var n uint64
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, 0 // the tenth byte would carry bits past the 64th
		}
		if i > 9 {
			return 0, 0
		}
		n |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return n, i + 1
		}
	}
	return 0, 0
}

// appendBlock appends one whole block header: its CRC32, its size, and the
// header itself.
//
// Two things about the CRC are easy to get wrong and both are load-bearing. It
// covers the header from the byte AFTER itself to the header's end -- which
// INCLUDES the header-size field, a field a reader has already consumed before
// it checks the sum. And it covers the header only: the packed data that follows
// a file block is not part of it, and has its own CRC in the file header.
//
// HeaderSize counts from after its own last byte to the end of the header, so
// the value depends on nothing that follows the block.
func appendBlock(out []byte, htype, flags uint64, dataSize int64, body []byte) []byte {
	var h []byte
	h = appendVint(h, htype)
	h = appendVint(h, flags)
	if flags&blockHasData != 0 {
		h = appendVint(h, uint64(dataSize))
	}
	h = append(h, body...)

	var sized []byte
	sized = appendVint(sized, uint64(len(h)))
	sized = append(sized, h...)

	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(sized))
	return append(out, sized...)
}

// attributes is what the attributes field holds for a Unix host: the POSIX
// mode, type bits and all.
//
// os.FileMode is NOT that number. Its type bits sit at the TOP -- os.ModeDir is
// 1<<31 -- so handing one over unconverted records a directory as a mode no
// POSIX reader recognises. The same reason fs.go keeps these constants.
func attributes(dir bool, perm os.FileMode) uint64 {
	if dir {
		return uint64(modeDir | perm.Perm())
	}
	return uint64(modeRegular | perm.Perm())
}

// fileBody builds the type-specific half of a file block header.
//
// The order is the format's and every field is positional: file flags, unpacked
// size, attributes, then the optional mtime and CRC32 that two of those flags
// announce, then the compression word, the host OS, and the name with its
// length. A reader that meets these in another order does not fail on the field
// that moved, it fails further along on a length that is now a name.
func fileBody(name string, flags, unpackedSize, attrs uint64, sum uint32) []byte {
	var b []byte
	b = appendVint(b, flags)
	b = appendVint(b, unpackedSize)
	b = appendVint(b, attrs)
	if flags&fileHasMtime != 0 {
		// Never set by this writer; the field is here so the layout above is the
		// format's and not an abridgement of it.
		b = binary.LittleEndian.AppendUint32(b, 0)
	}
	if flags&fileHasCRC32 != 0 {
		b = binary.LittleEndian.AppendUint32(b, sum)
	}
	b = appendVint(b, compressionInfoStored)
	b = appendVint(b, hostOSUnix)
	b = appendVint(b, uint64(len(name)))
	return append(b, name...)
}

// start writes the signature and the main archive header, once.
func (z *Writer) start() error {
	if z.closed {
		return ErrWriterClosed
	}
	if z.started {
		return nil
	}
	z.started = true
	if _, err := z.w.Write(signature[:]); err != nil {
		return fmt.Errorf("rar: writing the signature: %w", err)
	}
	// Archive flags 0: a single volume, not solid, carrying no volume number.
	return z.writeBlock(blockMain, 0, 0, appendVint(nil, 0))
}

func (z *Writer) writeBlock(htype, flags uint64, dataSize int64, body []byte) error {
	_, err := z.w.Write(appendBlock(nil, htype, flags, dataSize, body))
	return err
}

// AddDir records a directory, which in this format is a file entry with the
// directory flag, no data, and no data size.
func (z *Writer) AddDir(name string, perm os.FileMode) error {
	if err := z.start(); err != nil {
		return err
	}
	if err := checkName(name); err != nil {
		return err
	}
	body := fileBody(name, fileIsDir, 0, attributes(true, perm), 0)
	return z.writeBlock(blockFile, 0, 0, body)
}

// AddFile records a file of exactly size bytes and copies them from r.
//
// The size is the caller's: it goes in the header, before the bytes, and cannot
// be discovered without reading the whole entry first. A reader that delivers
// some other number of bytes is refused here, naming the entry and the size it
// was promised, because the alternative is an archive whose header disagrees
// with its data -- which every reader reports as corruption of the file, with
// nothing to say who wrote it.
func (z *Writer) AddFile(name string, perm os.FileMode, size int64, r io.Reader) error {
	if err := z.start(); err != nil {
		return err
	}
	if err := checkName(name); err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("rar: %s: negative size %d", name, size)
	}

	at, err := z.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("rar: %s: finding the header position: %w", name, err)
	}
	flags := uint64(fileHasCRC32)
	attrs := attributes(false, perm)
	// The checksum is not known yet, so the field goes out zeroed and is written
	// again below. Only its four bytes change, which is why the header can be
	// replaced where it lies.
	header := appendBlock(nil, blockFile, blockHasData, size, fileBody(name, flags, uint64(size), attrs, 0))
	if _, err := z.w.Write(header); err != nil {
		return fmt.Errorf("rar: %s: writing the header: %w", name, err)
	}

	sum := crc32.NewIEEE()
	n, err := io.Copy(z.w, io.TeeReader(io.LimitReader(r, size), sum))
	if err != nil {
		return fmt.Errorf("rar: %s: copying the entry: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("rar: %s: declared %d bytes and delivered %d: %w", name, size, n, ErrSizeMismatch)
	}
	// A reader with MORE than it promised is the same defect seen from the other
	// side, and it is the quiet one: the tail is simply dropped, and the archive
	// that results is valid.
	var one [1]byte
	if extra, _ := r.Read(one[:]); extra > 0 {
		return fmt.Errorf("rar: %s: declared %d bytes and delivered more: %w", name, size, ErrSizeMismatch)
	}

	end, err := z.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("rar: %s: finding the end of the entry: %w", name, err)
	}
	patched := appendBlock(nil, blockFile, blockHasData, size, fileBody(name, flags, uint64(size), attrs, sum.Sum32()))
	if len(patched) != len(header) {
		// Unreachable while the only difference is a fixed-width field, and
		// checked anyway: a shorter header written over a longer one leaves the
		// difference behind as the first bytes of the entry.
		return fmt.Errorf("rar: %s: the checksummed header is %d bytes, not %d", name, len(patched), len(header))
	}
	if _, err := z.w.Seek(at, io.SeekStart); err != nil {
		return fmt.Errorf("rar: %s: seeking back to the header: %w", name, err)
	}
	if _, err := z.w.Write(patched); err != nil {
		return fmt.Errorf("rar: %s: rewriting the header: %w", name, err)
	}
	if _, err := z.w.Seek(end, io.SeekStart); err != nil {
		return fmt.Errorf("rar: %s: returning to the end of the entry: %w", name, err)
	}
	return nil
}

// Close writes the end-of-archive block.
//
// An archive without it is one a reader meets the end of the file in, and
// readers differ on whether that is an error or an archive that stops. Writing
// it is what says the set ends here rather than continuing in another volume.
func (z *Writer) Close() error {
	if z.closed {
		return ErrWriterClosed
	}
	if err := z.start(); err != nil {
		return err
	}
	z.closed = true
	// End flags 0: this is the last volume of the set.
	return z.writeBlock(blockEnd, 0, 0, appendVint(nil, 0))
}

// checkName refuses a name the archive cannot carry unchanged.
//
// The names go in as UTF-8 with '/' separators, and the rule is that what the
// caller wrote is what a reader gets back. So a backslash is REFUSED rather than
// translated: it is an ordinary character in a Unix filename and a separator to
// a RAR reader -- including this package's own, which folds it before it looks
// anything up -- and silently turning one file into two directories is worse
// than declining to store it.
//
// A leading slash or a ".." component is refused for the reason every archive
// format eventually learns: the name is a path the extractor will join, and one
// that climbs out of the destination is not a filename.
func checkName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: an entry with no name", ErrBadName)
	case len(name) > maxNameLen:
		return fmt.Errorf("%w: %d bytes, more than the %d a header can carry", ErrBadName, len(name), maxNameLen)
	case strings.Contains(name, `\`):
		return fmt.Errorf("%w: %q holds a backslash, which a RAR reader takes for a separator", ErrBadName, name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: %q holds a NUL", ErrBadName, name)
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrBadName, name)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q names no entry", ErrBadName, name)
	case strings.HasPrefix(name, "../"):
		return fmt.Errorf("%w: %q climbs out of the archive", ErrBadName, name)
	case name != path.Clean(name):
		return fmt.Errorf("%w: %q is not in its cleaned form %q", ErrBadName, name, path.Clean(name))
	}
	return nil
}
