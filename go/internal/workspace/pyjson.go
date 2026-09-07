package workspace

import (
	"fmt"
	"strings"
)

// buildStatePayload renders the workspace file body with Python-compatible
// JSON: compact separators, ensure_ascii escaping, and the old field set
// plus spaces. The trailing newline is added by the atomic write.
func buildStatePayload(groupID string, rootID string, spaces []Mount) string {
	var b strings.Builder
	b.WriteString(`{"group_id":"`)
	b.WriteString(pyEscape(groupID))
	b.WriteString(`","root_id":"`)
	b.WriteString(pyEscape(rootID))
	b.WriteString(`"`)
	if len(spaces) > 0 {
		b.WriteString(`,"spaces":[`)
		for i, mount := range spaces {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"group_id":"`)
			b.WriteString(pyEscape(mount.GroupID))
			b.WriteString(`","root_id":"`)
			b.WriteString(pyEscape(mount.RootID))
			b.WriteString(`","name":"`)
			b.WriteString(pyEscape(mount.Name))
			b.WriteString(`"}`)
		}
		b.WriteByte(']')
	}
	b.WriteString("}")
	return b.String()
}

// pyEscape mirrors json.dumps(ensure_ascii=True) for one string: short
// escapes for the C0 classics, \uXXXX for everything outside printable
// ASCII, and surrogate pairs for astral runes.
func pyEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r > 0x7E {
				pyEscapeUnicode(&b, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func pyEscapeUnicode(b *strings.Builder, r rune) {
	writeU := func(v rune) { fmt.Fprintf(b, `\u%04x`, v) }
	if r > 0xFFFF {
		r -= 0x10000
		writeU(0xD800 + (r >> 10))
		writeU(0xDC00 + (r & 0x3FF))
		return
	}
	writeU(r)
}
