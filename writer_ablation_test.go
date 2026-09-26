// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

// Ablations.
//
// Every decision the writer makes about the container is undone here, one at a
// time, and a judge is asked whether it can tell. A decision whose ablation
// still PASSES is not a decision this suite is testing -- it is a missing
// assertion, and the two that turned out that way are named as such below rather
// than quietly dropped.
//
// The ablated archive is always the REAL writer's output with one input changed
// and the header rebuilt by the writer's own appendBlock: there is no second
// encoder here that could differ from the first and make an ablation fail for a
// reason that has nothing to do with the decision.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwaples/rardecode/v2"
)

// blk is one block, as a walker written for these tests sees it. It is a second
// reading of the format, and it checks the header CRC as it goes, so a block
// this walker accepts is one whose framing the writer got right.
type blk struct {
	off      int    // where the block's CRC32 starts
	hdrLen   int    // the whole header: CRC32, the size field, and what it counts
	htype    uint64 // 1 main, 2 file, 5 end of archive
	flags    uint64
	dataSize int64
	body     []byte // the type-specific fields
}

func walk(t *testing.T, a []byte) []blk {
	t.Helper()
	if !bytes.HasPrefix(a, signature[:]) {
		t.Fatalf("not a RAR 5 archive: %x", a[:min(8, len(a))])
	}
	var out []blk
	for i := len(signature); i < len(a); {
		if i+4 > len(a) {
			t.Fatalf("the archive ends inside a block header at %d", i)
		}
		crc := binary.LittleEndian.Uint32(a[i:])
		size, n := readVint(a[i+4:])
		if n == 0 || i+4+n+int(size) > len(a) {
			t.Fatalf("no usable header size at %d", i)
		}
		counted := a[i+4 : i+4+n+int(size)]
		if got := crc32.ChecksumIEEE(counted); got != crc {
			t.Fatalf("block at %d: header CRC %#x, the bytes say %#x", i, crc, got)
		}
		p := counted[n:]
		htype, k := readVint(p)
		p = p[k:]
		flags, k := readVint(p)
		p = p[k:]
		if flags&blockHasExtra != 0 {
			t.Fatalf("block at %d claims an extra area, which this writer does not write", i)
		}
		var dataSize uint64
		if flags&blockHasData != 0 {
			dataSize, k = readVint(p)
			p = p[k:]
		}
		b := blk{off: i, hdrLen: 4 + n + int(size), htype: htype, flags: flags, dataSize: int64(dataSize), body: p}
		out = append(out, b)
		i += b.hdrLen + int(dataSize)
	}
	return out
}

// find returns the first block of a type, or the one whose body holds name.
func find(t *testing.T, blocks []blk, htype uint64, name string) blk {
	t.Helper()
	for _, b := range blocks {
		if b.htype == htype && (name == "" || bytes.Contains(b.body, []byte(name))) {
			return b
		}
	}
	t.Fatalf("no block of type %d holding %q", htype, name)
	return blk{}
}

// replaceHeader returns a with b's header replaced by hdr, which may be a
// different length: the data that follows moves with it.
func replaceHeader(a []byte, b blk, hdr []byte) []byte {
	out := append([]byte(nil), a[:b.off]...)
	out = append(out, hdr...)
	return append(out, a[b.off+b.hdrLen:]...)
}

// fileFields locates the positional fields inside a file header body, by parsing
// them in the order the format puts them: file flags, unpacked size, attributes,
// the optional mtime and CRC32 two of those flags announce, then the compression
// word, the host OS and the name.
type fileFields struct {
	flags    uint64
	compOff  int // where the compression-information vint starts
	compLen  int
	nameOff  int
	nameLen  int
	beforeNL int // where the name-length vint starts
}

func parseFileBody(t *testing.T, body []byte) fileFields {
	t.Helper()
	var f fileFields
	p := body
	adv := func() uint64 {
		v, n := readVint(p)
		if n == 0 {
			t.Fatal("a file header body ends inside a number")
		}
		p = p[n:]
		return v
	}
	f.flags = adv()
	adv() // unpacked size
	adv() // attributes
	if f.flags&fileHasMtime != 0 {
		p = p[4:]
	}
	if f.flags&fileHasCRC32 != 0 {
		p = p[4:]
	}
	f.compOff = len(body) - len(p)
	_, f.compLen = readVint(p)
	p = p[f.compLen:]
	adv() // host OS
	f.beforeNL = len(body) - len(p)
	f.nameLen = int(adv())
	f.nameOff = len(body) - len(p)
	return f
}

