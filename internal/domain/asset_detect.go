// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

func sortStrings(s []string) { sort.Strings(s) }

// neverExtensions are refused by name whatever the bytes are and whatever the
// admin setting says: anything a browser would run or render, executables and
// installers, macro-enabled Office files and archives (whose contents cannot
// be checked).
var neverExtensions = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`exe msi dmg app dll jar com bat cmd sh ps1 vbs scr pif
		html htm xhtml svg xml js mjs css
		docm xlsm pptm dotm xltm potm
		zip rar 7z tar gz tgz bz2 xz`) {
		m[e] = true
	}
	return m
}()

// Detected is what the bytes of an upload are.
type Detected struct {
	Kind        AssetKind
	ContentType string
}

// IsImage reports whether the asset is served inline.
func (d Detected) IsImage() bool { return d.Kind == KindImage }

// found is the internal result of looking at the bytes.
type found struct {
	kind AssetKind
	// contentType, when set, is final; otherwise byExt maps the name's
	// extension to the stored type (the formats whose bytes do not say).
	contentType string
	byExt       map[string]string
	extensions  []string
	// never is the plain-words reason a kind is refused outright.
	never string
	// fromText marks plain text that the name may call txt, md or csv.
	fromText bool
}

const (
	typeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	typeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	typePptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

var oleTypes = map[string]string{
	"doc": "application/msword",
	"xls": "application/vnd.ms-excel",
	"ppt": "application/vnd.ms-powerpoint",
}

var oleKinds = map[string]AssetKind{"doc": KindWord, "xls": KindOffice, "ppt": KindOffice}

var odfTypes = map[string]string{
	"text":         "application/vnd.oasis.opendocument.text",
	"spreadsheet":  "application/vnd.oasis.opendocument.spreadsheet",
	"presentation": "application/vnd.oasis.opendocument.presentation",
}
var odfExt = map[string]string{"text": "odt", "spreadsheet": "ods", "presentation": "odp"}

// DetectAsset works out what an upload really is from its bytes, never from
// the name or the declared type, then checks the name and the declared type
// against it and the admin setting (allowed) against its kind. The returned
// content type is the one to store. Only the first failing rule is reported.
func DetectAsset(data []byte, filename, declared string, allowed []AssetKind) (Detected, error) {
	if len(data) == 0 {
		return Detected{}, ErrAssetEmpty
	}
	name := SanitizeFilename(filename)
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))

	if neverExtensions[ext] {
		return Detected{}, fmt.Errorf("%w: files of this kind are not allowed (.%s)", ErrAssetUnsupportedType, ext)
	}
	f := inspect(data)
	if f.never != "" {
		return Detected{}, fmt.Errorf("%w: files of this kind are not allowed (%s)", ErrAssetUnsupportedType, f.never)
	}

	ct, kind := f.contentType, f.kind
	switch {
	case f.fromText:
		switch ext {
		case "", "txt":
			kind, ct = KindText, "text/plain; charset=utf-8"
		case "md":
			kind, ct = KindMarkdown, "text/markdown; charset=utf-8"
		case "csv":
			kind, ct = KindOffice, "text/csv; charset=utf-8"
		default:
			return Detected{}, mismatch(name, "text")
		}
	case f.byExt != nil:
		t, ok := f.byExt[ext]
		if !ok {
			return Detected{}, mismatch(name, strings.Join(sortedKeys(f.byExt), "/")+" document")
		}
		ct = t
		if k, ok := oleKinds[ext]; ok && f.contentType == "" {
			kind = k
		}
	default:
		if ext != "" && !contains(f.extensions, ext) {
			return Detected{}, mismatch(name, ct)
		}
	}
	if !declaredAgrees(declared, kind, f.fromText) {
		return Detected{}, fmt.Errorf("%w: the contents of %q do not match the declared type %q (found %s)",
			ErrAssetMismatch, name, strings.TrimSpace(declared), ct)
	}
	if !allowedKind(allowed, kind) {
		return Detected{}, fmt.Errorf("%w: %s files are turned off (allowed: %s)", ErrAssetKindDisabled, KindLabel(kind), labelsOf(allowed))
	}
	return Detected{Kind: kind, ContentType: ct}, nil
}

