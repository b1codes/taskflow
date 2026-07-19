package session

import (
	"regexp"
	"strings"
)

var (
	pathRegex      = regexp.MustCompile(`[^\s:]+\.(go|js|py|ts|rs|java|rb|c|cpp|h)`)
	lineRegex      = regexp.MustCompile(`:\d+`)
	uuidRegex      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	timestampRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	spaceRegex     = regexp.MustCompile(`\s+`)
)

func NormalizeErrorSignature(errStr string) string {
	if errStr == "" {
		return ""
	}

	// 1. Strip ISO timestamps
	res := timestampRegex.ReplaceAllString(errStr, "<timestamp>")

	// 2. Strip UUIDs
	res = uuidRegex.ReplaceAllString(res, "<uuid>")

	// 3. Strip absolute file paths
	res = pathRegex.ReplaceAllString(res, "<file>")

	// 4. Strip line numbers
	res = lineRegex.ReplaceAllString(res, ":<line>")

	// 5. Lowercase
	res = strings.ToLower(res)

	// 6. Collapse multiple whitespaces and trim
	res = spaceRegex.ReplaceAllString(res, " ")
	res = strings.TrimSpace(res)

	return res
}
