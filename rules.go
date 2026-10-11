// boeyu-proxy (POC)：规则转发与改写逻辑（fakeDeviceInfo / simplestResponse）。
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Rule follows the ProxyPin rewrites format:
//   - type "requestReplace" modifies the request before it is forwarded
//   - type "responseReplace" modifies the response returned from upstream
// ID is the stable programmatic identifier (English); Name is the display
// name (Chinese). When ID is empty it falls back to Name.
type Rule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Type    string `json:"type"`
	// Fallback marks a catch-all rule that only takes effect when no other
	// (non-fallback) rule matched the request, so it never interferes with
	// specific rules.
	Fallback bool   `json:"fallback"`
	Items    []Item `json:"items"`
}

// ruleID returns the programmatic identifier for a rule.
func ruleID(r Rule) string {
	if r.ID != "" {
		return r.ID
	}
	return r.Name
}

// Item is one action inside a Rule, e.g. replaceResponseBody.
type Item struct {
	Enabled bool            `json:"enabled"`
	Type    string          `json:"type"`
	Values  json.RawMessage `json:"values"`
}

// LoadRulesFile reads a ProxyPin-style rewrites file (an array of rules).
func LoadRulesFile(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var rules []Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("parse %s: no rules", path)
	}
	normalizeResponseBodies(rules)
	return rules, nil
}

// normalizeResponseBodies fixes every enabled replaceResponseBody whose body
// is a quoted JSON string. Some converter tools store embedded JSON with the
// control characters only single-escaped ("\n"), so after decoding the string
// holds raw control bytes that would produce invalid JSON when served
// verbatim. escapeJSONControl re-escapes exactly those bytes; valid bodies
// pass through byte-for-byte unchanged.
func normalizeResponseBodies(rules []Rule) {
	for i := range rules {
		for j := range rules[i].Items {
			it := &rules[i].Items[j]
			if !it.Enabled || strings.ToLower(it.Type) != "replaceresponsebody" {
				continue
			}
			var bv ruleBodyValue
			if err := json.Unmarshal(it.Values, &bv); err != nil {
				continue
			}
			b := bytes.TrimSpace(bv.Body)
			if len(b) == 0 || b[0] != '"' {
				continue
			}
			var s string
			if err := json.Unmarshal(b, &s); err != nil {
				continue
			}
			esc := escapeJSONControl([]byte(s))
			if json.Valid(esc) {
				q, err := json.Marshal(string(esc))
				if err != nil {
					continue
				}
				if nv, err := json.Marshal(ruleBodyValue{Body: q}); err == nil {
					it.Values = nv
				}
			}
		}
	}
}

// escapeJSONControl replaces raw control characters (U+0000..U+001F) inside
// JSON string literals with the proper short / \uXXXX escape sequence. Bytes
// outside string literals and everything already escaped are left untouched,
// so already-valid JSON round-trips byte-for-byte.
func escapeJSONControl(raw []byte) []byte {
	var out bytes.Buffer
	inStr := false
	escaped := false
	for _, c := range raw {
		if !inStr {
			if c == '"' {
				inStr = true
				escaped = false
			}
			out.WriteByte(c)
			continue
		}
		if escaped {
			escaped = false
			out.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			escaped = true
			out.WriteByte(c)
		case '"':
			inStr = false
			out.WriteByte(c)
		default:
			if c < 0x20 {
				switch c {
				case '\n':
					out.WriteString(`\n`)
				case '\r':
					out.WriteString(`\r`)
				case '\t':
					out.WriteString(`\t`)
				default:
					fmt.Fprintf(&out, `\u%04x`, c)
				}
			} else {
				out.WriteByte(c)
			}
		}
	}
	return out.Bytes()
}

