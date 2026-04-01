package fastboot

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestSplitFlashDataKeepsSmallImageRaw(t *testing.T) {
	data := []byte("small raw image")

	parts, err := splitFlashData(data, autoSparseMaxDownloadSize)
	if err != nil {
		t.Fatalf("splitFlashData() error = %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if !bytes.Equal(parts[0], data) {
		t.Fatalf("expected raw payload to be unchanged")
	}
}

func TestSplitFlashDataSplitsLargeRawImage(t *testing.T) {
	raw := buildPatternedData(3*int(androidSparseDefaultBlockSize) + 173)
	limit := uint64(androidSparseFileHeaderSize) + 3*uint64(androidSparseChunkHeaderSize) + 2*uint64(androidSparseDefaultBlockSize)

	parts, err := splitFlashData(raw, limit)
	if err != nil {
		t.Fatalf("splitFlashData() error = %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected multiple sparse parts, got %d", len(parts))
	}

	assertSparsePartsReconstruct(t, parts, raw, int(androidSparseDefaultBlockSize))
	for i, part := range parts {
		if uint64(len(part)) > limit {
			t.Fatalf("part %d exceeds limit: %d > %d", i, len(part), limit)
		}
	}
}

func TestForEachFlashDataSplitsLargeRawImage(t *testing.T) {
	raw := buildPatternedData(3*int(androidSparseDefaultBlockSize) + 173)
	limit := uint64(androidSparseFileHeaderSize) + 3*uint64(androidSparseChunkHeaderSize) + 2*uint64(androidSparseDefaultBlockSize)

	parts := make([][]byte, 0, 2)
	err := forEachFlashData(raw, limit, func(index int, payload []byte) error {
		if index != len(parts)+1 {
			t.Fatalf("unexpected callback index: got %d want %d", index, len(parts)+1)
		}
		if uint64(len(payload)) > limit {
			t.Fatalf("part %d exceeds limit: %d > %d", index, len(payload), limit)
		}

		part := make([]byte, len(payload))
		copy(part, payload)
		parts = append(parts, part)
		return nil
	})
	if err != nil {
		t.Fatalf("forEachFlashData() error = %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected multiple sparse parts, got %d", len(parts))
	}

	assertSparsePartsReconstruct(t, parts, raw, int(androidSparseDefaultBlockSize))
}

func TestForEachRawFileFlashDataSplitsLargeRawImage(t *testing.T) {
	raw := buildPatternedData(3*int(androidSparseDefaultBlockSize) + 173)
	limit := uint64(androidSparseFileHeaderSize) + 3*uint64(androidSparseChunkHeaderSize) + 2*uint64(androidSparseDefaultBlockSize)

	reader := bytes.NewReader(raw)
	parts := make([][]byte, 0, 2)
	err := forEachRawFileFlashData(reader, uint64(len(raw)), limit, func(index int, payload []byte) error {
		if index != len(parts)+1 {
			t.Fatalf("unexpected callback index: got %d want %d", index, len(parts)+1)
		}
		if uint64(len(payload)) > limit {
			t.Fatalf("part %d exceeds limit: %d > %d", index, len(payload), limit)
		}

		part := make([]byte, len(payload))
		copy(part, payload)
		parts = append(parts, part)
		return nil
	})
	if err != nil {
		t.Fatalf("forEachRawFileFlashData() error = %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected multiple sparse parts, got %d", len(parts))
	}

	assertSparsePartsReconstruct(t, parts, raw, int(androidSparseDefaultBlockSize))
}

func TestForEachRawFileFlashStreamSplitsLargeRawImage(t *testing.T) {
	raw := buildPatternedData(3*int(androidSparseDefaultBlockSize) + 173)
	limit := uint64(androidSparseFileHeaderSize) + 3*uint64(androidSparseChunkHeaderSize) + 2*uint64(androidSparseDefaultBlockSize)

	reader := bytes.NewReader(raw)
	parts := make([][]byte, 0, 2)
	err := forEachRawFileFlashStream(reader, uint64(len(raw)), limit, func(index int, size uint64, payload io.Reader) error {
		if index != len(parts)+1 {
			t.Fatalf("unexpected callback index: got %d want %d", index, len(parts)+1)
		}

		part, err := io.ReadAll(payload)
		if err != nil {
			t.Fatalf("failed to read streamed payload: %v", err)
		}
		if uint64(len(part)) != size {
			t.Fatalf("streamed part %d size mismatch: got %d want %d", index, len(part), size)
		}
		if size > limit {
			t.Fatalf("part %d exceeds limit: %d > %d", index, size, limit)
		}

		parts = append(parts, part)
		return nil
	})
	if err != nil {
		t.Fatalf("forEachRawFileFlashStream() error = %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected multiple sparse parts, got %d", len(parts))
	}

	assertSparsePartsReconstruct(t, parts, raw, int(androidSparseDefaultBlockSize))
}

func TestForEachEncodedPieceStreamResplitsSparseInput(t *testing.T) {
	raw := buildPatternedData(4 * int(androidSparseDefaultBlockSize))
	baseImg, err := newAndroidSparseImage(raw)
	if err != nil {
		t.Fatalf("newAndroidSparseImage() error = %v", err)
	}

	singleSparse, err := baseImg.encode(baseImg.chunks)
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}

	limit := uint64(androidSparseFileHeaderSize) + 3*uint64(androidSparseChunkHeaderSize) + 2*uint64(androidSparseDefaultBlockSize)
	reader := bytes.NewReader(singleSparse)
	streamImg, err := parseAndroidSparseImageReaderAt(reader, uint64(len(singleSparse)))
	if err != nil {
		t.Fatalf("parseAndroidSparseImageReaderAt() error = %v", err)
	}

	parts := make([][]byte, 0, 2)
	err = streamImg.forEachEncodedPieceStream(reader, limit, func(index int, size uint64, payload io.Reader) error {
		if index != len(parts)+1 {
			t.Fatalf("unexpected callback index: got %d want %d", index, len(parts)+1)
		}

		part, err := io.ReadAll(payload)
		if err != nil {
			t.Fatalf("failed to read streamed sparse payload: %v", err)
		}
		if uint64(len(part)) != size {
			t.Fatalf("streamed sparse part %d size mismatch: got %d want %d", index, len(part), size)
		}
		if size > limit {
			t.Fatalf("part %d exceeds limit: %d > %d", index, size, limit)
		}

		parts = append(parts, part)
		return nil
	})
	if err != nil {
		t.Fatalf("forEachEncodedPieceStream() error = %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected sparse input to be re-split, got %d part(s)", len(parts))
	}

	assertSparsePartsReconstruct(t, parts, raw, int(androidSparseDefaultBlockSize))
}

