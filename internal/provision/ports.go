package provision

import (
	"strconv"
	"strings"
)

// firstPort reads "63501", "1433,1434" or "" (none; 0 too means none).
func firstPort(s string) int {
	first, _, _ := strings.Cut(s, ",")
	n, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}
