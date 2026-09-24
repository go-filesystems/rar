// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// partRE matches the modern volume suffix: ".partN" followed by anything that
// is not itself a volume marker, then ".rar". The middle group is what a
// downloader, a de-duplicator or a person has added -- " [49736858]",
// " (1)", " copy" -- and it is the reason this file exists.
var partRE = regexp.MustCompile(`^(?i)(.*)\.part(\d+)(.*)\.rar$`)

// oldRE matches the pre-RAR3 scheme, where the first volume is ".rar" and the
// rest count up from ".r00".
var oldRE = regexp.MustCompile(`^(?i)(.*)\.r(?:ar|(\d{2,}))(.*)$`)

// Volumes is the set of files one multi-volume archive is split across, keyed
// by volume number.
//
// It answers [fs.FS] so it can be handed straight to the decoder, which asks
// for volumes BY NAME -- and the names it asks for are the canonical ones the
// archive was made with, not the ones the files ended up carrying. Every
// request is answered by the NUMBER in the requested name, so the real files
// keep whatever they are called.
type Volumes struct {
	dir   string
	byNum map[int]string // volume number -> file name within dir
	asked []string       // names requested, in order, for tests and diagnosis
}

// FindVolumes collects the volume set that first belongs to, by looking in its
// directory for files with the same stem and a volume number.
//
// Only the stem decides membership, so a directory holding three different
// multi-volume sets -- which is an ordinary thing for a downloaded album --
// resolves each of them separately instead of mixing them.
func FindVolumes(first string) (*Volumes, error) {
	dir := filepath.Dir(first)
	stem, num, ok := splitVolume(filepath.Base(first))
	if !ok {
		// Not a volume name at all: a single-file archive is a set of one.
		return &Volumes{dir: dir, byNum: map[int]string{1: filepath.Base(first)}}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	v := &Volumes{dir: dir, byNum: map[int]string{num: filepath.Base(first)}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		s, n, ok := splitVolume(e.Name())
		if !ok || !strings.EqualFold(s, stem) {
			continue
		}
		// First writer wins, so the file the caller named keeps its number
		// even if another file claims it.
		if _, taken := v.byNum[n]; !taken {
			v.byNum[n] = e.Name()
		}
	}
	return v, nil
}

// splitVolume reads a file name as <stem>, <volume number>, ignoring whatever
// sits between the number and the extension.
func splitVolume(name string) (stem string, num int, ok bool) {
	if m := partRE.FindStringSubmatch(name); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil || n < 1 {
			return "", 0, false
		}
		return m[1], n, true
	}
	if m := oldRE.FindStringSubmatch(name); m != nil {
		if m[2] == "" {
			return m[1], 1, true // ".rar" is the first volume of the old scheme
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0, false
		}
		// .r00 is the SECOND volume: the first is the plain .rar.
		return m[1], n + 2, true
	}
	return "", 0, false
}

// Open answers the decoder's request for a volume.
//
// The name is read for its number and nothing else. A request this set cannot
// answer returns ErrVolumeMissing rather than a bare not-exist, because the
// two mean different things to somebody looking at a half-extracted file: one
// is a set with a hole in it, the other is a wrong path.
func (v *Volumes) Open(name string) (fs.File, error) {
	v.asked = append(v.asked, name)
	_, num, ok := splitVolume(filepath.Base(name))
	if !ok {
		num = 1
	}
	real, found := v.byNum[num]
	if !found {
		return nil, ErrVolumeMissing
	}
	return os.Open(filepath.Join(v.dir, real))
}

// Count is how many volumes were found.
func (v *Volumes) Count() int { return len(v.byNum) }

// Numbers are the volume numbers found, in order, so a caller can see a hole
// rather than discover it halfway through an extraction.
func (v *Volumes) Numbers() []int {
	out := make([]int, 0, len(v.byNum))
	for n := range v.byNum {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// First is the name the decoder should be given as the archive to open. It is
// a canonical name, not a real one: the set answers by number.
func (v *Volumes) First() string { return "volume.part1.rar" }

// Asked reports the names the decoder requested, for tests and diagnosis.
func (v *Volumes) Asked() []string { return v.asked }