func mismatch(name, found string) error {
	return fmt.Errorf("%w: the contents of %q do not match its name (found %s)", ErrAssetMismatch, name, found)
}

func allowedKind(allowed []AssetKind, k AssetKind) bool {
	for _, a := range allowed {
		if a == k {
			return true
		}
	}
	return false
}

func labelsOf(kinds []AssetKind) string {
	if len(kinds) == 0 {
		return "none"
	}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = KindLabel(k)
	}
	return strings.Join(out, ", ")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// declaredAgrees compares the client's content type to the detected kind. A
// missing, generic or unrecognised declared type makes no claim.
func declaredAgrees(declared string, kind AssetKind, fromText bool) bool {
	base := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch base {
	case "", "application/octet-stream", "binary/octet-stream":
		return true
	}
	var claim AssetKind
	switch {
	case strings.HasPrefix(base, "image/"):
		claim = KindImage
	case strings.HasPrefix(base, "video/"):
		claim = KindVideo
	case base == "application/pdf":
		claim = KindPDF
	case base == "application/msword", base == typeDocx:
		claim = KindWord
	case base == "text/plain", base == "text/markdown", base == "text/x-markdown":
		// Browsers label .md and .csv as plain text; the name decides which.
		return kind == KindText || kind == KindMarkdown || fromText
	case base == "text/csv", base == "application/vnd.ms-excel" && fromText:
		return fromText
	case base == "application/vnd.ms-excel", base == "application/vnd.ms-powerpoint", base == typeXlsx, base == typePptx,
		base == "application/rtf", base == "text/rtf", strings.HasPrefix(base, "application/vnd.oasis.opendocument."):
		claim = KindOffice
	default:
		return true
	}
	return claim == kind
}

// inspect looks at the bytes alone.
func inspect(data []byte) found {
	switch {
	case bytes.HasPrefix(data, []byte("MZ")), bytes.HasPrefix(data, []byte("\x7fELF")), bytes.HasPrefix(data, []byte("#!")):
		return found{never: "an executable or script"}
	case hasAnyPrefix(data, "\xca\xfe\xba\xbe", "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe"):
		return found{never: "an executable"}
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return found{kind: KindPDF, contentType: "application/pdf", extensions: []string{"pdf"}}
	case bytes.HasPrefix(data, []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")):
		if hasVBA(data) {
			return found{never: "a file with macros"}
		}
		return found{kind: KindWord, byExt: oleTypes}
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return inspectZip(data)
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return inspectBMFF(string(data[8:12]))
	case bytes.HasPrefix(data, []byte("\x1aE\xdf\xa3")):
		if bytes.Contains(data[:min(len(data), 64)], []byte("matroska")) {
			return found{never: "a Matroska video"}
		}
		return found{kind: KindVideo, contentType: "video/webm", extensions: []string{"webm"}}
	case bytes.HasPrefix(data, []byte(`{\rtf`)):
		return found{kind: KindOffice, contentType: "application/rtf", extensions: []string{"rtf"}}
	}
	if ct := sniffImage(data); ct != "" {
		exts := map[string][]string{
			"image/png": {"png"}, "image/jpeg": {"jpg", "jpeg"}, "image/gif": {"gif"}, "image/webp": {"webp"},
		}[ct]
		return found{kind: KindImage, contentType: ct, extensions: exts}
	}
	return inspectText(data)
}

func hasAnyPrefix(data []byte, prefixes ...string) bool {
	for _, p := range prefixes {
		if bytes.HasPrefix(data, []byte(p)) {
			return true
		}
	}
	return false
}

// sniffImage returns the type of the four raster images that are accepted.
// SVG is never one of them: served inline it is a stored-XSS vector.
func sniffImage(data []byte) string {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct
	}
	return ""
}