func TestSplitFlashDataResplitsSparseInput(t *testing.T) {
	raw := buildPatternedData(4 * int(androidSparseDefaultBlockSize))
	img, err := newAndroidSparseImage(raw)
	if err != nil {
		t.Fatalf("newAndroidSparseImage() error = %v", err)
	}

	singleSparse, err := img.encode(img.chunks)
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}

	limit := uint64(androidSparseFileHeaderSize) + 3*uint64(androidSparseChunkHeaderSize) + 2*uint64(androidSparseDefaultBlockSize)
	parts, err := splitFlashData(singleSparse, limit)
	if err != nil {
		t.Fatalf("splitFlashData() error = %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected sparse input to be re-split, got %d part(s)", len(parts))
	}

	assertSparsePartsReconstruct(t, parts, raw, int(androidSparseDefaultBlockSize))
}

func assertSparsePartsReconstruct(t *testing.T, parts [][]byte, raw []byte, blockSize int) {
	t.Helper()

	expected := make([]byte, int(alignUp(uint64(len(raw)), uint64(blockSize))))
	copy(expected, raw)

	merged, err := mergeSparseParts(parts)
	if err != nil {
		t.Fatalf("mergeSparseParts() error = %v", err)
	}

	if !bytes.Equal(merged, expected) {
		t.Fatalf("reconstructed sparse output does not match original data")
	}
}

func mergeSparseParts(parts [][]byte) ([]byte, error) {
	var merged []byte

	for _, part := range parts {
		img, err := parseAndroidSparseImage(part)
		if err != nil {
			return nil, err
		}

		if merged == nil {
			merged = make([]byte, int(img.totalBlocks()*uint64(img.blockSize)))
		}

		for _, chunk := range img.chunks {
			offset := int(uint64(chunk.block) * uint64(img.blockSize))
			switch chunk.chunkType {
			case androidSparseChunkRaw:
				copy(merged[offset:], chunk.data)
			case androidSparseChunkFill:
				fill := make([]byte, 4)
				binary.LittleEndian.PutUint32(fill, chunk.fillValue)
				for written := uint64(0); written < chunk.length; written += 4 {
					copy(merged[offset+int(written):], fill)
				}
			}
		}
	}

	return merged, nil
}

func buildPatternedData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i * 31) % 251)
	}
	return data
}