// --- the judges, as functions over bytes -------------------------------------

// goReaderRefuses says why the Go reader would not read every entry whole, or
// nil when it read them all.
func goReaderRefuses(a []byte) error {
	rc, err := rardecode.NewReader(bytes.NewReader(a))
	if err != nil {
		return err
	}
	for {
		_, err := rc.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := io.ReadAll(rc); err != nil {
			return err
		}
	}
}

// sevenZipRefuses runs `7zz t` and says why it refused, or nil when it passed.
func sevenZipRefuses(t *testing.T, a []byte) error {
	t.Helper()
	bin := sevenZip(t)
	p := filepath.Join(t.TempDir(), "ablated.rar")
	if err := os.WriteFile(p, a, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "t", p).CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "Everything is Ok") {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

// bothJudgesRefuse is the assertion most ablations make.
func bothJudgesRefuse(t *testing.T, a []byte, what string) {
	t.Helper()
	if err := goReaderRefuses(a); err == nil {
		t.Errorf("ABLATION PASSED: the Go reader accepted an archive with %s", what)
	} else {
		t.Logf("Go reader refused: %v", err)
	}
	if err := sevenZipRefuses(t, a); err == nil {
		t.Errorf("ABLATION PASSED: 7-Zip accepted an archive with %s", what)
	} else {
		t.Logf("7-Zip refused: %v", err)
	}
}

// oneFile is the smallest archive an ablation can be applied to, plus a
// directory and a non-ASCII name for the ablations that need them.
func ablationCorpus() []item {
	return []item{
		{name: "dir", dir: true, perm: 0o755},
		{name: "réf/ünïcødé.txt", perm: 0o644, body: []byte("a name that is not ASCII\n")},
		// Long enough that its header size needs a multi-byte vint, which is what
		// the vint ablation needs something to bite on.
		{name: "deep/" + strings.Repeat("a-rather-long-path-component/", 5) + "name.txt", perm: 0o640, body: []byte("long\n")},
	}
}

func ablationArchive(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(build(t, ablationCorpus()))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAblationTheVintEncoding.
//
// The vint puts the LEAST significant seven bits first. Writing the groups the
// other way round keeps the length and every continuation bit -- so the archive
// is still well formed, and only the VALUE is wrong. A reader then takes a
// header size of 152 for one of 12545 and looks for the next block past the end.
func TestAblationTheVintEncoding(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "a-rather-long-path-component")
	// Rebuild the header with the size vint's groups reversed. The counted bytes
	// are untouched: only the encoding of the number in front of them changes.
	counted := a[b.off+4 : b.off+b.hdrLen]
	_, n := readVint(counted)
	value, _ := readVint(counted)
	if n < 2 {
		t.Fatalf("this header's size vint is %d byte(s); the ablation needs a multi-byte one", n)
	}
	reversed := reverseGroups(appendVint(nil, value))
	hdr := append([]byte(nil), reversed...)
	hdr = append(hdr, counted[n:]...)
	full := binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(hdr))
	full = append(full, hdr...)
	bothJudgesRefuse(t, replaceHeader(a, b, full), "its header size written most-significant group first")
}

func lenVint(n uint64) int { return len(appendVint(nil, n)) }

// reverseGroups re-encodes a vint with its seven-bit groups in the opposite
// order, keeping the continuation bits where the encoding needs them.
func reverseGroups(v []byte) []byte {
	groups := make([]byte, len(v))
	for i, c := range v {
		groups[len(v)-1-i] = c & 0x7f
	}
	out := make([]byte, len(groups))
	for i, g := range groups {
		if i < len(groups)-1 {
			g |= 0x80
		}
		out[i] = g
	}
	return out
}

// TestAblationWhichFieldsTheHeaderCRCSpans.
//
// The CRC covers the header from the byte after itself to the header's end,
// which INCLUDES the header-size field -- a field the reader has already
// consumed by the time it checks the sum, and therefore the one a writer leaves
// out. Both directions are ablated: a sum over the counted bytes alone, and a
// sum that also swallows the packed data.
func TestAblationWhichFieldsTheHeaderCRCSpans(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "ünïcødé")
	counted := a[b.off+4 : b.off+b.hdrLen]
	_, n := readVint(counted)

	t.Run("the size field left out", func(t *testing.T) {
		broken := append([]byte(nil), a...)
		binary.LittleEndian.PutUint32(broken[b.off:], crc32.ChecksumIEEE(counted[n:]))
		bothJudgesRefuse(t, broken, "a header CRC that skips the header-size field")
	})
	t.Run("the packed data swallowed", func(t *testing.T) {
		broken := append([]byte(nil), a...)
		over := a[b.off+4 : b.off+b.hdrLen+int(b.dataSize)]
		binary.LittleEndian.PutUint32(broken[b.off:], crc32.ChecksumIEEE(over))
		bothJudgesRefuse(t, broken, "a header CRC that also covers the packed data")
	})
}

