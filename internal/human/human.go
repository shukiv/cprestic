// Package human formats numbers the way a sentence would say them.
//
// It exists because the same quantities are said in two places — a page
// and a notification — and an operator reading "7685765 bytes" in an
// email after seeing "7.3 MiB" on the page has to work out that they are
// the same number.
package human

import "fmt"

// Bytes says a size in the largest unit that leaves it readable.
func Bytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	size, exponent := float64(value), 0
	for size >= unit && exponent < 4 {
		size /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", size, "KMGT"[exponent-1])
}