// hasVBA looks for the VBA project storage of a legacy Office file.
func hasVBA(data []byte) bool {
	const marker = "_VBA_PROJECT"
	if bytes.Contains(data, []byte(marker)) {
		return true
	}
	wide := make([]byte, 0, 2*len(marker))
	for i := 0; i < len(marker); i++ {
		wide = append(wide, marker[i], 0)
	}
	return bytes.Contains(data, wide)
}

func inspectZip(data []byte) found {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return found{never: "an unreadable archive"}
	}
	var hasTypes, word, xl, ppt bool
	var odf string
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		switch {
		case strings.HasSuffix(n, "vbaproject.bin"), strings.HasSuffix(n, "vbadata.xml"):
			return found{never: "a file with macros"}
		case n == "[content_types].xml":
			hasTypes = true
			if body := readEntry(f, 64<<10); bytes.Contains(bytes.ToLower(body), []byte("macroenabled")) {
				return found{never: "a file with macros"}
			}
		case n == "mimetype":
			const prefix = "application/vnd.oasis.opendocument."
			if body := string(readEntry(f, 128)); strings.HasPrefix(body, prefix) {
				odf = strings.TrimPrefix(body, prefix)
			}
		case strings.HasPrefix(n, "word/"):
			word = true
		case strings.HasPrefix(n, "xl/"):
			xl = true
		case strings.HasPrefix(n, "ppt/"):
			ppt = true
		}
	}
	if t, ok := odfTypes[odf]; ok {
		return found{kind: KindOffice, contentType: t, extensions: []string{odfExt[odf]}}
	}
	switch {
	case hasTypes && word:
		return found{kind: KindWord, contentType: typeDocx, extensions: []string{"docx"}}
	case hasTypes && xl:
		return found{kind: KindOffice, contentType: typeXlsx, extensions: []string{"xlsx"}}
	case hasTypes && ppt:
		return found{kind: KindOffice, contentType: typePptx, extensions: []string{"pptx"}}
	}
	return found{never: "an archive"}
}

func readEntry(f *zip.File, limit int64) []byte {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(io.LimitReader(rc, limit))
	return b
}

// inspectBMFF tells the ISO base media files apart: QuickTime, MP4 and the
// brands that are not video.
func inspectBMFF(brand string) found {
	switch {
	case brand == "qt  ":
		return found{kind: KindVideo, contentType: "video/quicktime", extensions: []string{"mov"}}
	case strings.HasPrefix(brand, "heic"), strings.HasPrefix(brand, "heix"), strings.HasPrefix(brand, "mif1"),
		strings.HasPrefix(brand, "msf1"), strings.HasPrefix(brand, "avif"), strings.HasPrefix(brand, "avis"),
		strings.HasPrefix(brand, "M4A"), strings.HasPrefix(brand, "M4B"), strings.HasPrefix(brand, "3g2"):
		return found{never: "an image or audio container"}
	}
	return found{kind: KindVideo, contentType: "video/mp4", extensions: []string{"mp4", "m4v"}}
}

// markup are the strings that make plain text something a browser would run
// or render, checked case-insensitively anywhere in the text.
var markup = []string{"<script", "<svg", "<html", "<iframe", "<object", "<embed", "<body", "<!doctype", "<?xml", "<?php", "javascript:"}

// inspectText accepts only valid UTF-8 without NUL or stray control bytes that
// does not look like markup or script.
func inspectText(data []byte) found {
	if !utf8.Valid(data) {
		return found{never: "an unknown binary file"}
	}
	for _, b := range data {
		if b == 0 || (b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f') {
			return found{never: "an unknown binary file"}
		}
	}
	lower := bytes.ToLower(data)
	head := bytes.TrimLeft(bytes.TrimPrefix(lower, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(head) > 1 && head[0] == '<' && (head[1] == '!' || head[1] == '?' || head[1] >= 'a' && head[1] <= 'z') {
		return found{never: "a web page, script or markup"}
	}
	for _, m := range markup {
		if bytes.Contains(lower, []byte(m)) {
			return found{never: "a web page, script or markup"}
		}
	}
	return found{kind: KindText, fromText: true}
}