// ruleTargetHost extracts the hostname from a rule URL.
func ruleTargetHost(ruleURL string) string {
	u, err := url.Parse(ruleURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ruleMatches reports whether ruleURL matches the actual request host+path.
func ruleMatches(ruleURL, reqHost, reqPath string) bool {
	u, err := url.Parse(ruleURL)
	if err != nil {
		return false
	}
	rHost := strings.ToLower(u.Hostname())
	if rHost != "" && rHost != strings.ToLower(reqHost) {
		return false
	}
	rPath := u.Path
	if rPath == "" {
		return true
	}
	if reqPath == rPath {
		return true
	}
	if strings.HasSuffix(rPath, "/") && strings.HasPrefix(reqPath, rPath) {
		return true
	}
	return false
}

// ---- value parsers ------------------------------------------------------

type ruleBodyValue struct {
	Body json.RawMessage `json:"body"`
}

// parseBody returns the replacement body bytes. The "body" value in the JSON
// may either be a quoted string (classic ProxyPin export) or a raw JSON
// object/array/number which is used verbatim.
func parseBody(raw json.RawMessage) ([]byte, bool) {
	var v ruleBodyValue
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	b := bytes.TrimSpace(v.Body)
	if len(b) == 0 {
		return []byte{}, true
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, false
		}
		return []byte(s), true
	}
	return b, true
}

func parseStatus(raw json.RawMessage) (int, bool) {
	var v struct {
		StatusCode int `json:"statusCode"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.StatusCode <= 0 {
		return 0, false
	}
	return v.StatusCode, true
}

func parseHeaders(raw json.RawMessage) (map[string]string, bool) {
	var v struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Headers == nil {
		return nil, false
	}
	return v.Headers, true
}

func parsePath(raw json.RawMessage) (string, bool) {
	var v struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Path == "" {
		return "", false
	}
	return v.Path, true
}

// parseJsonPatch decodes a "replaceJsonBody" item's values. The shape is
// {"json": {"status": 200, "msg": "", "data.user": "x"}} — each key is a
// dotted path inside the body JSON that gets overwritten; everything else is
// preserved.
func parseJsonPatch(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var v struct {
		Json map[string]json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Json) == 0 {
		return nil, false
	}
	return v.Json, true
}

// applyJsonPatch parses data as JSON, applies the dotted-path patch and
// re-serialises it. It returns (nil, false) when data is not JSON or nothing
// was matched, so the caller can keep the original body untouched.
func applyJsonPatch(data []byte, patch map[string]json.RawMessage) ([]byte, bool) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	changed := false
	for path, raw := range patch {
		if jsonSetPath(root, path, raw) {
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	return out, true
}

func jsonSetPath(root any, path string, raw json.RawMessage) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	appendMode := false
	if strings.HasSuffix(path, "+") {
		appendMode = true
		path = path[:len(path)-1]
	}
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return false
	}
	cur := root
	for i, p := range parts {
		last := i == len(parts)-1
		if !last {
			switch node := cur.(type) {
			case map[string]any:
				next, ok := node[p]
				if !ok || next == nil {
					next = map[string]any{}
					node[p] = next
				}
				cur = next
			case []any:
				idx, err := strconv.Atoi(p)
				if err != nil || idx < 0 || idx >= len(node) {
					return false
				}
				cur = node[idx]
			default:
				return false
			}
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			if !appendMode {
				node[p] = value
				return true
			}
			return jsonAppendMap(node, p, value)
		case []any:
			idx, err := strconv.Atoi(p)
			if err != nil || idx < 0 || idx >= len(node) {
				return false
			}
			if appendMode {
				updated, changed := jsonAppendValue(node[idx], value)
				if !changed {
					return false
				}
				node[idx] = updated
				return true
			}
			node[idx] = value
			return true
		default:
			return false
		}
	}
	return false
}

// jsonAppendMap appends value to node[key]. Comma-separated string fields get
// the new scalar items; array fields get the elements appended (objects are
// deduplicated by packageName/name when present, otherwise by exact JSON). A
// missing key is created as-is. Returns true when the value was set.
func jsonAppendMap(node map[string]any, key string, value any) bool {
	existing, ok := node[key]
	if !ok || existing == nil {
		node[key] = value
		return true
	}
	updated, changed := jsonAppendValue(existing, value)
	if !changed {
		return false
	}
	node[key] = updated
	return true
}

func jsonAppendValue(existing, value any) (any, bool) {
	switch ex := existing.(type) {
	case string:
		toks, ok := csvTokens(value)
		if !ok || len(toks) == 0 {
			return existing, false
		}
		have := map[string]bool{}
		out := make([]string, 0, len(ex)+8)
		for _, t := range splitCSV(ex) {
			if !have[t] {
				have[t] = true
				out = append(out, t)
			}
		}
		changed := false
		for _, t := range toks {
			if t == "" || have[t] {
				continue
			}
			have[t] = true
			out = append(out, t)
			changed = true
		}
		return strings.Join(out, ","), changed
	case []any:
		have := map[string]bool{}
		for _, e := range ex {
			have[elemID(e)] = true
		}
		changed := false
		for _, e := range multi(value) {
			id := elemID(e)
			if id == "" || have[id] {
				continue
			}
			have[id] = true
			ex = append(ex, e)
			changed = true
		}
		return ex, changed
	}
	return existing, false
}

func splitCSV(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func csvTokens(value any) ([]string, bool) {
	switch v := value.(type) {
	case string:
		return splitCSV(v), true
	case []any:
		var out []string
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	case float64:
		return []string{strconv.FormatFloat(v, 'f', -1, 64)}, true
	case bool:
		return []string{strconv.FormatBool(v)}, true
	}
	return nil, false
}

func multi(value any) []any {
	if arr, ok := value.([]any); ok {
		return arr
	}
	return []any{value}
}

// elemID gives an append-dedup identity for one array element.
func elemID(e any) string {
	if m, ok := e.(map[string]any); ok {
		if pkg, ok := m["packageName"].(string); ok && pkg != "" {
			return "pkg:" + pkg
		}
		if name, ok := m["name"].(string); ok && name != "" {
			return "name:" + name
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeBodyIfGzip returns the uncompressed body when b is gzip, b otherwise.
func decodeBodyIfGzip(b []byte) []byte {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return b
	}
	out, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return b
	}
	return out
}

// ---- request rewriting --------------------------------------------------

func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Transfer-Encoding")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// applyRequestReplace applies every enabled requestReplace rule/item on req.
// Returns the number of items that actually changed something.
func applyRequestReplace(req *http.Request, matched []*Rule) int {
	hits := 0
	for _, r := range matched {
		if r.Type != "requestReplace" {
			continue
		}
		for i := range r.Items {
			it := &r.Items[i]
			if !it.Enabled {
				continue
			}
			switch it.Type {
			case "replaceRequestLine":
				if p, ok := parsePath(it.Values); ok {
					req.URL.Path = p
					hits++
				}
			case "replaceRequestHeader":
				if hdrs, ok := parseHeaders(it.Values); ok {
					for k, v := range hdrs {
						if strings.EqualFold(k, "Host") {
							req.Host = v
						} else {
							req.Header.Set(k, v)
						}
					}
					hits++
				}
			case "replaceRequestBody":
				if b, ok := parseBody(it.Values); ok {
					setRequestBody(req, b)
					hits++
				}
			case "replaceJsonBody":
				if patch, ok := parseJsonPatch(it.Values); ok {
					b, rerr := io.ReadAll(req.Body)
					req.Body.Close()
					if rerr == nil {
						if out, ok2 := applyJsonPatch(b, patch); ok2 {
							setRequestBody(req, out)
							hits++
						} else {
							req.Body = io.NopCloser(bytes.NewReader(b))
							req.ContentLength = int64(len(b))
						}
					}
				}
			}
		}
	}
	return hits
}

// ---- response rewriting -------------------------------------------------

func setResponseBody(resp *http.Response, body []byte) {
	if resp.Body != nil {
		resp.Body.Close()
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
}

// applyResponseReplace applies every enabled responseReplace rule/item on the
// response. It returns true if anything was modified.
func applyResponseReplace(resp *http.Response, matched []*Rule) bool {
	modified := false
	for _, r := range matched {
		if r.Type != "responseReplace" {
			continue
		}
		for i := range r.Items {
			it := &r.Items[i]
			if !it.Enabled {
				continue
			}
			switch it.Type {
			case "replaceResponseStatus":
				if sc, ok := parseStatus(it.Values); ok {
					resp.StatusCode = sc
					resp.Status = fmt.Sprintf("%d %s", sc, http.StatusText(sc))
					modified = true
				}
			case "replaceResponseHeader":
				if hdrs, ok := parseHeaders(it.Values); ok {
					for k, v := range hdrs {
						resp.Header.Set(k, v)
					}
					modified = true
				}
			case "replaceResponseBody":
				if b, ok := parseBody(it.Values); ok {
					setResponseBody(resp, b)
					modified = true
				}
			case "replaceJsonBody":
				if patch, ok := parseJsonPatch(it.Values); ok {
					raw, rerr := io.ReadAll(resp.Body)
					resp.Body.Close()
					if rerr == nil {
						if out, ok2 := applyJsonPatch(decodeBodyIfGzip(raw), patch); ok2 {
							setResponseBody(resp, out)
							modified = true
						} else {
							resp.Body = io.NopCloser(bytes.NewReader(raw))
							resp.ContentLength = int64(len(raw))
						}
					}
				}
			}
		}
	}
	return modified
}

var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Transfer-Encoding",
	"Upgrade",
}

// finalizeResponse cleans framing/hop-by-hop headers so the response re-sent
// by us is always well formed. When the body was replaced we also drop
// Content-Encoding/Date because they no longer describe the body. For
// streaming passthrough (ContentLength == -1) no Content-Length is set so the
// write path can keep chunked framing intact.
func finalizeResponse(resp *http.Response, modified bool) {
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	resp.Header.Del("Content-Length")
	if modified {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Date")
	}
	if resp.ContentLength >= 0 {
		resp.Header.Set("Content-Length", strconv.Itoa(int(resp.ContentLength)))
	}
}
