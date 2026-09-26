// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

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

	// ErrWriterClosed is returned by a Writer that has already been closed.
	ErrWriterClosed = errors.New("rar: writer is closed")

	// ErrBadName is returned for an entry name the archive cannot carry
	// unchanged: absolute, climbing out of the archive, or holding a character a
	// RAR reader would read as something else. See the Writer.
	ErrBadName = errors.New("rar: an entry name the archive cannot carry")

	// ErrSizeMismatch is returned when the reader handed to AddFile delivers a
	// different number of bytes than the size it was called with.
	//
	// The size is in the header before the bytes are read, so a mismatch cannot
	// be absorbed: it has to fail at the call that made the promise, while the
	// entry still has a name to report, rather than become an archive whose
	// header disagrees with its data.
	ErrSizeMismatch = errors.New("rar: the entry did not deliver the size it declared")
)
