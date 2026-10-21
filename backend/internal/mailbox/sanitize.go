package mailbox

import (
	"html"
	"strconv"
	"strings"
	"unicode"
)

// rawTextDropTags are dangerous elements whose contents are raw text up to the
// matching closing tag (e.g. <script>if (a < b) ...</script>).
var rawTextDropTags = map[string]bool{
	"script":    true,
	"style":     true,
	"textarea":  true,
	"title":     true,
	"xmp":       true,
	"plaintext": true,
}

// subtreeDropTags are dangerous container elements whose entire subtree
// (opening tag through matching closing tag) must be stripped.
var subtreeDropTags = map[string]bool{
	"iframe":   true,
	"object":   true,
	"applet":   true,
	"form":     true,
	"svg":      true,
	"math":     true,
	"frameset": true,
	"template": true,
	"noscript": true,
}

// voidDropTags are dangerous standalone/void elements that are stripped without
// swallowing subsequent sibling elements when unclosed.
var voidDropTags = map[string]bool{
	"embed": true,
	"base":  true,
	"meta":  true,
	"link":  true,
	"frame": true,
}

// urlAttrs are attributes whose values are interpreted as URLs or navigations.
var urlAttrs = map[string]bool{
	"href":       true,
	"src":        true,
	"action":     true,
	"xlink:href": true,
	"formaction": true,
	"background": true,
	"poster":     true,
	"cite":       true,
	"data":       true,
}

// allowedSchemes are the URI schemes permitted in URL attributes. Relative URLs
// (no scheme before '/', '?' or '#') are also permitted.
var allowedSchemes = map[string]bool{
	"http":   true,
	"https":  true,
	"mailto": true,
	"tel":    true,
	"cid":    true,
}

