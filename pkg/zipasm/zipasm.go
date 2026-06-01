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
		offset += 30 + uint64(len(names[i])) + sizes[i]
	}
	cdOffset := offset
	var cdSize uint64
	for _, e := range entries {
		cdSize += 46 + uint64(len(e.Name))
	}
	eocdSize := uint64(22)
	return &ZipPlan{
		Entries:   entries,
		CDOffset:  cdOffset,
		CDSize:    cdSize,
		TotalSize: cdOffset + cdSize + eocdSize,
	}
}

// LocalFileHeader returns the 30+len(name) byte local file header for an entry.
func LocalFileHeader(e FileEntry) []byte {
	name := []byte(e.Name)
	buf := make([]byte, 30+len(name))
	binary.LittleEndian.PutUint32(buf[0:], 0x04034b50)  // signature
	binary.LittleEndian.PutUint16(buf[4:], 20)           // version needed
	binary.LittleEndian.PutUint16(buf[6:], 0)            // flags
	binary.LittleEndian.PutUint16(buf[8:], 0)            // compression: STORE
	binary.LittleEndian.PutUint16(buf[10:], 0)           // mod time
	binary.LittleEndian.PutUint16(buf[12:], 0)           // mod date
	binary.LittleEndian.PutUint32(buf[14:], e.CRC32)     // crc32
	binary.LittleEndian.PutUint32(buf[18:], uint32(e.Size)) // compressed size
	binary.LittleEndian.PutUint32(buf[22:], uint32(e.Size)) // uncompressed size
	binary.LittleEndian.PutUint16(buf[26:], uint16(len(name)))
	binary.LittleEndian.PutUint16(buf[28:], 0) // extra field length
	copy(buf[30:], name)
	return buf
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
		binary.LittleEndian.PutUint16(rec[4:], 20)           // version made by
		binary.LittleEndian.PutUint16(rec[6:], 20)           // version needed
		binary.LittleEndian.PutUint16(rec[8:], 0)            // flags
		binary.LittleEndian.PutUint16(rec[10:], 0)           // compression: STORE
		binary.LittleEndian.PutUint16(rec[12:], 0)           // mod time
		binary.LittleEndian.PutUint16(rec[14:], 0)           // mod date
		binary.LittleEndian.PutUint32(rec[16:], e.CRC32)
		binary.LittleEndian.PutUint32(rec[20:], uint32(e.Size)) // compressed
		binary.LittleEndian.PutUint32(rec[24:], uint32(e.Size)) // uncompressed
		binary.LittleEndian.PutUint16(rec[28:], uint16(len(e.Name)))
		binary.LittleEndian.PutUint16(rec[30:], 0) // extra len
		binary.LittleEndian.PutUint16(rec[32:], 0) // comment len
		binary.LittleEndian.PutUint16(rec[34:], 0) // disk number start
		binary.LittleEndian.PutUint16(rec[36:], 0) // internal attrs
		binary.LittleEndian.PutUint32(rec[38:], 0) // external attrs
		binary.LittleEndian.PutUint32(rec[42:], uint32(e.Offset))
		copy(rec[46:], e.Name)
		buf = append(buf, rec...)
	}
	return buf
}

// EOCD returns the 22-byte end-of-central-directory record.
func EOCD(cdOffset, cdSize uint64, count uint16) []byte {
	buf := make([]byte, 22)
	binary.LittleEndian.PutUint32(buf[0:], 0x06054b50)
	binary.LittleEndian.PutUint16(buf[4:], 0)      // disk number
	binary.LittleEndian.PutUint16(buf[6:], 0)      // disk with CD
	binary.LittleEndian.PutUint16(buf[8:], count)  // entries on disk
	binary.LittleEndian.PutUint16(buf[10:], count) // total entries
	binary.LittleEndian.PutUint32(buf[12:], uint32(cdSize))
	binary.LittleEndian.PutUint32(buf[16:], uint32(cdOffset))
	binary.LittleEndian.PutUint16(buf[20:], 0) // comment length
	return buf
}

// CRC32Stream computes CRC32 while copying from reader to writer.
func CRC32Stream(w io.Writer, r io.Reader) (uint32, int64, error) {
	h := crc32.NewIEEE()
	mw := io.MultiWriter(w, h)
	n, err := io.Copy(mw, r)
	return h.Sum32(), n, err
}
