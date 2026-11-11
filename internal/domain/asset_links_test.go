// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

const (
	linkA = "/api/assets/11111111-1111-1111-1111-111111111111"
	linkB = "/api/assets/22222222-2222-2222-2222-222222222222"
)

func card(typ, src string) string {
	return `{"type":"` + typ + `","src":"` + src + `","fileName":"a.pdf","fileSize":1234567890123,"children":[],"version":1}`
}

func doc(children ...string) string {
	return `{"root":{"type":"root","version":1,"children":[` + strings.Join(children, ",") + `]}}`
}

const para = `{"type":"paragraph","version":1,"children":[{"type":"text","text":"keep me","version":1}]}`

func TestRemoveAssetFromContent_TakesOutTheFileCard(t *testing.T) {
	in := doc(para, card("file", linkA), card("file", linkB))
	out, changed, err := domain.RemoveAssetFromContent(in, linkA)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want changed", changed, err)
	}
	if strings.Contains(out, linkA) {
		t.Errorf("link still in content: %s", out)
	}
	if !strings.Contains(out, linkB) || !strings.Contains(out, "keep me") {
		t.Errorf("other content was lost: %s", out)
	}
}

func TestRemoveAssetFromContent_TakesOutAnImageCardAndNestedCards(t *testing.T) {
	nested := `{"type":"table","children":[{"type":"tablerow","children":[{"type":"tablecell","children":[` +
		card("image", linkA) + `,` + para + `]}]}]}`
	out, changed, err := domain.RemoveAssetFromContent(doc(nested), linkA)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Contains(out, linkA) || !strings.Contains(out, "keep me") {
		t.Errorf("nested image not removed cleanly: %s", out)
	}
}

func TestRemoveAssetFromContent_NothingToRemoveIsUnchanged(t *testing.T) {
	in := doc(para, card("file", linkB))
	out, changed, err := domain.RemoveAssetFromContent(in, linkA)
	if err != nil || changed || out != in {
		t.Errorf("out=%q changed=%v err=%v, want the input back unchanged", out, changed, err)
	}
}

func TestRemoveAssetFromContent_KeepsLargeNumbersExact(t *testing.T) {
	out, changed, err := domain.RemoveAssetFromContent(doc(card("file", linkA), card("file", linkB)), linkA)
	if err != nil || !changed {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1234567890123") {
		t.Errorf("a number was rewritten: %s", out)
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Errorf("result is not JSON: %v", err)
	}
}

func TestRemoveAssetFromContent_RefusesContentThatIsNotJSON(t *testing.T) {
	if _, _, err := domain.RemoveAssetFromContent("{not json "+linkA, linkA); err == nil {
		t.Error("want an error for content that is not JSON")
	}
}
