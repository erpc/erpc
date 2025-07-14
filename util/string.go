package util

import (
	"fmt"
	"strconv"
	"strings"
)

func StringPtr(s string) *string {
	return &s
}

func IntPtr(i int) *int {
	return &i
}

func BoolPtr(b bool) *bool {
	return &b
}

func Float64Ptr(f float64) *float64 {
	return &f
}

func ParseByteSize(size string) (int, error) {
	size = strings.TrimSpace(strings.ToUpper(size))

	if size == "" {
		return 0, fmt.Errorf("empty size string")
	}

	var multiplier int = 1

	if strings.HasSuffix(size, "KB") {
		multiplier = 1024
		size = strings.TrimSuffix(size, "KB")
	} else if strings.HasSuffix(size, "MB") {
		multiplier = 1024 * 1024
		size = strings.TrimSuffix(size, "MB")
	} else {
		size = strings.TrimSuffix(size, "B")
	}

	value, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size format: %w", err)
	}

	if value < 0 {
		return 0, fmt.Errorf("size cannot be negative")
	}

	return int(value) * multiplier, nil
}
