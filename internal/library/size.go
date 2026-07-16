package library

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// ParseFileSize parses a human-readable file size into bytes.
// Empty or whitespace-only input returns (0, nil) meaning "unset / unlimited".
//
// Accepted forms (case-insensitive, optional space before unit):
//
//	123          → 123 bytes
//	300KB, 300K  → 300 * 1024
//	300MB, 300M  → 300 * 1024^2
//	10GB, 10G    → 10 * 1024^3
//	1TB, 1T      → 1 * 1024^4
//	1.5GB        → fractional values allowed
//
// Units use binary (1024) multipliers.
func ParseFileSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Split trailing unit letters from the numeric prefix.
	i := len(s)
	for i > 0 {
		r := rune(s[i-1])
		if unicode.IsLetter(r) {
			i--
			continue
		}
		break
	}
	numStr := strings.TrimSpace(s[:i])
	unit := strings.ToUpper(strings.TrimSpace(s[i:]))

	if numStr == "" {
		return 0, fmt.Errorf("invalid file size %q: missing number", s)
	}

	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid file size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid file size %q: must be non-negative", s)
	}

	var mult float64 = 1
	switch unit {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1024
	case "M", "MB", "MIB":
		mult = 1024 * 1024
	case "G", "GB", "GIB":
		mult = 1024 * 1024 * 1024
	case "T", "TB", "TIB":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("invalid file size %q: unknown unit %q (use B, KB, MB, GB, TB)", s, unit)
	}

	bytes := n * mult
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("invalid file size %q: value too large", s)
	}
	return int64(bytes), nil
}
