package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"taskworker.local/taskworker/internal/core"
)

const MaxBody = 8 << 20
const MaxResponse = 256 << 20
const MaxEvent = 16 << 20

func invalid() error {
	return &core.Fault{Code: core.ErrInvalidRequest, Message: "invalid JSON command"}
}

// DecodeCommand accepts exact JSON field names, no duplicates, nulls, unknown
// fields, invalid Unicode or trailing values. Optional values must be omitted.
func DecodeCommand(b []byte, dst any) error {
	if len(b) > MaxBody {
		return &core.Fault{Code: core.ErrLimitExceeded, Message: "command exceeds body limit"}
	}
	if !utf8.Valid(b) || !json.Valid(b) || !validEscapes(b) {
		return invalid()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := shape(d, reflect.TypeOf(dst).Elem(), 0); err != nil {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return invalid()
	}
	return nil
}
func shape(d *json.Decoder, t reflect.Type, depth int) error {
	if depth > 16 {
		return invalid()
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := d.Token()
	if err != nil || tok == nil {
		return invalid()
	}
	if t.Kind() == reflect.Struct {
		if tok != json.Delim('{') {
			return invalid()
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return invalid()
			}
			key, ok := k.(string)
			ft, exists := fields[key]
			if !ok || !exists || seen[key] {
				return invalid()
			}
			seen[key] = true
			if e = shape(d, ft, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return invalid()
		}
		return nil
	}
	if _, ok := tok.(json.Delim); ok {
		return invalid()
	}
	return nil
}

// encoding/json replaces unpaired UTF-16 escapes; reject them before decoding.
func validEscapes(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		i++
		for i < len(b) && b[i] != '"' {
			if b[i] == '\\' {
				i++
				if b[i] == 'u' {
					n, _ := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
					i += 4
					if n >= 0xd800 && n <= 0xdbff {
						if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
							return false
						}
						m, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
						if e != nil || m < 0xdc00 || m > 0xdfff {
							return false
						}
						i += 6
					} else if n >= 0xdc00 && n <= 0xdfff {
						return false
					}
				}
			}
			i++
		}
	}
	return true
}

func CursorID(c core.Cursor) string { return c.StoreID + ":" + strconv.FormatUint(c.Sequence, 10) }
func ParseCursor(s string) (core.Cursor, error) {
	p := strings.Split(s, ":")
	bad := func() (core.Cursor, error) {
		return core.Cursor{}, &core.Fault{Code: core.ErrCursorInvalid, Message: "invalid event cursor"}
	}
	if len(p) != 2 || len(p[0]) != 32 || p[1] == "" {
		return bad()
	}
	for _, c := range p[0] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return bad()
		}
	}
	n, e := strconv.ParseUint(p[1], 10, 64)
	if e != nil || strconv.FormatUint(n, 10) != p[1] {
		return bad()
	}
	return core.Cursor{StoreID: p[0], Sequence: n}, nil
}
