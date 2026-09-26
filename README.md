<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-rar.png" alt="go-filesystems/rar" width="720"></p>

# rar

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/rar.svg)](https://pkg.go.dev/github.com/go-filesystems/rar)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/rar/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/rar/actions/workflows/ci.yml)

Pure-Go access to **RAR** archives — no CGO, no external tools. Reading exposes
an archive through the shared `github.com/go-filesystems/interface`
`Filesystem` API, including multi-volume sets whose files carry extra text in
their names. Writing produces **RAR 5 archives holding stored entries**.

Decoding goes through [`github.com/nwaples/rardecode/v2`](https://github.com/nwaples/rardecode).
The writer is this package's own, built from RARLAB's published container
specification.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | `Open` resolves a whole volume set; `OpenReader` takes one `io.ReaderAt` |
| ReadFile | ✅ | Holds the entry in memory; use `OpenFile` for anything whose size you did not choose |
| Read at an offset | ✅ | `(*FS).OpenFile(path)` → `io.ReaderAt` + `Size()`; forwards is linear, backwards restarts the pass (the format's cost, not this code's) |
| ListDir / Stat | ✅ | Directories the archive never named are synthesised from the paths inside them |
| ReadLink | ✅ | For entries the archive recorded a link target for |
| Short entry detected | ✅ | `ErrShort` rather than a shorter count, so a zero-padded remainder cannot pass for a whole file |
| Multi-volume | ✅ | Resolved by volume **number** within a directory, so `film.part2 [49736858].rar` still chains |
| Encrypted archives | ✅ | `Password(...)`, passed through to the decoder |
| Mutate an opened archive | ❌ | Read-only: every `filesystem.Filesystem` mutator returns `ErrReadOnly` |
| **Write: stored entries** | ✅ | `NewWriter` / `AddDir` / `AddFile` / `Close`; RAR 5 container, method 0 |
| **Write: compressed entries** | ⛔ | **Not possible to implement lawfully — see below.** Not a missing feature |
| Write: RAR 4 | ❌ | RAR 5 only |
| Write: multi-volume, encryption, solid | ❌ | Single volume, no encryption, no solid blocks |

## Why there is no compressed writer

RAR's compression is **proprietary and unpublished**. The only full description
of the algorithms is UnRAR's source, and its licence permits reading RAR
archives while forbidding precisely the use a compressing writer would need —
clause 2, in full:

> UnRAR source code may be used in any software to handle RAR archives without
> limitations free of charge, **but cannot be used to develop RAR (WinRAR)
> compatible archiver and to re-create RAR compression algorithm, which is
> proprietary.**

A compressing RAR writer is the "RAR compatible archiver" that clause
forecloses, and the algorithm is available from nowhere else. So there is
nothing here to finish later: an entry this package cannot store uncompressed
is an entry it cannot write at all.

What **is** published is the container. RARLAB's
["RAR 5.0 archive format" technote](https://www.rarlab.com/technote.htm)
documents the block headers, the variable-length integers, the CRCs, the flags
and the volume linkage — and the stored method needs the container and nothing
else, which is the whole reason an independent writer is possible.

**No UnRAR-derived source was read, consulted, ported or translated** in writing
this package. That includes 7-Zip's own RAR sources (`CPP/7zip/Compress/Rar*`),
which 7-Zip distributes under "GNU LGPL with unRAR license restriction" and
which are therefore the same code under another cover. `7zz` is used here only
as a **program**, run over finished archives in the tests to see whether it
agrees.

## Module

```
github.com/go-filesystems/rar
```

## Usage

### Reading

```go
fs, err := rar.Open("film.part1.rar")
if err != nil { /* ... */ }
defer fs.Close()

entries, err := fs.ListDir(".")
data, err := fs.ReadFile("film.mkv")
```

### Writing

```go
f, err := os.Create("out.rar")
if err != nil { /* ... */ }
defer f.Close()

z := rar.NewWriter(f)
if err := z.AddDir("docs", 0o755); err != nil { /* ... */ }
if err := z.AddFile("docs/notes.txt", 0o644, int64(len(body)), bytes.NewReader(body)); err != nil { /* ... */ }
if err := z.Close(); err != nil { /* ... */ }
```

`AddDir`/`AddFile` are `github.com/go-filesystems/overlay`'s `Builder`, so an
overlay seals straight into one.

### Why the writer needs an `io.WriteSeeker`

A file header carries a CRC32 of the entry's **unpacked** data, and the header
is written *before* that data is read. The alternatives are to hold every entry
in memory to checksum it first, or to leave the checksum out — and the second is
not the small compromise it looks like. With no checksum in a stored entry's
header there is nothing for a reader to verify: `7zz t` answers *"Everything is
Ok"* for an archive whose bytes have been altered under it. (That is measured,
not assumed — `TestAblationTheDataChecksum`.)

So the header goes out with the field zeroed, the data streams past a running
CRC, and the header is rewritten in place. It is the same length either way,
because only a fixed four-byte field changed.

Entries carry **no timestamp**: the `Builder` contract offers none, and a time
this package invented would be a fact about when it ran rather than about the
file. Leaving the field out also makes the same input produce the same bytes.

## How the writer is tested

Two independent readers judge every archive, and both are shown to be capable of
refusing one:

- **`github.com/nwaples/rardecode/v2`** reads every entry back and its bytes are
  compared per entry.
- **`7zz`** extracts every entry and the extracted bytes are compared on disk.
  Its exit status is not the assertion — the contents are.

Every container decision is then **ablated**: the vint encoding, which fields
the header CRC spans, the method bits, the data-size flag, the directory flag,
the end-of-archive block, the name encoding, the data checksum. An ablation that
still passes is reported as such in the test rather than removed.

## References

- RARLAB, "RAR 5.0 archive format" — <https://www.rarlab.com/technote.htm>
- `github.com/nwaples/rardecode/v2` (BSD-3-Clause), for the container layout it
  already parses