// SanitizeHTML strips executable and style-injection vectors from an inbound or
// outbound HTML email body while preserving safe formatting markup.
func SanitizeHTML(input string) string {
	if input == "" {
		return ""
	}

	var out strings.Builder
	out.Grow(len(input))

	var skipStack []string
	n := len(input)
	i := 0

	for i < n {
		lt := strings.IndexByte(input[i:], '<')
		if lt < 0 {
			if len(skipStack) == 0 {
				out.WriteString(input[i:])
			}
			break
		}
		if len(skipStack) == 0 && lt > 0 {
			out.WriteString(input[i : i+lt])
		}
		i += lt // input[i] == '<'

		if i+1 >= n {
			if len(skipStack) == 0 {
				out.WriteString("&lt;")
			}
			i++
			break
		}

		// Comments, declarations (<!DOCTYPE ...>, <![CDATA[...]]>) and
		// processing instructions (<?...?>) are stripped outright.
		next := input[i+1]
		if next == '!' {
			if strings.HasPrefix(input[i:], "<!--") {
				end := strings.Index(input[i+4:], "-->")
				if end < 0 {
					break
				}
				i += 4 + end + 3
				continue
			}
			end := strings.IndexByte(input[i+2:], '>')
			if end < 0 {
				break
			}
			i += 2 + end + 1
			continue
		}
		if next == '?' {
			end := strings.IndexByte(input[i+2:], '>')
			if end < 0 {
				break
			}
			i += 2 + end + 1
			continue
		}

		// Parse opening or closing tag.
		closing := false
		pos := i + 1
		if pos < n && input[pos] == '/' {
			closing = true
			pos++
		}

		nameStart := pos
		for pos < n && isTagNameChar(input[pos], pos == nameStart) {
			pos++
		}
		if pos == nameStart {
			// Not a valid tag start; escape the '<' if not skipping.
			if len(skipStack) == 0 {
				out.WriteString("&lt;")
			}
			i++
			continue
		}
		tagName := strings.ToLower(input[nameStart:pos])

		// Scan to the end of the tag, respecting quoted attribute values.
		tagEnd, attrs, selfClosing, ok := scanTagRest(input, pos)
		if !ok {
			// Unclosed '<tag...' at EOF: drop it rather than emitting raw '<'.
			break
		}
		i = tagEnd

		if closing {
			if len(skipStack) > 0 {
				// Pop the most recent matching tag on the skip stack.
				for idx := len(skipStack) - 1; idx >= 0; idx-- {
					if skipStack[idx] == tagName {
						skipStack = skipStack[:idx]
						break
					}
				}
				continue
			}
			if rawTextDropTags[tagName] || subtreeDropTags[tagName] || voidDropTags[tagName] {
				continue
			}
			out.WriteString("</")
			out.WriteString(tagName)
			out.WriteByte('>')
			continue
		}

		// Opening tag while inside a skipped subtree.
		if len(skipStack) > 0 {
			if rawTextDropTags[tagName] && !selfClosing {
				i = skipRawTextElement(input, i, tagName)
				continue
			}
			if subtreeDropTags[tagName] && !selfClosing {
				skipStack = append(skipStack, tagName)
			}
			continue
		}

		// Dangerous raw-text elements (<script>, <style>, etc.).
		if rawTextDropTags[tagName] {
			if !selfClosing {
				i = skipRawTextElement(input, i, tagName)
			}
			continue
		}

		// Dangerous container elements (<iframe>, <svg>, <math>, <object>, <form>, etc.).
		if subtreeDropTags[tagName] {
			if !selfClosing {
				skipStack = append(skipStack, tagName)
			}
			continue
		}

		// Dangerous void/standalone elements (<meta>, <link>, <base>, <embed>, <frame>).
		if voidDropTags[tagName] {
			continue
		}

		// Safe element: sanitize attributes and emit.
		out.WriteByte('<')
		out.WriteString(tagName)
		for _, a := range attrs {
			name := strings.ToLower(a.name)
			if !isValidAttrName(name) {
				continue
			}
			if strings.HasPrefix(name, "on") || name == "style" || name == "srcdoc" || name == "xmlns" {
				continue
			}
			if urlAttrs[name] {
				if isUnsafeURL(a.value) {
					continue
				}
			}
			out.WriteByte(' ')
			out.WriteString(name)
			if a.hasValue {
				out.WriteString(`="`)
				out.WriteString(html.EscapeString(decodeHTMLReferences(a.value)))
				out.WriteByte('"')
			}
		}
		if selfClosing {
			out.WriteString("/>")
		} else {
			out.WriteByte('>')
		}
	}

	return out.String()
}

type parsedAttr struct {
	name     string
	value    string
	hasValue bool
}

func isTagNameChar(b byte, first bool) bool {
	if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
		return true
	}
	if !first && ((b >= '0' && b <= '9') || b == '-') {
		return true
	}
	return false
}

func isValidAttrName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == ':' {
			continue
		}
		return false
	}
	return true
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// scanTagRest parses attributes starting at pos (immediately after the tag
// name) and returns the index right after the closing '>'.
func scanTagRest(s string, pos int) (end int, attrs []parsedAttr, selfClosing bool, ok bool) {
	n := len(s)
	for pos < n {
		for pos < n && isASCIISpace(s[pos]) {
			pos++
		}
		if pos >= n {
			return n, nil, false, false
		}
		if s[pos] == '>' {
			return pos + 1, attrs, selfClosing, true
		}
		if s[pos] == '/' {
			pos++
			for pos < n && isASCIISpace(s[pos]) {
				pos++
			}
			if pos < n && s[pos] == '>' {
				return pos + 1, attrs, true, true
			}
			continue
		}

		// Attribute name.
		attrStart := pos
		for pos < n && !isASCIISpace(s[pos]) && s[pos] != '=' && s[pos] != '>' && s[pos] != '/' {
			pos++
		}
		attrName := s[attrStart:pos]
		for pos < n && isASCIISpace(s[pos]) {
			pos++
		}

		if pos >= n || s[pos] != '=' {
			if attrName != "" {
				attrs = append(attrs, parsedAttr{name: attrName})
			}
			continue
		}
		pos++ // skip '='
		for pos < n && isASCIISpace(s[pos]) {
			pos++
		}
		if pos >= n {
			return n, nil, false, false
		}

		var val string
		if s[pos] == '"' || s[pos] == '\'' {
			quote := s[pos]
			pos++
			valStart := pos
			for pos < n && s[pos] != quote {
				pos++
			}
			if pos >= n {
				return n, nil, false, false
			}
			val = s[valStart:pos]
			pos++ // skip closing quote
		} else {
			valStart := pos
			for pos < n && !isASCIISpace(s[pos]) && s[pos] != '>' {
				pos++
			}
			val = s[valStart:pos]
		}
		if attrName != "" {
			attrs = append(attrs, parsedAttr{name: attrName, value: val, hasValue: true})
		}
	}
	return n, nil, false, false
}

