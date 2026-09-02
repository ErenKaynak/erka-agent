package tui

import (
	"strings"
	"unicode/utf8"
)

const maxRenderedInputBytes = 1 << 20

// Sanitize removes terminal escape/control sequences from all untrusted text
// before it is handed to a terminal renderer.
func Sanitize(value string) string {
	if len(value) > maxRenderedInputBytes {
		value = value[:maxRenderedInputBytes]
	}
	var output strings.Builder
	output.Grow(len(value))
	for index := 0; index < len(value); {
		character := value[index]
		if character == 0x1b {
			index++
			if index >= len(value) {
				break
			}
			switch value[index] {
			case '[':
				index++
				for index < len(value) {
					final := value[index]
					index++
					if final >= 0x40 && final <= 0x7e {
						break
					}
				}
			case ']':
				index++
				for index < len(value) {
					if value[index] == 0x07 {
						index++
						break
					}
					if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
						index += 2
						break
					}
					index++
				}
			default:
				index++
			}
			continue
		}
		if character < 0x20 && character != '\n' && character != '\t' {
			index++
			continue
		}
		if character == 0x7f {
			index++
			continue
		}
		decoded, size := utf8.DecodeRuneInString(value[index:])
		if decoded == utf8.RuneError && size == 1 {
			index++
			continue
		}
		output.WriteRune(decoded)
		index += size
	}
	return output.String()
}
