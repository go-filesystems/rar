// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSplitVolumeReadsTheNumberThroughWhateverWasInserted is the case this
// package was written for. A downloader that files an archive under
//
//	[MOVIE] … 1080p.part2 [49736858].rar
//
// has not corrupted anything -- but the RAR format finds its next volume BY
// NAME, so part1 asks for "….part2.rar", the inserted id makes that file
// nothing the chain can see, and a reader that pads the remainder with zeros
// calls the result a success.
func TestSplitVolumeReadsTheNumberThroughWhateverWasInserted(t *testing.T) {
	for _, c := range []struct {
		name string
		stem string
		num  int
		ok   bool
	}{
		{"film.part1.rar", "film", 1, true},
		{"film.part2.rar", "film", 2, true},
		// What a download put there, and what a duplicate-file tidy-up does.
		{"[MOVIE] Ange Elle 1080p.part1 [49736866].rar", "[MOVIE] Ange Elle 1080p", 1, true},
		{"[MOVIE] Ange Elle 1080p.part2 [49736858].rar", "[MOVIE] Ange Elle 1080p", 2, true},
		{"film.part3 (1).rar", "film", 3, true},
		{"film.part10.rar", "film", 10, true}, // two digits, not "1" then junk
		// The old scheme: the first volume is .rar and the rest count from .r00.
		{"film.rar", "film", 1, true},
		{"film.r00", "film", 2, true},
		{"film.r01", "film", 3, true},
		// Case does not decide it.
		{"FILM.PART2.RAR", "FILM", 2, true},
		// Not a volume at all.
		{"film.mp4", "", 0, false},
		{"film.part0.rar", "", 0, false},
		{"notes.txt", "", 0, false},
	} {
		stem, num, ok := splitVolume(c.name)
		if ok != c.ok || (ok && (stem != c.stem || num != c.num)) {
			t.Errorf("splitVolume(%q) = (%q, %d, %v), want (%q, %d, %v)",
				c.name, stem, num, ok, c.stem, c.num, c.ok)
		}
	}
}

// TestFindVolumesKeepsThreeSetsApart is an ordinary downloaded album: one
// directory, three different multi-volume films. Grouping by number alone
// would hand part2 of one film to part1 of another, and the decoder would
// accept it -- the volumes of a set are not self-identifying enough to notice.
func TestFindVolumesKeepsThreeSetsApart(t *testing.T) {
	dir := t.TempDir()
	first := write(t, dir, "Amatrices 1080p.part1 [49736866].rar")
	write(t, dir, "Amatrices 1080p.part2 [49736858].rar")
	// Named to sort BEFORE the set under test. Without it the directory order
	// happens to put the right part2 first, "first writer wins" keeps it, and
	// this test passes with the stem check removed -- the fixture agreeing
	// with the sort key instead of proving anything.
	write(t, dir, "A different film.part2 [11111111].rar")
	write(t, dir, "A different film.part1 [11111112].rar")
	write(t, dir, "Gros atouts 720p.part1 [49736884].rar")
	write(t, dir, "Gros atouts 720p.part2 [49736877].rar")
	write(t, dir, "Leche mon Anus 1080p.part1 [49736900].rar")
	write(t, dir, "Leche mon Anus 1080p.part2 [49736896].rar")
	write(t, dir, "a video that is not an archive.mp4")

	v, err := FindVolumes(first)
	if err != nil {
		t.Fatal(err)
	}
	if v.Count() != 2 {
		t.Errorf("found %d volumes, want 2: the other two sets were swept in", v.Count())
	}
	f, err := v.Open("whatever.part2.rar")
	if err != nil {
		t.Fatalf("volume 2: %v", err)
	}
	defer f.Close()
	b := make([]byte, 64)
	n, _ := f.Read(b)
	if got := string(b[:n]); got != "Amatrices 1080p.part2 [49736858].rar" {
		t.Errorf("volume 2 resolved to %q, want this set's own part2", got)
	}
}

// TestOpenAnswersByNumberNotByName is the indirection itself: the decoder asks
// for the canonical name the archive was built with, and gets the file that
// actually holds that volume, whatever it is called. Nothing is renamed and
// nothing is linked.
func TestOpenAnswersByNumberNotByName(t *testing.T) {
	dir := t.TempDir()
	first := write(t, dir, "film.part1 [aaa].rar")
	write(t, dir, "film.part2 [bbb].rar")

	v, err := FindVolumes(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ asked, want string }{
		{"volume.part1.rar", "film.part1 [aaa].rar"},
		{"volume.part2.rar", "film.part2 [bbb].rar"},
		// The old-scheme derivation resolves to the same volumes, because the
		// number decides and the spelling does not.
		{"volume.r00", "film.part2 [bbb].rar"},
	} {
		f, err := v.Open(c.asked)
		if err != nil {
			t.Errorf("Open(%q): %v", c.asked, err)
			continue
		}
		b := make([]byte, 64)
		n, _ := f.Read(b)
		f.Close()
		if got := string(b[:n]); got != c.want {
			t.Errorf("Open(%q) gave %q, want %q", c.asked, got, c.want)
		}
	}

	// A hole in the set is named as such: "the third volume is missing" and
	// "that path is wrong" send a person looking in different places.
	if _, err := v.Open("volume.part3.rar"); !errors.Is(err, ErrVolumeMissing) {
		t.Errorf("a missing volume gave %v, want ErrVolumeMissing", err)
	}
}

// TestFindVolumesOnASingleFileArchive: a set of one is still a set.
func TestFindVolumesOnASingleFileArchive(t *testing.T) {
	dir := t.TempDir()
	first := write(t, dir, "notes.rar")
	v, err := FindVolumes(first)
	if err != nil {
		t.Fatal(err)
	}
	if v.Count() != 1 {
		t.Errorf("%d volumes for a lone archive, want 1", v.Count())
	}
	if got := v.Numbers(); len(got) != 1 || got[0] != 1 {
		t.Errorf("Numbers() = %v, want [1]", got)
	}
}
