package rar

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwaples/rardecode/v2"
)

// auditCountingAt counts the bytes the driver reads from the archive.
type auditCountingAt struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *auditCountingAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

// auditReadAll opens a RAR 5 archive of n STORED one-byte files and reads every
// member once, the way an extractor does. It returns the archive size, the
// bytes read from it, and the time taken.
func auditReadAll(t *testing.T, n int) (size, read int64, d time.Duration) {
	t.Helper()
	es := make([]rarEntry, n)
	for i := range es {
		es[i] = rarEntry{name: fmt.Sprintf("f%06d", i), unpacked: 1, packed: []byte("x")}
	}
	raw := rar5(es...)
	ca := &auditCountingAt{r: bytes.NewReader(raw)}
	fsys, err := OpenReaderLimits(ca, int64(len(raw)), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	afterOpen := ca.n.Load()
	start := time.Now()
	for i := 0; i < n; i++ {
		b, err := fsys.ReadFile(fmt.Sprintf("f%06d", i))
		if err != nil {
			t.Fatalf("f%06d: %v", i, err)
		}
		if string(b) != "x" {
			t.Fatalf("f%06d = %q", i, b)
		}
	}
	return int64(len(raw)), ca.n.Load() - afterOpen, time.Since(start)
}

// TestAuditQuadraticMemberScan: reading every member once must cost a bounded
// number of passes over the archive. On HEAD every handle starts a new pass at
// the first header, so n members cost n*n/2 header parses.
func TestAuditQuadraticMemberScan(t *testing.T) {
	const n = 4000
	s1, r1, d1 := auditReadAll(t, n)
	s2, r2, d2 := auditReadAll(t, 2*n)
	t.Logf("%5d members: archive %d bytes, %d bytes read from it (%d passes), %v", n, s1, r1, r1/s1, d1)
	t.Logf("%5d members: archive %d bytes, %d bytes read from it (%d passes), %v", 2*n, s2, r2, r2/s2, d2)
	t.Logf("doubling the archive multiplied the time by %.1f", float64(d2)/float64(d1))
	const max = DefaultMaxEntries
	t.Logf("extrapolated to the default MaxEntries (%d members, a ~%d MiB archive): %.0f hours",
		max, (s2/int64(2*n)*max)>>20, d2.Hours()*float64(max)*float64(max)/float64(4*n*n))
	// The work bound is on bytes read, not time. Opening a member costs one
	// decoder buffer, so the total is linear in the member count: a bounded cost
	// per member plus the archive itself. A pass from the first header per
	// member read the archive thousands of times over (784 MB for 8000 members).
	const perMember = 16 << 10
	if r2 > int64(2*n)*perMember+s2 {
		t.Fatalf("reading each of %d members once read %d bytes from a %d-byte archive (%d passes)", 2*n, r2, s2, r2/s2)
	}
	// And doubling the members must not much more than double the bytes read.
	if r2 > 5*r1/2 {
		t.Fatalf("doubling the members multiplied the bytes read by %.1f", float64(r2)/float64(r1))
	}
}

func TestLookupMemberFallsBack(t *testing.T) {
	raw := rar5(rarEntry{name: "a", unpacked: 1, packed: []byte("x")}, rarEntry{name: "b", unpacked: 1, packed: []byte("y")})
	fsys, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	f := fsys.(*FS)
	b := f.byPath["b"]
	if f.lookupMember(b) == nil {
		t.Fatal("a non-solid member was not found in the index")
	}
	solid := *b
	solid.solid = true
	if f.lookupMember(&solid) != nil {
		t.Fatal("a solid member was served from the index")
	}
	implied := *b
	implied.ordinal = -1
	if f.lookupMember(&implied) != nil {
		t.Fatal("an implied directory was served from the index")
	}
	past := *b
	past.ordinal = 99
	if f.lookupMember(&past) != nil {
		t.Fatal("an ordinal past the index was served")
	}
	wrong := *b
	wrong.ordinal = 0 // names "a", not "b"
	if f.lookupMember(&wrong) != nil {
		t.Fatal("an ordinal holding another name was served")
	}
	f.list = nil
	f.members = memberIndex{}
	if f.lookupMember(b) != nil {
		t.Fatal("no index builder, yet a member was served")
	}
}

func TestReaderFSOnlyServesItsVolume(t *testing.T) {
	r := readerFS{r: bytes.NewReader([]byte("abc")), size: 3}
	if _, err := r.Open("archive.r00"); err == nil {
		t.Fatal("a second volume was served")
	}
	f, err := r.Open(readerVolume)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if st.Size() != 3 || st.Name() != readerVolume || st.IsDir() || st.Mode() != 0o444 || !st.ModTime().IsZero() || st.Sys() != nil {
		t.Fatalf("stat = %+v", st)
	}
	if f.Close() != nil {
		t.Fatal("close failed")
	}
}

func TestIndexedMemberShortOfItsHeader(t *testing.T) {
	// The header claims 10 bytes, the data holds 1: discarding up to offset 5
	// must report the shortfall, not park a decoder at a position it never reached.
	raw := rar5(rarEntry{name: "a", unpacked: 10, packed: []byte("x")})
	fsys, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := fsys.(*FS).OpenFile("a")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	n, err := h.ReadAt(make([]byte, 1), 5)
	if err == nil {
		t.Fatalf("read %d bytes at offset 5 of a 1-byte member without an error", n)
	}
	// The discard's own error is what the sequential path reports at the same
	// point; it is not relabelled ErrShort by a later read.
	if errors.Is(err, ErrShort) {
		t.Fatalf("error = %v, want the decoder's own error, not ErrShort", err)
	}
}

func TestOpenReaderRefusesGarbageWithNilInterface(t *testing.T) {
	fsys, err := OpenReader(bytes.NewReader([]byte("not a rar archive")), 17)
	if err == nil || fsys != nil {
		t.Fatalf("OpenReader(garbage) = %v, %v; want nil, error", fsys, err)
	}
}

// failingVolumes serves the archive for the first okOpens opens, then refuses.
type failingVolumes struct {
	readerFS
	okOpens, opens int
}

func (f *failingVolumes) Open(name string) (fs.File, error) {
	f.opens++
	if f.opens > f.okOpens {
		return nil, errors.New("volume went away")
	}
	return f.readerFS.Open(name)
}

func twoMemberFS(t *testing.T) (*FS, []byte) {
	t.Helper()
	raw := rar5(rarEntry{name: "a", unpacked: 1, packed: []byte("x")}, rarEntry{name: "b", unpacked: 10, packed: []byte("y")})
	fsys, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return fsys.(*FS), raw
}

func TestMemberOpenFailureFallsBackToThePass(t *testing.T) {
	f, raw := twoMemberFS(t)
	vols := &failingVolumes{readerFS: readerFS{r: bytes.NewReader(raw), size: int64(len(raw))}, okOpens: 1}
	f.list = func() ([]*rardecode.File, error) {
		return rardecode.List(readerVolume, rardecode.FileSystem(vols))
	}
	got, err := f.ReadFile("a")
	if err != nil || string(got) != "x" {
		t.Fatalf("ReadFile(a) = %q, %v; want the sequential pass to answer", got, err)
	}
	if vols.opens < 2 {
		t.Fatalf("the indexed open was never attempted (%d opens)", vols.opens)
	}
}

func TestSequentialPassErrors(t *testing.T) {
	f, _ := twoMemberFS(t)
	f.list = nil // force the sequential path
	boom := errors.New("boom")
	real := f.open
	f.open = func() (scanner, error) { return nil, boom }
	if _, err := f.ReadFile("a"); !errors.Is(err, boom) {
		t.Fatalf("a pass that cannot start: err = %v, want boom", err)
	}
	f.open = real
	ghost := *f.byPath["a"]
	ghost.path = "ghost"
	f.byPath["ghost"] = &ghost
	if _, err := f.ReadFile("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an entry the pass never meets: err = %v, want ErrNotFound", err)
	}
	// An empty read past the end of a short member must still report the
	// shortfall of the discard, on the sequential path as on the indexed one.
	h, err := f.OpenFile("b")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.ReadAt(nil, 5); err == nil {
		t.Fatal("sequential path: empty read at offset 5 of a 1-byte member reported no error")
	}
}

func TestIndexedEmptyReadPastTheMemberReportsTheShortfall(t *testing.T) {
	f, _ := twoMemberFS(t)
	h, err := f.OpenFile("b")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.ReadAt(nil, 5); err == nil {
		t.Fatal("indexed path: empty read at offset 5 of a 1-byte member reported no error")
	}
}
