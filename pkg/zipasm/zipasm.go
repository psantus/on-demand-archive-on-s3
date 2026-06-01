// Package zipasm builds zip archives in STORE mode with pre-calculable offsets,
// enabling parallel assembly by independent workers.
package zipasm

import (
	"encoding/binary"
	"hash/crc32"
	"io"
)

// FileEntry describes one file to include in the zip.
type FileEntry struct {
	Name   string
	Size   uint64
	CRC32  uint32
	Offset uint64 // byte offset of local file header in the zip
}

// ZipPlan holds the computed layout for a zip archive.
type ZipPlan struct {
	Entries       []FileEntry
	CDOffset      uint64 // byte offset where central directory starts
	TotalSize     uint64 // total zip file size including EOCD
	CDSize        uint64
	UseZip64      bool
}

// Plan computes offsets for all entries. CRC32 fields may be zero (filled later).
func Plan(names []string, sizes []uint64) *ZipPlan {
	entries := make([]FileEntry, len(names))
	var offset uint64
	for i := range names {
		entries[i] = FileEntry{
			Name:   names[i],
			Size:   sizes[i],
			Offset: offset,
		}
		offset += LocalFileHeaderSize(names[i]) + sizes[i]
	}
	cdOffset := offset
	var cdSize uint64
	for _, e := range entries {
		cdSize += 46 + uint64(len(e.Name)) + 28 // 28 = zip64 extra in CD
	}
	eocdSize := uint64(56 + 20 + 22) // zip64 EOCD + locator + standard EOCD
	return &ZipPlan{
		Entries:   entries,
		CDOffset:  cdOffset,
		CDSize:    cdSize,
		TotalSize: cdOffset + cdSize + eocdSize,
	}
}

// LocalFileHeader returns the local file header bytes for an entry (with zip64 extra).
func LocalFileHeader(e FileEntry) []byte {
	name := []byte(e.Name)
	extra := make([]byte, 20) // zip64 extra field
	binary.LittleEndian.PutUint16(extra[0:], 0x0001)
	binary.LittleEndian.PutUint16(extra[2:], 16)
	binary.LittleEndian.PutUint64(extra[4:], e.Size)  // uncompressed
	binary.LittleEndian.PutUint64(extra[12:], e.Size) // compressed

	buf := make([]byte, 30+len(name)+len(extra))
	binary.LittleEndian.PutUint32(buf[0:], 0x04034b50)  // signature
	binary.LittleEndian.PutUint16(buf[4:], 45)           // version needed (4.5 = zip64)
	binary.LittleEndian.PutUint16(buf[6:], 0)            // flags
	binary.LittleEndian.PutUint16(buf[8:], 0)            // compression: STORE
	binary.LittleEndian.PutUint16(buf[10:], 0)           // mod time
	binary.LittleEndian.PutUint16(buf[12:], 0)           // mod date
	binary.LittleEndian.PutUint32(buf[14:], e.CRC32)     // crc32
	binary.LittleEndian.PutUint32(buf[18:], 0xFFFFFFFF)  // compressed size (zip64)
	binary.LittleEndian.PutUint32(buf[22:], 0xFFFFFFFF)  // uncompressed size (zip64)
	binary.LittleEndian.PutUint16(buf[26:], uint16(len(name)))
	binary.LittleEndian.PutUint16(buf[28:], uint16(len(extra)))
	copy(buf[30:], name)
	copy(buf[30+len(name):], extra)
	return buf
}

// LocalFileHeaderSize returns the size of a local file header for a given filename.
func LocalFileHeaderSize(name string) uint64 {
	return 30 + uint64(len(name)) + 20 // 20 = zip64 extra field
}

