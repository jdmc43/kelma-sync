package ankiimport

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

const maxArchiveExpandedBytes = MaxDatabaseBytes + MaxMediaBytes + MaxPackageBytes

func Read(data []byte) (Collection, error) {
	return ReadContext(context.Background(), data)
}

func ReadContext(ctx context.Context, data []byte) (Collection, error) {
	if len(data) < 4 || len(data) > MaxPackageBytes || data[0] != 'P' || data[1] != 'K' {
		return Collection{}, errors.New("the selected file is not a bounded ZIP package")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Collection{}, errors.New("the selected package is corrupt")
	}
	entries, err := validatedEntries(archive)
	if err != nil {
		return Collection{}, err
	}
	var database []byte
	for _, name := range []string{"collection.anki21b", "collection.anki21", "collection.anki2"} {
		entry := entries[name]
		if entry == nil {
			continue
		}
		database, err = readEntry(entry, MaxDatabaseBytes)
		if err == nil && name == "collection.anki21b" {
			database, err = decompressZstd(database, MaxDatabaseBytes)
		}
		if err != nil {
			return Collection{}, fmt.Errorf("invalid Anki collection: %w", err)
		}
		break
	}
	if len(database) < 16 || string(database[:16]) != "SQLite format 3\x00" {
		return Collection{}, errors.New("the package does not contain an Anki collection")
	}
	collection, err := readSQLite(ctx, database)
	if err != nil {
		return Collection{}, fmt.Errorf("invalid Anki collection: %w", err)
	}
	media, warnings, err := readMedia(entries)
	if err != nil {
		return Collection{}, fmt.Errorf("invalid Anki media: %w", err)
	}
	collection.Media = media
	collection.Warnings = append(collection.Warnings, warnings...)
	return collection, nil
}

func validatedEntries(archive *zip.Reader) (map[string]*zip.File, error) {
	if len(archive.File) == 0 || len(archive.File) > MaxArchiveEntries {
		return nil, errors.New("the package has an invalid number of ZIP entries")
	}
	entries := make(map[string]*zip.File, len(archive.File))
	var expanded uint64
	for _, entry := range archive.File {
		name := entry.Name
		if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") ||
			strings.HasPrefix(name, "/") || path.Clean(name) != name || strings.HasPrefix(name, "../") {
			return nil, errors.New("the package contains an unsafe ZIP path")
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, errors.New("the package contains duplicate ZIP paths")
		}
		if entry.UncompressedSize64 > uint64(maxArchiveExpandedBytes)-expanded {
			return nil, errors.New("the package expands beyond the allowed size")
		}
		expanded += entry.UncompressedSize64
		entries[name] = entry
	}
	return entries, nil
}

func readEntry(entry *zip.File, maximum int) ([]byte, error) {
	if entry.UncompressedSize64 > uint64(maximum) {
		return nil, errors.New("ZIP entry exceeds the allowed size")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximum {
		return nil, errors.New("ZIP entry exceeds the allowed size")
	}
	return data, nil
}

func decompressZstd(data []byte, maximum int) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maximum)+(8<<20)))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	decoded, err := io.ReadAll(io.LimitReader(decoder, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > maximum {
		return nil, errors.New("Zstandard payload exceeds the allowed size")
	}
	return decoded, nil
}

func isZstd(data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], []byte{0x28, 0xb5, 0x2f, 0xfd})
}

type mediaManifestEntry struct {
	name  string
	size  int64
	sha1  []byte
	index int
}

func readMedia(entries map[string]*zip.File) ([]Media, []string, error) {
	manifestFile := entries["media"]
	if manifestFile == nil {
		return nil, nil, nil
	}
	manifest, err := readEntry(manifestFile, 8<<20)
	if err != nil {
		return nil, nil, err
	}
	if isZstd(manifest) {
		manifest, err = decompressZstd(manifest, 8<<20)
		if err != nil {
			return nil, nil, err
		}
	}
	items, err := decodeMediaManifest(manifest)
	if err != nil {
		return nil, nil, err
	}
	if len(items) > MaxArchiveEntries {
		return nil, nil, errors.New("media manifest exceeds the web import limit")
	}
	out := make([]Media, 0, len(items))
	warnings := make([]string, 0)
	seen := map[string]bool{}
	total := 0
	for _, item := range items {
		filename, err := normalizeMediaFilename(item.name)
		if err != nil {
			return nil, nil, err
		}
		if seen[item.name] {
			warnings = append(warnings, "A duplicate package media name was skipped.")
			continue
		}
		seen[item.name] = true
		entry := entries[strconv.Itoa(item.index)]
		if entry == nil {
			warnings = append(warnings, "A package media entry was missing and was skipped.")
			continue
		}
		data, err := readEntry(entry, MaxMediaFileBytes+(1<<20))
		if err != nil {
			return nil, nil, err
		}
		if isZstd(data) {
			data, err = decompressZstd(data, MaxMediaFileBytes)
			if err != nil {
				return nil, nil, err
			}
		}
		if len(data) > MaxMediaFileBytes || (item.size > 0 && item.size != int64(len(data))) {
			return nil, nil, errors.New("media size check failed")
		}
		digest := sha1.Sum(data)
		if len(item.sha1) > 0 && !bytes.Equal(item.sha1, digest[:]) {
			return nil, nil, errors.New("media checksum check failed")
		}
		total += len(data)
		if total > MaxMediaBytes {
			return nil, nil, errors.New("package media exceeds the allowed size")
		}
		out = append(out, Media{SourceName: item.name, Filename: filename, Bytes: data})
	}
	return out, warnings, nil
}

func decodeMediaManifest(data []byte) ([]mediaManifestEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var values map[string]string
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, err
		}
		out := make([]mediaManifestEntry, 0, len(values))
		for rawIndex, name := range values {
			index, err := strconv.Atoi(rawIndex)
			if err != nil || index < 0 {
				continue
			}
			out = append(out, mediaManifestEntry{name: name, index: index})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
		return out, nil
	}
	fields, err := decodeProto(data)
	if err != nil {
		return nil, err
	}
	out := make([]mediaManifestEntry, 0)
	for _, outer := range fields {
		if outer.number != 1 || outer.wire != 2 {
			continue
		}
		inner, err := decodeProto(outer.bytes)
		if err != nil {
			return nil, err
		}
		name, err := fieldString(inner, 1)
		if err != nil {
			return nil, err
		}
		size, _ := fieldInt(inner, 2)
		index := len(out)
		if legacy, ok := fieldInt(inner, 255); ok && legacy >= 0 && legacy <= int64(MaxArchiveEntries) {
			index = int(legacy)
		}
		out = append(out, mediaManifestEntry{name: name, size: size, sha1: fieldBytes(inner, 3), index: index})
	}
	return out, nil
}

func normalizeMediaFilename(value string) (string, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return "", errors.New("media filename is empty")
	}
	name := strings.TrimSpace(parts[len(parts)-1])
	if name == "" || name == "." || name == ".." || len([]byte(name)) > 255 || strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		return "", errors.New("media filename is invalid")
	}
	return name, nil
}
