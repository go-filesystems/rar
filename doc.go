// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package rar reads a RAR archive as a filesystem.Filesystem, and writes one
// holding stored entries.
//
// It decodes through github.com/nwaples/rardecode/v2, which is pure Go and
// maintained; this package is the go-filesystems shape around it -- the
// Filesystem/Opener contract, the volume resolver, and an extraction that can
// say whether an entry came out whole. [NewWriter] is the other direction, and
// owes that library nothing: it writes the container itself.
//
// # Reading: multi-volume
//
// A RAR set may be split across files, and the format finds the next one BY
// NAME -- part1 asks for part2. Any text a downloader or a tidy-up has added to
// those names breaks the chain, and what happens then is worse than an error:
// some readers pad the missing remainder with zeros and report success, so the
// file is the right LENGTH and the wrong content. [Volumes] resolves the chain
// by volume NUMBER within a directory instead, so a set survives names like
// "film.part2 [49736858].rar" without anything being renamed or linked.
//
// [OpenReader] takes a single io.ReaderAt, which is the shape
// github.com/go-filesystems/detect opens every driver with; it can therefore
// only serve a single-volume archive, and says so. [Open] takes a path and
// resolves the whole set.
//
// # Reading: an archive is read-only
//
// An archive opened for reading is read-only: every mutating method of
// filesystem.Filesystem (WriteFile, MkDir, DeleteFile, DeleteDir, Rename)
// returns [ErrReadOnly], as in the other read-only drivers of this org. Writing
// an archive means building a new one with [NewWriter], which is what
// github.com/go-filesystems/overlay seals into.
//
// # Writing: the stored method, and why there is no other
//
// [NewWriter] writes RAR 5 archives whose entries are STORED -- held
// byte-for-byte, uncompressed. It writes no compressed entry, and this is not a
// feature that has not been got to yet. It is the boundary of what may lawfully
// be written at all.
//
// RAR's compression is proprietary and its algorithms are unpublished. The only
// full description of them is UnRAR's source, whose licence permits reading RAR
// archives and forbids the one use that would be needed here. Clause 2, in full:
//
//	UnRAR source code may be used in any software to handle RAR archives
//	without limitations free of charge, but cannot be used to develop RAR
//	(WinRAR) compatible archiver and to re-create RAR compression algorithm,
//	which is proprietary.
//
// A compressing RAR writer is exactly the "RAR compatible archiver" that clause
// forecloses, and the algorithm is not available from anywhere else to write one
// independently. So there is nothing to implement later: an entry this package
// cannot store uncompressed is an entry it cannot write.
//
// What IS published is the container. RARLAB's "RAR 5.0 archive format"
// technote (https://www.rarlab.com/technote.htm) documents the block headers,
// the variable-length integers, the CRCs, the flags and the volume linkage --
// and the stored method needs the container and nothing else, which is why an
// independent writer of it is possible at all.
//
// # What this package was built from
//
// The writer was built from that published technote, and checked against two
// independent readers that accept what it produces: github.com/nwaples/rardecode/v2
// and 7-Zip's 7zz.
//
// No UnRAR-derived source was read, consulted, ported or translated. That
// includes 7-Zip's own RAR sources (CPP/7zip/Compress/Rar*), which 7-Zip
// distributes under "GNU LGPL with unRAR license restriction" and which are
// therefore the same code under another cover. 7zz is used here only as a
// program, run on a finished archive to see whether it agrees -- which is the
// use its licence and UnRAR's both allow, and the only one this package makes of
// it.
//
// rardecode was read, for the container layout it already parses. It carries no
// UnRAR-derived code: it is BSD-licensed original work by Nicholas Waples, and a
// search of its source for any mention of UnRAR or Roshal finds nothing.
package rar
