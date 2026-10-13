package task

// TextLength returns the number of UTF-16 code units in text, the unit used by
// the extension schema for character limits.
func TextLength(text string) int {
	length := 0
	for _, r := range text {
		if r > 0xFFFF {
			length += 2
		} else {
			length++
		}
	}
	return length
}
