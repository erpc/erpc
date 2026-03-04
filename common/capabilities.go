package common

import (
	"regexp"
	"strings"
)

var capabilityTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*$`)

func NormalizeCapabilityTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

func NormalizeCapabilityTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}

	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, raw := range tags {
		tag := NormalizeCapabilityTag(raw)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func IsValidCapabilityTag(tag string) bool {
	return capabilityTagPattern.MatchString(NormalizeCapabilityTag(tag))
}

func HasAllCapabilities(have []string, required []string) bool {
	normalizedRequired := NormalizeCapabilityTags(required)
	if len(normalizedRequired) == 0 {
		return true
	}
	normalizedHave := NormalizeCapabilityTags(have)
	if len(normalizedHave) == 0 {
		return false
	}

	haveSet := make(map[string]struct{}, len(normalizedHave))
	for _, tag := range normalizedHave {
		haveSet[tag] = struct{}{}
	}
	for _, tag := range normalizedRequired {
		if _, ok := haveSet[tag]; !ok {
			return false
		}
	}
	return true
}
