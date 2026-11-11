// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadReviewLimitDefaults(t *testing.T) {
	c, err := Load(env(base()))
	require.NoError(t, err)
	require.Equal(t, 10000, c.ReviewBodyMaxChars)
	require.Equal(t, 4000, c.ReviewQuoteMaxChars)
}

func TestLoadReadsReviewLimits(t *testing.T) {
	m := base()
	m["REVIEW_BODY_MAX_CHARS"] = "500"
	m["REVIEW_QUOTE_MAX_CHARS"] = "200"
	c, err := Load(env(m))
	require.NoError(t, err)
	require.Equal(t, 500, c.ReviewBodyMaxChars)
	require.Equal(t, 200, c.ReviewQuoteMaxChars)
}

func TestLoadRejectsBadReviewLimits(t *testing.T) {
	for name, kv := range map[string][2]string{
		"body not a number":  {"REVIEW_BODY_MAX_CHARS", "lots"},
		"body zero":          {"REVIEW_BODY_MAX_CHARS", "0"},
		"body negative":      {"REVIEW_BODY_MAX_CHARS", "-1"},
		"quote not a number": {"REVIEW_QUOTE_MAX_CHARS", "x"},
		"quote zero":         {"REVIEW_QUOTE_MAX_CHARS", "0"},
	} {
		m := base()
		m[kv[0]] = kv[1]
		_, err := Load(env(m))
		require.Error(t, err, name)
		require.Contains(t, err.Error(), kv[0], name)
	}
}
