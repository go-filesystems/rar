// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package rar is a pure-Go driver for the RAR archive format, reading an
// archive as a filesystem.Filesystem.
//
// It decodes through github.com/nwaples/rardecode/v2, which is pure Go and
// maintained; this package is the go-filesystems shape around it -- the
// Filesystem/Opener contract, the volume resolver, and an extraction that can
// say whether an entry came out whole.
//
// # Read-only
//
// An archive is read-only: every mutating method of filesystem.Filesystem
// (WriteFile, MkDir, DeleteFile, DeleteDir, Rename) returns ErrReadOnly, as in
// the other read-only drivers of this org.
//
// # Multi-volume
//
// A RAR set may be split across files, and the format finds the next one BY
// NAME -- part1 asks for part2. Any text a downloader or a tidy-up has added
// to those names breaks the chain, and what happens then is worse than an
// error: some readers pad the missing remainder with zeros and report success,
// so the file is the right LENGTH and the wrong content. [Volumes] resolves the
// chain by volume NUMBER within a directory instead, so a set survives names
// like "film.part2 [49736858].rar" without anything being renamed or linked.
//
// [OpenReader] takes a single io.ReaderAt, which is the shape
// github.com/go-filesystems/detect opens every driver with; it can therefore
// only serve a single-volume archive, and says so. [Open] takes a path and
// resolves the whole set.
package rar

import (
	"errors"
	"fmt"
	iofs "io/fs"
)

// Sentinel errors. Compare with errors.Is so wrapped errors continue to match.
var (
	// ErrReadOnly is returned by every mutating method (WriteFile, MkDir,
	// DeleteFile, DeleteDir, Rename). An archive is a read-only format.
	ErrReadOnly = errors.New("rar: archive is read-only")

	// ErrNotFound is returned when a path is not in the archive.
	ErrNotFound = fmt.Errorf("rar: path not found: %w", iofs.ErrNotExist)

	// ErrNotDirectory is returned when ListDir targets a non-directory.
	ErrNotDirectory = errors.New("rar: not a directory")

	// ErrNotRegular is returned when ReadFile targets a non-regular file.
	ErrNotRegular = errors.New("rar: not a regular file")

	// ErrNotSymlink is returned by ReadLink when the target is not a symlink.
	ErrNotSymlink = errors.New("rar: not a symbolic link")

	// ErrShort is returned when an entry yields fewer bytes than its header
	// declares.
	//
	// It exists because the alternative is what this package was written
	// against: a reader that meets the end of the volume chain, pads the
	// remainder with zeros and reports success. The file is then exactly the
	// declared length and its tail is nothing, which no size check can see --
	// and for a container that keeps its index at the end, such as MP4, the
	// result opens nowhere.
	ErrShort = errors.New("rar: entry ended before its declared size")

	// ErrVolumeMissing is returned when the chain asks for a volume the
	// directory does not hold.
	ErrVolumeMissing = errors.New("rar: a volume of this set is missing")
)
