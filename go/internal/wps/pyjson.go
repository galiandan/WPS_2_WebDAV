// Ordered JSON handling for request bodies. Python's json module preserves
// object key order and dumps with ensure_ascii plus compact separators, so
// a 401 retry that rewrites only the CSRF field keeps every other wire byte
// identical. Go's map type loses order, so bodies round-trip through a
// small ordered document.

package wps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// pyObject is a JSON object with preserved key order. Values are string,
// json.Number, bool, nil, *pyObject, or []any.
type pyObject struct {
	keys   []string
	values map[string]any
}

// decodePYValue decodes one JSON document preserving object order. Numbers
// stay raw via json.Number so re-serialization is byte-faithful.
func decodePYValue(data []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, ok := decodePYToken(decoder)
	if !ok {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return value, true
}

func decodePYToken(decoder *json.Decoder) (any, bool) {
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delim, isDelim := token.(json.Delim); isDelim {
		switch delim {
		case '{':
			object := &pyObject{values: map[string]any{}}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, false
				}
				key, isString := keyToken.(string)
				if !isString {
					return nil, false
				}
				value, ok := decodePYToken(decoder)
				if !ok {
					return nil, false
				}
				if _, exists := object.values[key]; !exists {
					object.keys = append(object.keys, key)
				}
				object.values[key] = value
			}
			if _, err := decoder.Token(); err != nil {
				return nil, false
			}
			return object, true
		case '[':
			items := []any{}
			for decoder.More() {
				item, ok := decodePYToken(decoder)
				if !ok {
					return nil, false
				}
				items = append(items, item)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, false
			}
			return items, true
		default:
			return nil, false
		}
	}
	return token, true
}

// dumpPYValue renders like json.dumps(payload, ensure_ascii=True,
// separators=(",", ":")).
func dumpPYValue(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writePYValue(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writePYValue(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		buffer.WriteString(pyQuote(typed))
	case json.Number:
		buffer.WriteString(typed.String())
	case *pyObject:
		buffer.WriteByte('{')
		for index, key := range typed.keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			buffer.WriteString(pyQuote(key))
			buffer.WriteByte(':')
			if err := writePYValue(buffer, typed.values[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writePYValue(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	default:
		return fmt.Errorf("unsupported body value type %T", value)
	}
	return nil
}

// pyQuote mirrors workspace.pyEscape and json.dumps(ensure_ascii=True) for
// one string: short escapes for the C0 classics, lowercase \uXXXX for
// everything outside printable ASCII, surrogate pairs for astral runes.
func pyQuote(value string) string {
	var buffer strings.Builder
	buffer.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if char < 0x20 || char > 0x7E {
				pyQuoteUnicode(&buffer, char)
			} else {
				buffer.WriteRune(char)
			}
		}
	}
	buffer.WriteByte('"')
	return buffer.String()
}

func pyQuoteUnicode(buffer *strings.Builder, char rune) {
	writeU := func(v rune) { fmt.Fprintf(buffer, `\u%04x`, v) }
	if char > 0xFFFF {
		char -= 0x10000
		writeU(0xD800 + (char >> 10))
		writeU(0xDC00 + (char & 0x3FF))
		return
	}
	writeU(char)
}

// refreshJSONBody mirrors _refresh_json_body: only a JSON object body that
// already carries a string csrfmiddlewaretoken field is rewritten; anything
// else returns the original body untouched.
func refreshJSONBody(body []byte, csrfToken string) []byte {
	if len(body) == 0 || csrfToken == "" {
		return body
	}
	decoded, ok := decodePYValue(body)
	if !ok {
		return body
	}
	object, ok := decoded.(*pyObject)
	if !ok {
		return body
	}
	current, exists := object.values["csrfmiddlewaretoken"]
	if !exists {
		return body
	}
	if _, isString := current.(string); !isString {
		return body
	}
	object.values["csrfmiddlewaretoken"] = csrfToken
	dumped, err := dumpPYValue(object)
	if err != nil {
		return body
	}
	return dumped
}
