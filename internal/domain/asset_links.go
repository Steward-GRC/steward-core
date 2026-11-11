// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Where a document links to a stored file.
const (
	// LinkLocationPolicy is the document's own content.
	LinkLocationPolicy = "policy"
	// LinkLocationAppendix is one of the document's appendices.
	LinkLocationAppendix = "appendix"
)

// AssetDocLink is one document that holds a stored file's url in a draft or
// published version's content, or in one of its appendices.
type AssetDocLink struct {
	PolicyID uuid.UUID
	Title    string
	Location string
}

// LinkedAsset is a stored file with the documents that link to it. No links
// means the file is unlinked.
type LinkedAsset struct {
	Asset Asset
	Links []AssetDocLink
}

// Linked reports whether any document links to the file.
func (l LinkedAsset) Linked() bool { return len(l.Links) > 0 }

// assetCardTypes are the editor nodes that embed a stored file by its url.
var assetCardTypes = map[string]bool{"file": true, "image": true}

// RemoveAssetFromContent takes every File or image card whose src is assetURL
// out of the editor content (Lexical JSON), wherever it nests. It reports
// whether anything was removed; when nothing was, the input is returned
// unchanged. Numbers are kept exactly as written. Content that is not JSON is
// an error.
func RemoveAssetFromContent(content, assetURL string) (string, bool, error) {
	if !strings.Contains(content, assetURL) {
		// Cheap exit, and it keeps unrelated content byte-for-byte.
		if !json.Valid([]byte(content)) {
			return content, false, fmt.Errorf("asset links: content is not JSON")
		}
		return content, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return content, false, fmt.Errorf("asset links: content is not JSON: %w", err)
	}
	out, changed := stripAssetCards(root, assetURL)
	if !changed {
		return content, false, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return content, false, fmt.Errorf("asset links: encode content: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), true, nil
}

func isAssetCard(v any, assetURL string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	typ, _ := m["type"].(string)
	src, _ := m["src"].(string)
	return assetCardTypes[typ] && src == assetURL
}

func stripAssetCards(v any, assetURL string) (any, bool) {
	switch t := v.(type) {
	case []any:
		out := make([]any, 0, len(t))
		changed := false
		for _, el := range t {
			if isAssetCard(el, assetURL) {
				changed = true
				continue
			}
			n, c := stripAssetCards(el, assetURL)
			changed = changed || c
			out = append(out, n)
		}
		return out, changed
	case map[string]any:
		changed := false
		for k, el := range t {
			n, c := stripAssetCards(el, assetURL)
			if c {
				t[k] = n
				changed = true
			}
		}
		return t, changed
	}
	return v, false
}