// TestAblationTheMethodBits.
//
// The method is three bits at offset SEVEN of the compression word, and 0 is
// stored. Claiming method 1 promises a compressed entry whose bytes are plain,
// and a reader answers that by decoding nothing that makes sense.
func TestAblationTheMethodBits(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "ünïcødé")
	f := parseFileBody(t, b.body)

	body := append([]byte(nil), b.body[:f.compOff]...)
	body = appendVint(body, 1<<7) // method 1, at the right offset
	body = append(body, b.body[f.compOff+f.compLen:]...)
	hdr := appendBlock(nil, b.htype, b.flags, b.dataSize, body)
	bothJudgesRefuse(t, replaceHeader(a, b, hdr), "the stored method replaced by method 1")
}

// TestAblationTheMethodNumberAtTheWrongOffset.
//
// ⚠ THIS ABLATION PASSES, and it is reported rather than removed.
//
// Writing the method number 1 into the low bits -- where the compression
// ALGORITHM VERSION lives -- instead of at offset seven leaves the method field
// reading 0, so both judges take the entry for a stored one and return its bytes
// intact. Neither reader validates the version of an entry it is not going to
// decompress.
//
// So no judge available here can hold the writer to the version field, and the
// only thing keeping it right is that it is written from the note and stated in
// a comment. The test asserts what IS true -- that the bytes still come back --
// so that the day a reader does start checking, this fails and says why.
func TestAblationTheMethodNumberAtTheWrongOffset(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "ünïcødé")
	f := parseFileBody(t, b.body)

	body := append([]byte(nil), b.body[:f.compOff]...)
	body = appendVint(body, 1) // method number in the version field
	body = append(body, b.body[f.compOff+f.compLen:]...)
	ablated := replaceHeader(a, b, appendBlock(nil, b.htype, b.flags, b.dataSize, body))

	if err := goReaderRefuses(ablated); err != nil {
		t.Fatalf("a judge now checks the version field, and this ablation should become a refusal: %v", err)
	}
	if err := sevenZipRefuses(t, ablated); err != nil {
		t.Fatalf("7-Zip now checks the version field, and this ablation should become a refusal: %v", err)
	}
	t.Log("ABLATION PASSES, as documented: no judge here validates the compression version of a stored entry")
}

// TestAblationTheDataSizeFlag.
//
// The flag is what puts the byte count in the common header. Without it a reader
// looks for the next block immediately after this one -- and finds the entry's
// own contents, which it parses as a header.
func TestAblationTheDataSizeFlag(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "ünïcødé")
	hdr := appendBlock(nil, b.htype, b.flags&^blockHasData, 0, b.body)
	bothJudgesRefuse(t, replaceHeader(a, b, hdr), "the data-size flag cleared on an entry that has data")
}

// TestAblationTheDirectoryFlag.
//
// A directory is a file entry with this bit set and no data. Clearing it produces
// a VALID archive -- no judge refuses it -- in which the directory has become an
// empty file. So the assertion is not a refusal but the consequence, which is
// what TestTheGoReaderReadsBackEveryEntryByteForByte asserts for every entry.
func TestAblationTheDirectoryFlag(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "dir")
	f := parseFileBody(t, b.body)
	if f.flags&fileIsDir == 0 {
		t.Fatal("the entry picked for this ablation is not a directory")
	}
	body := appendVint(nil, f.flags&^fileIsDir)
	body = append(body, b.body[lenVint(f.flags):]...)
	ablated := replaceHeader(a, b, appendBlock(nil, b.htype, b.flags, b.dataSize, body))

	rc, err := rardecode.NewReader(bytes.NewReader(ablated))
	if err != nil {
		t.Fatal(err)
	}
	h, err := rc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if h.IsDir {
		t.Error("ABLATION PASSED: the entry is still a directory with the flag cleared")
	}
}

