// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package rar

// Defaults for [Limits], and the two limits that are not tunable.
const (
	// DefaultMaxEntries bounds the paths an archive may name, counting the
	// directories it implies (1,048,576).
	DefaultMaxEntries = 1 << 20

	// DefaultMaxFileSize bounds what ReadFile will buffer (1 GiB).
	DefaultMaxFileSize = 1 << 30

	// DefaultMaxDictionary bounds the decoder's dictionary (1 GiB). The decoder
	// allocates it whole, before decoding a byte, and a RAR 5 header may name up
	// to 64 GiB; the library's own default is 4 GiB. 1 GiB is the largest
	// dictionary WinRAR's dialog offers (RAR 7 can write 4 GiB with -md4g).
	DefaultMaxDictionary = 1 << 30

	// maxPathLen is the longest path accepted: Linux's PATH_MAX. A longer path
	// cannot be extracted anywhere.
	maxPathLen = 4096

	// maxPathBytes bounds all stored paths together.
	maxPathBytes = 256 << 20
)

// Limits bounds what an untrusted archive may ask this package to hold. The
// zero value selects the defaults, so a field is raised by naming it.
//
// A limit that is exceeded fails with an error wrapping [ErrTooLarge] (entries,
// paths, ReadFile) or with the decoder's ErrDictionaryTooLarge (dictionary),
// before the memory is reserved.
type Limits struct {
	// MaxEntries bounds the number of paths. Zero means DefaultMaxEntries.
	MaxEntries int

	// MaxFileSize bounds ReadFile, in unpacked bytes. Zero means
	// DefaultMaxFileSize. OpenFile streams and is not affected.
	MaxFileSize int64

	// MaxDictionary bounds the decoder's dictionary, in bytes. Zero means
	// DefaultMaxDictionary. A rardecode.MaxDictionarySize passed as an Option
	// overrides it.
	MaxDictionary int64
}

func (l Limits) resolved() Limits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = DefaultMaxFileSize
	}
	if l.MaxDictionary <= 0 {
		l.MaxDictionary = DefaultMaxDictionary
	}
	return l
}
