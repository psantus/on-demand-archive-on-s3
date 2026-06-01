package zipasm

import (
	"archive/zip"
	"bytes"
	"hash/crc32"
	"io"
	"testing"
)

func TestPlanOffsets(t *testing.T) {
	names := []string{"a.txt", "bb.txt"}
	sizes := []uint64{10, 20}
	p := Plan(names, sizes)

	// First entry at offset 0
	if p.Entries[0].Offset != 0 {
		t.Fatalf("entry 0 offset = %d, want 0", p.Entries[0].Offset)
	}
	// Second entry at 30 + 5("a.txt") + 10(data) = 45
	want := uint64(30 + 5 + 10)
	if p.Entries[1].Offset != want {
		t.Fatalf("entry 1 offset = %d, want %d", p.Entries[1].Offset, want)
	}
	// CDOffset = 45 + 30 + 6("bb.txt") + 20 = 101
	wantCD := want + 30 + 6 + 20
	if p.CDOffset != wantCD {
		t.Fatalf("CDOffset = %d, want %d", p.CDOffset, wantCD)
	}
}

func TestProducesValidZip(t *testing.T) {
	files := []struct {
		name string
		data []byte
	}{
		{"hello.txt", []byte("Hello, World!")},
		{"sub/data.bin", bytes.Repeat([]byte{0xAB}, 1024)},
	}

	names := make([]string, len(files))
	sizes := make([]uint64, len(files))
	for i, f := range files {
		names[i] = f.name
		sizes[i] = uint64(len(f.data))
	}

	plan := Plan(names, sizes)

	// Fill CRC32
	for i, f := range files {
		plan.Entries[i].CRC32 = crc32.ChecksumIEEE(f.data)
	}

	// Assemble zip bytes
	var buf bytes.Buffer
	for i, e := range plan.Entries {
		buf.Write(LocalFileHeader(e))
		buf.Write(files[i].data)
	}
	buf.Write(CentralDirectory(plan.Entries))
	buf.Write(EOCD(plan.CDOffset, plan.CDSize, uint16(len(plan.Entries))))

	// Verify with archive/zip reader
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(r.File) != len(files) {
		t.Fatalf("got %d files, want %d", len(r.File), len(files))
	}
	for i, zf := range r.File {
		if zf.Name != files[i].name {
			t.Errorf("file %d name = %q, want %q", i, zf.Name, files[i].name)
		}
		rc, err := zf.Open()
		if err != nil {
			t.Fatalf("open %s: %v", zf.Name, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, files[i].data) {
			t.Errorf("file %d data mismatch", i)
		}
	}
}

func TestCRC32Stream(t *testing.T) {
	data := []byte("test data for crc")
	want := crc32.ChecksumIEEE(data)
	var buf bytes.Buffer
	got, n, err := CRC32Stream(&buf, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) {
		t.Fatalf("n = %d, want %d", n, len(data))
	}
	if got != want {
		t.Fatalf("crc = %08x, want %08x", got, want)
	}
}
