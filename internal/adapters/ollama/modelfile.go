package ollama

import "strings"

// templateOnlyModelfile checks the generated /api/show Modelfile, not a user
// configuration file. Ollama 0.18.3's ShowResponse omits Renderer, but Model.String
// includes a RENDERER command. Missing or ambiguous serialization is unsupported.
//
// This intentionally accepts a strict subset of parser.Command.String output:
// line-based commands/comments and raw, double-quoted or triple-quoted values.
// Quotes have no backslash escaping in that version, and quote() can emit a lone
// quote unwrapped. Parsing alone therefore cannot prove command boundaries.
// All fields before RENDERER in Model.String are constrained: FROM/ADAPTER must
// be unquoted single-line paths; TEMPLATE and optional SYSTEM must match their
// independent structured metadata. Enforce their order and uniqueness, and do
// not permit a later command to reintroduce these prefix fields. Thus suffix
// values cannot swallow a real renderer from the audited serialization order.
// See v0.18.3 server/images.go and parser/parser.go; do not replace with a regex
// scan, since template/system/license text can contain apparent directives.
func templateOnlyModelfile(source, template, system string) bool {
	if source == "" || strings.ContainsRune(source, '\x00') {
		return false
	}
	foundFrom, foundTemplate, foundSystem, tail := false, false, false, false
	for source != "" {
		source = strings.TrimLeft(source, " \t\r\n")
		if source == "" {
			break
		}
		if source[0] == '#' {
			_, source = modelfileLine(source)
			continue
		}
		end := strings.IndexAny(source, " \t\r\n")
		if end < 0 || source[end] == '\r' || source[end] == '\n' {
			return false
		}
		command := strings.ToUpper(source[:end])
		source = strings.TrimLeft(source[end:], " \t")
		// A renderer directive is unsupported even if empty or inconsistent with
		// top-level metadata. MESSAGE is likewise incompatible with isolation.
		switch command {
		case "RENDERER", "MESSAGE":
			return false
		case "FROM", "ADAPTER":
			// There is no independent structured path in ShowResponse. Do not
			// infer quoted/multiline path boundaries from this lossy serialization.
			value, remaining := modelfileLine(source)
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\"\t") || foundTemplate || tail {
				return false
			}
			if command == "FROM" {
				if foundFrom {
					return false
				}
				foundFrom = true
			} else if !foundFrom {
				return false
			}
			source = remaining
			continue
		case "TEMPLATE":
			if !foundFrom || foundTemplate || tail {
				return false
			}
		case "SYSTEM":
			if !foundTemplate || foundSystem || tail || system == "" {
				return false
			}
		case "LICENSE", "PARSER", "REQUIRES":
			if !foundTemplate || (system != "" && !foundSystem) {
				return false
			}
			tail = true
		case "PARAMETER":
			if !foundTemplate || (system != "" && !foundSystem) {
				return false
			}
			tail = true
			// Consume the parameter name before parsing its possibly multiline value.
			i := strings.IndexAny(source, " \t\r\n")
			if i <= 0 || source[i] == '\r' || source[i] == '\n' {
				return false
			}
			for _, c := range source[:i] {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
					return false
				}
			}
			source = strings.TrimLeft(source[i:], " \t")
		default:
			return false
		}
		value, remaining, ok := modelfileValue(source)
		if !ok || (value == "" && command != "PARAMETER") {
			return false
		}
		source = remaining
		switch command {
		case "TEMPLATE":
			if value != template {
				return false
			}
			foundTemplate = true
		case "SYSTEM":
			if value != system {
				return false
			}
			foundSystem = true
		}
	}
	return foundFrom && foundTemplate && (system == "" || foundSystem)
}

func modelfileLine(s string) (line, rest string) {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func modelfileValue(s string) (value, rest string, ok bool) {
	if s == "" || s[0] == '\r' || s[0] == '\n' {
		return "", "", false
	}
	delimiter := ""
	if strings.HasPrefix(s, `"""`) {
		delimiter = `"""`
	} else if s[0] == '"' {
		delimiter = `"`
	}
	if delimiter == "" {
		value, rest = modelfileLine(s)
		return strings.TrimSpace(value), rest, true
	}
	s = s[len(delimiter):]
	i := strings.Index(s, delimiter)
	if i < 0 {
		return "", "", false
	}
	value = s[:i]
	line, rest := modelfileLine(s[i+len(delimiter):])
	if strings.Trim(line, " \t") != "" {
		return "", "", false
	}
	return value, rest, true
}
