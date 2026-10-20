// SPDX-License-Identifier: CC-BY-4.0
// Copyright (c) 2026 lilongsky

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

const redactedValue = "[REDACTED]"

var (
	encodedBytePattern  = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
	authValuePattern    = regexp.MustCompile(`(?i)\b((?:bearer|basic)[ \t]+)[A-Za-z0-9._~+/%=-]+`)
	secretFieldPattern  = regexp.MustCompile(`(?i)((?:"|')?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|token|api[_-]?key|client[_-]?(?:key|secret)|consumer[_-]?(?:key|secret)|authorization)(?:"|')?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\s,;}\]<]+)`)
	escapedFieldPattern = regexp.MustCompile(`(?i)((?:\\)+"(?:access[_-]?token|refresh[_-]?token|id[_-]?token|token|api[_-]?key|client[_-]?(?:key|secret)|consumer[_-]?(?:key|secret)|authorization)(?:\\)+"\s*:\s*(?:\\)+")[^"\\]*((?:\\)+")`)
	xmlSecretPattern    = regexp.MustCompile(`(?is)(<(?:[[:alnum:]_-]+:)?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|token|api[_-]?key|client[_-]?(?:key|secret)|consumer[_-]?(?:key|secret)|authorization)(?:\s[^>]*)?>)[^<]*(</(?:[[:alnum:]_-]+:)?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|token|api[_-]?key|client[_-]?(?:key|secret)|consumer[_-]?(?:key|secret)|authorization)\s*>)`)
)

// Credential variants stay in memory. This also covers credentials echoed by an
// upstream service in a Basic value, a URL, or an escaped JSON error message.
func redactKnownCredentials(text string, additional ...string) string {
	key := os.Getenv("EPO_CLIENT_KEY")
	secret := os.Getenv("EPO_CLIENT_SECRET")
	values := append([]string{key, secret}, additional...)
	if key != "" && secret != "" {
		values = append(values, key+":"+secret)
	}
	variants := make(map[string]struct{})
	add := func(value string) {
		if value != "" {
			variants[value] = struct{}{}
		}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		forms := []string{
			value,
			base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value)),
			base64.URLEncoding.EncodeToString([]byte(value)),
			base64.RawURLEncoding.EncodeToString([]byte(value)),
		}
		for _, form := range forms {
			add(form)
			for _, escaped := range []string{url.QueryEscape(form), url.PathEscape(form)} {
				add(escaped)
				add(encodedBytePattern.ReplaceAllStringFunc(escaped, strings.ToLower))
			}
			encoded, _ := json.Marshal(form)
			add(string(encoded[1 : len(encoded)-1]))
		}
	}
	ordered := make([]string, 0, len(variants))
	for value := range variants {
		ordered = append(ordered, value)
	}
	// Replace combined or encoded credentials before their shorter components.
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	replacements := make([]string, 0, len(ordered)*2)
	for _, value := range ordered {
		replacements = append(replacements, value, redactedValue)
	}
	return strings.NewReplacer(replacements...).Replace(text)
}

// Generic token masking is restricted to errors and fault diagnostics, leaving
// successful patent text unchanged apart from exact configured credentials.
func redactErrorText(text string, additional ...string) string {
	text = redactKnownCredentials(text, additional...)
	text = authValuePattern.ReplaceAllString(text, "${1}"+redactedValue)
	text = secretFieldPattern.ReplaceAllString(text, "${1}\""+redactedValue+"\"")
	text = escapedFieldPattern.ReplaceAllString(text, "${1}"+redactedValue+"${2}")
	return xmlSecretPattern.ReplaceAllString(text, "${1}"+redactedValue+"${2}")
}