// skipRawTextElement advances past the closing </tagName> of a raw-text
// element such as <script> or <style>, or to len(s) if unclosed.
func skipRawTextElement(s string, pos int, tagName string) int {
	n := len(s)
	needle := "</" + tagName
	for pos < n {
		idx := strings.IndexByte(s[pos:], '<')
		if idx < 0 {
			return n
		}
		pos += idx
		if pos+len(needle) <= n && strings.EqualFold(s[pos:pos+len(needle)], needle) {
			after := pos + len(needle)
			if after < n && (s[after] == '>' || s[after] == '/' || isASCIISpace(s[after])) {
				end := strings.IndexByte(s[after:], '>')
				if end < 0 {
					return n
				}
				return after + end + 1
			}
		}
		pos++
	}
	return n
}

// isUnsafeURL reports whether an attribute value carries a dangerous URI
// scheme (javascript:, vbscript:, data:, or any non-allowlisted scheme) even
// when obfuscated with HTML character references, mixed case, or embedded
// whitespace/control characters.
func isUnsafeURL(raw string) bool {
	decoded := decodeHTMLReferences(raw)

	var cleaned strings.Builder
	cleaned.Grow(len(decoded))
	for _, r := range decoded {
		if r <= 0x20 || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r) {
			continue
		}
		cleaned.WriteRune(unicode.ToLower(r))
	}
	norm := cleaned.String()
	if norm == "" {
		return false
	}

	if strings.HasPrefix(norm, "javascript:") ||
		strings.HasPrefix(norm, "vbscript:") ||
		strings.HasPrefix(norm, "data:") {
		return true
	}

	for i := 0; i < len(norm); i++ {
		switch norm[i] {
		case '/', '?', '#':
			// Relative URL (no scheme before path/query/fragment).
			return false
		case ':':
			scheme := norm[:i]
			return !allowedSchemes[scheme]
		}
	}
	return false
}

// decodeHTMLReferences unescapes standard HTML entities and semicolon-less
// numeric character references (e.g. &#x61 or &#0000106) that browsers decode
// inside attribute values.
func decodeHTMLReferences(s string) string {
	s = html.UnescapeString(s)
	if !strings.Contains(s, "&#") {
		return s
	}

	var out strings.Builder
	out.Grow(len(s))
	n := len(s)
	for i := 0; i < n; {
		if i+2 < n && s[i] == '&' && s[i+1] == '#' {
			j := i + 2
			hex := false
			if j < n && (s[j] == 'x' || s[j] == 'X') {
				hex = true
				j++
			}
			digitsStart := j
			for j < n && ((s[j] >= '0' && s[j] <= '9') ||
				(hex && ((s[j] >= 'a' && s[j] <= 'f') || (s[j] >= 'A' && s[j] <= 'F')))) {
				j++
			}
			if j > digitsStart {
				base := 10
				if hex {
					base = 16
				}
				if v, err := strconv.ParseInt(s[digitsStart:j], base, 32); err == nil && v > 0 && v <= unicode.MaxRune {
					out.WriteRune(rune(v))
					if j < n && s[j] == ';' {
						j++
					}
					i = j
					continue
				}
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}
