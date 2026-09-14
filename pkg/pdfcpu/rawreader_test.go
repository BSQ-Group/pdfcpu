/*
Copyright 2026 The pdfcpu Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pdfcpu_test

import (
	"bytes"
	"compress/zlib"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/filter"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/validate"
)

// sourceBackedContext returns a one page context whose content stream is available via RawReader only.
// The stream holds size bytes of content stream operators stored in a file.
// It also returns the expected bytes and a counter of RawReader calls.
func sourceBackedContext(t *testing.T, size int) (*model.Context, []byte, *int) {
	t.Helper()

	dir := t.TempDir()
	op := []byte("0 0 m 100 100 l S\n")
	var content bytes.Buffer
	for content.Len() < size {
		content.Write(op)
	}
	want := content.Bytes()
	src := filepath.Join(dir, "content.bin")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	length := int64(len(want))
	opened := 0
	sd := types.StreamDict{Dict: types.NewDict(), StreamLength: &length}
	sd.Insert("Length", types.Integer(length))
	sd.RawReader = func() (io.Reader, error) {
		opened++
		return io.NewSectionReader(f, 0, length), nil
	}

	xref, err := pdfcpu.CreateXRefTableWithRootDict()
	if err != nil {
		t.Fatal(err)
	}
	contentsRef, err := xref.IndRefForNewObject(sd)
	if err != nil {
		t.Fatal(err)
	}
	page := types.Dict(map[string]types.Object{
		"Type":     types.Name("Page"),
		"MediaBox": types.NewNumberArray(0, 0, 595, 842),
		"Contents": *contentsRef,
	})
	pageRef, err := xref.IndRefForNewObject(page)
	if err != nil {
		t.Fatal(err)
	}
	pages := types.Dict(map[string]types.Object{
		"Type":  types.Name("Pages"),
		"Count": types.Integer(1),
		"Kids":  types.Array{*pageRef},
	})
	pagesRef, err := xref.IndRefForNewObject(pages)
	if err != nil {
		t.Fatal(err)
	}
	page.Insert("Parent", *pagesRef)
	catalog, err := xref.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog.Insert("Pages", *pagesRef)
	xref.PageCount = 1

	return pdfcpu.CreateContext(xref, model.NewDefaultConfiguration()), want, &opened
}

// writeToFile writes ctx to a temp file and returns its path.
func writeToFile(t *testing.T, ctx *model.Context) string {
	t.Helper()

	dir := t.TempDir()
	ctx.Write.DirName, ctx.Write.FileName = dir, "out.pdf"
	if err := pdfcpu.WriteContext(ctx); err != nil {
		t.Fatal(err)
	}

	return filepath.Join(dir, "out.pdf")
}

// pageContent reads and validates the PDF at path and returns its context and the content stream of page 1.
func pageContent(t *testing.T, path string) (*model.Context, []byte) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, err := pdfcpu.Read(f, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if err := validate.XRefTable(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := pdfcpu.ExtractPageContent(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	return ctx, got
}

// TestWriteCopiesSourceBackedStreamWithoutBuffering verifies a source-backed stream is written without buffering.
// The heap measurement is process wide, so this test must not run in parallel.
func TestWriteCopiesSourceBackedStreamWithoutBuffering(t *testing.T) {
	const size = 32 << 20 // 32MiB

	ctx, want, opened := sourceBackedContext(t, size)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	path := writeToFile(t, ctx)
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("stream=%d bytes, allocated during write=%d bytes", len(want), allocated)
	if allocated > uint64(len(want))/4 {
		t.Fatalf("write buffered the stream: allocated %d bytes for a %d byte stream", allocated, len(want))
	}
	if *opened != 1 {
		t.Fatalf("source opened %d times, want 1", *opened)
	}

	if _, got := pageContent(t, path); !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

// shortenSource replaces the content stream's source with one byte less than StreamLength.
func shortenSource(t *testing.T, ctx *model.Context) {
	t.Helper()

	var entry *model.XRefTableEntry
	for _, e := range ctx.Table {
		if _, ok := e.Object.(types.StreamDict); ok {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatal("content stream object missing")
	}
	sd := entry.Object.(types.StreamDict)
	short := *sd.StreamLength - 1
	sd.RawReader = func() (io.Reader, error) {
		return bytes.NewReader(make([]byte, short)), nil
	}
	entry.Object = sd
}

// encrypting configures ctx for encryption on write with an owner password only.
func encrypting(ctx *model.Context) {
	ctx.Cmd = model.ENCRYPT
	ctx.OwnerPW = "owner"
}

// TestWriteRefusesSourceBackedStreamShorterThanItsLength verifies WriteContext fails if RawReader yields fewer than StreamLength bytes.
func TestWriteRefusesSourceBackedStreamShorterThanItsLength(t *testing.T) {
	ctx, _, _ := sourceBackedContext(t, 1<<16)
	shortenSource(t, ctx)

	dir := t.TempDir()
	ctx.Write.DirName, ctx.Write.FileName = dir, "out.pdf"
	if err := pdfcpu.WriteContext(ctx); err == nil {
		t.Fatal("write succeeded with a source shorter than /Length")
	}
}

// TestEncryptedWriteCopiesSourceBackedStream verifies encryption loads a source-backed stream before writing it.
func TestEncryptedWriteCopiesSourceBackedStream(t *testing.T) {
	ctx, want, opened := sourceBackedContext(t, 1<<16)
	encrypting(ctx)

	path := writeToFile(t, ctx)

	if *opened != 1 {
		t.Fatalf("source opened %d times, want 1", *opened)
	}
	written, got := pageContent(t, path)
	if written.Encrypt == nil {
		t.Fatal("written file is not encrypted")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

// TestEncryptedWriteRefusesSourceBackedStreamShorterThanItsLength verifies an encrypting WriteContext fails if RawReader yields fewer than StreamLength bytes.
func TestEncryptedWriteRefusesSourceBackedStreamShorterThanItsLength(t *testing.T) {
	ctx, _, _ := sourceBackedContext(t, 1<<16)
	encrypting(ctx)
	shortenSource(t, ctx)

	dir := t.TempDir()
	ctx.Write.DirName, ctx.Write.FileName = dir, "out.pdf"
	if err := pdfcpu.WriteContext(ctx); err == nil {
		t.Fatal("encrypted write succeeded with a source shorter than /Length")
	}
}

// flateSourceBacked returns a Flate encoded stream dict backed by RawReader.
// StreamLength is the length of the compressed data minus cut, so a positive cut leaves the data incomplete.
func flateSourceBacked(t *testing.T, plain []byte, cut int) types.StreamDict {
	t.Helper()

	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if cut >= compressed.Len() {
		t.Fatalf("cut %d exceeds %d compressed bytes", cut, compressed.Len())
	}
	length := int64(compressed.Len() - cut)
	source := compressed.Bytes()

	sd := types.StreamDict{
		Dict:           types.NewDict(),
		StreamLength:   &length,
		FilterPipeline: []types.PDFFilter{{Name: filter.Flate}},
		RawReader:      func() (io.Reader, error) { return bytes.NewReader(source), nil },
	}
	sd.InsertName("Filter", filter.Flate)
	sd.Insert("Length", types.Integer(length))

	return sd
}

// TestDecodeReadsSourceBackedStream verifies decoding a source-backed stream.
func TestDecodeReadsSourceBackedStream(t *testing.T) {
	plain := bytes.Repeat([]byte("decoded "), 1<<12)
	sd := flateSourceBacked(t, plain, 0)

	if err := sd.Decode(); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(sd.Content, plain) {
		t.Fatalf("decoded %d bytes, want %d", len(sd.Content), len(plain))
	}
}

// TestDecodeStopsSourceBackedStreamAtItsLength verifies decoding reads no further than StreamLength.
func TestDecodeStopsSourceBackedStreamAtItsLength(t *testing.T) {
	plain := bytes.Repeat([]byte("decoded "), 1<<12)
	sd := flateSourceBacked(t, plain, 32)

	err := sd.Decode()

	if err == nil && bytes.Equal(sd.Content, plain) {
		t.Fatal("decoding read past StreamLength")
	}
}

// TestExtractImageReadsSourceBackedStream verifies an unfiltered image is extracted from a source-backed stream.
func TestExtractImageReadsSourceBackedStream(t *testing.T) {
	pixels := []byte{0x00, 0x40, 0x80, 0xff}
	length := int64(len(pixels))
	sd := types.StreamDict{
		Dict:         types.NewDict(),
		StreamLength: &length,
		RawReader:    func() (io.Reader, error) { return bytes.NewReader(pixels), nil },
	}
	sd.InsertName("Type", "XObject")
	sd.InsertName("Subtype", "Image")
	sd.InsertInt("Width", 2)
	sd.InsertInt("Height", 2)
	sd.InsertName("ColorSpace", "DeviceGray")
	sd.InsertInt("BitsPerComponent", 8)
	sd.Insert("Length", types.Integer(length))
	xref, err := pdfcpu.CreateXRefTableWithRootDict()
	if err != nil {
		t.Fatal(err)
	}
	ctx := pdfcpu.CreateContext(xref, model.NewDefaultConfiguration())

	img, err := pdfcpu.ExtractImage(ctx, &sd, false, "Im0", 1, false)
	if err != nil {
		t.Fatal(err)
	}

	if img == nil {
		t.Fatal("no image extracted")
	}
	decoded, err := png.Decode(img)
	if err != nil {
		t.Fatalf("extracted %s image does not decode: %v", img.FileType, err)
	}
	if b := decoded.Bounds(); b.Dx() != 2 || b.Dy() != 2 {
		t.Fatalf("extracted image is %dx%d, want 2x2", b.Dx(), b.Dy())
	}
}

// TestEqualObjectsComparesSourceBackedStreams verifies stream dict comparison for source-backed streams.
func TestEqualObjectsComparesSourceBackedStreams(t *testing.T) {
	plain := bytes.Repeat([]byte("same "), 1<<10)
	same, alsoSame := flateSourceBacked(t, plain, 0), flateSourceBacked(t, plain, 0)
	different := flateSourceBacked(t, bytes.Repeat([]byte("other "), 1<<10), 0)
	xref, err := pdfcpu.CreateXRefTableWithRootDict()
	if err != nil {
		t.Fatal(err)
	}

	equal, err := model.EqualObjects(same, alsoSame, xref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Fatal("streams over the same bytes compare unequal")
	}

	equal, err = model.EqualObjects(same, different, xref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Fatal("streams over different bytes compare equal")
	}
}