// TestAblationTheEndOfArchiveBlock.
//
// ⚠ Written expecting this ablation to PASS -- a reader that runs out of file
// where a header should start might reasonably call that the end -- and it does
// not. Both judges refuse: the last file block promises that something follows
// it, and EOF is not a block, so rardecode reports an unexpected EOF and 7-Zip an
// unexpected end of archive. The block is load-bearing, and the assertion says so
// rather than recording the guess.
func TestAblationTheEndOfArchiveBlock(t *testing.T) {
	a := ablationArchive(t)
	blocks := walk(t, a)
	end := blocks[len(blocks)-1]
	if end.htype != blockEnd {
		t.Fatalf("the last block is type %d, not the end of archive", end.htype)
	}
	bothJudgesRefuse(t, a[:end.off], "no end-of-archive block")
}

// TestAblationTheNameEncoding.
//
// Names go in as UTF-8. Writing the same string as Latin-1 keeps it a legal
// header -- the length field is adjusted with it -- so no judge refuses the
// archive; what changes is the NAME that comes back, which is why the read-back
// test asserts names rather than counting entries.
func TestAblationTheNameEncoding(t *testing.T) {
	a := ablationArchive(t)
	b := find(t, walk(t, a), blockFile, "ünïcødé")
	f := parseFileBody(t, b.body)
	utf8Name := string(b.body[f.nameOff : f.nameOff+f.nameLen])

	var latin1 []byte
	for _, r := range utf8Name {
		latin1 = append(latin1, byte(r))
	}
	if len(latin1) == len(utf8Name) {
		t.Fatal("the name picked for this ablation is ASCII, so the two encodings agree")
	}
	body := append([]byte(nil), b.body[:f.beforeNL]...)
	body = appendVint(body, uint64(len(latin1)))
	body = append(body, latin1...)
	ablated := replaceHeader(a, b, appendBlock(nil, b.htype, b.flags, b.dataSize, body))

	rc, err := rardecode.NewReader(bytes.NewReader(ablated))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for {
		h, err := rc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Logf("the Go reader refused a Latin-1 name outright: %v", err)
			return
		}
		names = append(names, h.Name)
		_, _ = io.ReadAll(rc)
	}
	for _, n := range names {
		if n == utf8Name {
			t.Errorf("ABLATION PASSED: the name came back as %q although it was written as Latin-1", n)
		}
	}
}

// TestAblationTheDataChecksum.
//
// ⛔ This is the ablation that decided the writer's shape, and it is the reason
// NewWriter asks for an io.WriteSeeker.
//
// With the checksum flag cleared, an entry whose bytes have been altered is
// accepted by BOTH judges: there is nothing in a stored entry for a reader to
// check, so `7zz t` answers "Everything is Ok" about data it has just been given
// wrong. The same corruption against the writer's real output is refused by both
// -- TestSevenZipRejectsAnArchiveWeBroke and its Go counterpart. An archive
// without this field is not a slightly weaker archive; it is an unverifiable one.
func TestAblationTheDataChecksum(t *testing.T) {
	const body = "the bytes a checksum would have covered\n"
	raw, err := os.ReadFile(build(t, []item{{name: "f.txt", perm: 0o644, body: []byte(body)}}))
	if err != nil {
		t.Fatal(err)
	}
	b := find(t, walk(t, raw), blockFile, "f.txt")
	f := parseFileBody(t, b.body)
	if f.flags&fileHasCRC32 == 0 {
		t.Fatal("the writer did not set the checksum flag, so there is nothing to ablate")
	}
	// Rebuild the body without the flag and without the four bytes it announced.
	nb := appendVint(nil, f.flags&^fileHasCRC32)
	tail := b.body[lenVint(f.flags):]
	var p []byte
	p = append(p, tail...)
	// Drop the CRC32 field: it sits just before the compression word.
	cut := f.compOff - 4 - lenVint(f.flags)
	nb = append(nb, p[:cut]...)
	nb = append(nb, p[cut+4:]...)
	ablated := replaceHeader(raw, b, appendBlock(nil, b.htype, b.flags, b.dataSize, nb))

	// It must still be a valid archive, or this proves nothing.
	if err := goReaderRefuses(ablated); err != nil {
		t.Fatalf("the ablated archive is not merely unverifiable, it is broken: %v", err)
	}
	i := bytes.Index(ablated, []byte(body))
	if i < 0 {
		t.Fatal("the entry's bytes are not in the ablated archive")
	}
	ablated[i] ^= 0xff

	goErr := goReaderRefuses(ablated)
	szErr := sevenZipRefuses(t, ablated)
	if goErr != nil || szErr != nil {
		t.Fatalf("a judge caught corruption with no checksum in the header, so the CRC32 is not what makes it detectable: go=%v 7zz=%v", goErr, szErr)
	}
	t.Log("ABLATION PASSES, as documented: with no data CRC32 both judges accept corrupted bytes, which is why the writer backpatches one")
}