// CentralDirectory builds the central directory bytes for all entries.
func CentralDirectory(entries []FileEntry) []byte {
	var size int
	for _, e := range entries {
		size += 46 + len(e.Name)
	}
	buf := make([]byte, 0, size)
	for _, e := range entries {
		rec := make([]byte, 46+len(e.Name))
		binary.LittleEndian.PutUint32(rec[0:], 0x02014b50)  // signature
		binary.LittleEndian.PutUint16(rec[4:], 45)           // version made by (4.5 = zip64)
		binary.LittleEndian.PutUint16(rec[6:], 45)           // version needed
		binary.LittleEndian.PutUint16(rec[8:], 0)            // flags
		binary.LittleEndian.PutUint16(rec[10:], 0)           // compression: STORE
		binary.LittleEndian.PutUint16(rec[12:], 0)           // mod time
		binary.LittleEndian.PutUint16(rec[14:], 0)           // mod date
		binary.LittleEndian.PutUint32(rec[16:], e.CRC32)
		binary.LittleEndian.PutUint32(rec[20:], 0xFFFFFFFF)  // compressed (zip64)
		binary.LittleEndian.PutUint32(rec[24:], 0xFFFFFFFF)  // uncompressed (zip64)
		binary.LittleEndian.PutUint16(rec[28:], uint16(len(e.Name)))
		binary.LittleEndian.PutUint16(rec[30:], 28) // extra field length (zip64 extra)
		binary.LittleEndian.PutUint16(rec[32:], 0)  // comment len
		binary.LittleEndian.PutUint16(rec[34:], 0)  // disk number start
		binary.LittleEndian.PutUint16(rec[36:], 0)  // internal attrs
		binary.LittleEndian.PutUint32(rec[38:], 0)  // external attrs
		binary.LittleEndian.PutUint32(rec[42:], 0xFFFFFFFF) // offset (zip64)
		copy(rec[46:], e.Name)
		// zip64 extra field
		extra := make([]byte, 28)
		binary.LittleEndian.PutUint16(extra[0:], 0x0001) // zip64 tag
		binary.LittleEndian.PutUint16(extra[2:], 24)     // data size
		binary.LittleEndian.PutUint64(extra[4:], e.Size)   // uncompressed
		binary.LittleEndian.PutUint64(extra[12:], e.Size)  // compressed
		binary.LittleEndian.PutUint64(extra[20:], e.Offset) // local header offset
		buf = append(buf, rec...)
		buf = append(buf, extra...)
	}
	return buf
}

// EOCD returns the ZIP64 end-of-central-directory record + locator + standard EOCD.
func EOCD(cdOffset, cdSize uint64, count uint16) []byte {
	// ZIP64 EOCD record (56 bytes)
	z64eocd := make([]byte, 56)
	binary.LittleEndian.PutUint32(z64eocd[0:], 0x06064b50)  // zip64 EOCD sig
	binary.LittleEndian.PutUint64(z64eocd[4:], 44)           // size of remaining record
	binary.LittleEndian.PutUint16(z64eocd[12:], 45)          // version made by
	binary.LittleEndian.PutUint16(z64eocd[14:], 45)          // version needed
	binary.LittleEndian.PutUint32(z64eocd[16:], 0)           // disk number
	binary.LittleEndian.PutUint32(z64eocd[20:], 0)           // disk with CD
	binary.LittleEndian.PutUint64(z64eocd[24:], uint64(count)) // entries on disk
	binary.LittleEndian.PutUint64(z64eocd[32:], uint64(count)) // total entries
	binary.LittleEndian.PutUint64(z64eocd[40:], cdSize)       // CD size
	binary.LittleEndian.PutUint64(z64eocd[48:], cdOffset)     // CD offset

	// ZIP64 EOCD locator (20 bytes)
	z64loc := make([]byte, 20)
	binary.LittleEndian.PutUint32(z64loc[0:], 0x07064b50)           // locator sig
	binary.LittleEndian.PutUint32(z64loc[4:], 0)                     // disk with zip64 EOCD
	binary.LittleEndian.PutUint64(z64loc[8:], cdOffset+cdSize)       // offset of zip64 EOCD
	binary.LittleEndian.PutUint32(z64loc[16:], 1)                    // total disks

	// Standard EOCD (22 bytes) with 0xFFFF markers
	eocd := make([]byte, 22)
	binary.LittleEndian.PutUint32(eocd[0:], 0x06054b50)
	binary.LittleEndian.PutUint16(eocd[4:], 0)          // disk number
	binary.LittleEndian.PutUint16(eocd[6:], 0)          // disk with CD
	binary.LittleEndian.PutUint16(eocd[8:], 0xFFFF)     // entries (zip64)
	binary.LittleEndian.PutUint16(eocd[10:], 0xFFFF)    // total entries (zip64)
	binary.LittleEndian.PutUint32(eocd[12:], 0xFFFFFFFF) // CD size (zip64)
	binary.LittleEndian.PutUint32(eocd[16:], 0xFFFFFFFF) // CD offset (zip64)
	binary.LittleEndian.PutUint16(eocd[20:], 0)          // comment length

	var buf []byte
	buf = append(buf, z64eocd...)
	buf = append(buf, z64loc...)
	buf = append(buf, eocd...)
	return buf
}

// CRC32Stream computes CRC32 while copying from reader to writer.
func CRC32Stream(w io.Writer, r io.Reader) (uint32, int64, error) {
	h := crc32.NewIEEE()
	mw := io.MultiWriter(w, h)
	n, err := io.Copy(mw, r)
	return h.Sum32(), n, err
}
