package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Outbound mode (Rule / Global / Direct), the way doona does it.
//
// honk's native mode endpoint (/api/v1/x-honk/runtime/mode) is gated, so
// doona switches modes by editing the configuration through the native API:
// Global/Direct insert one catch-all rule right after the leading `(must)`
// rules of the top-level routing block,
//
//	l4proto(tcp, udp) -> <group|direct> # doona: outbound mode
//
// and Rule removes it. The write is PUT /config/sources/{id} with If-Match,
// which validates, writes the file and reloads.
//
// This uses the same line and marker, so doona and the plugin agree. doona
// only edits the main source; the agent uses the main source when it has a
// routing block and otherwise the first writable source that does (here
// config.d/proxy.dae). doona does not see a mode written to an include.

const modeMarker = "# doona: outbound mode"

var clashModes = []string{"Rule", "Global", "Direct"}

func normalizeMode(m string) string {
	for _, known := range clashModes {
		if strings.EqualFold(known, m) {
			return known
		}
	}
	return m
}

// ---- Text transform -------------------------------------------------------

type routingBlock struct {
	open, close int // byte offsets of '{' and its matching '}'
}

// findRouting locates the top-level `routing { ... }` block, skipping quoted
// text and comments (an unquoted '#' at line start or after whitespace).
func findRouting(src string) (routingBlock, bool) {
	depth := 0
	var quote byte
	word := strings.Builder{}
	lastWord := ""
	for i := 0; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n'):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{':
			if depth == 0 && lastWord == "routing" {
				close := matchBrace(src, i)
				if close < 0 {
					return routingBlock{}, false
				}
				return routingBlock{open: i, close: close}, true
			}
			depth++
		case c == '}':
			depth--
		}
		if isWordByte(c) {
			word.WriteByte(c)
		} else if word.Len() > 0 {
			lastWord = word.String()
			word.Reset()
		}
	}
	return routingBlock{}, false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func matchBrace(src string, open int) int {
	depth := 0
	var quote byte
	for i := open; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n'):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

var (
	markerRe = regexp.MustCompile(`^\s*l4proto\(tcp, udp\)\s*->\s*(\S+)\s+` + regexp.QuoteMeta(modeMarker) + `\s*$`)
	mustRe   = regexp.MustCompile(`->\s*[^\s#]+\([^)]*\bmust\b[^)]*\)\s*(#.*)?$`)
	ruleRe   = regexp.MustCompile(`->`)
)

// readMode returns the mode and Global target encoded in src.
func readMode(src string) (string, string) {
	rb, ok := findRouting(src)
	if !ok {
		return "Rule", ""
	}
	for _, line := range strings.Split(src[rb.open+1:rb.close], "\n") {
		if m := markerRe.FindStringSubmatch(line); m != nil {
			if m[1] == "direct" {
				return "Direct", ""
			}
			return "Global", m[1]
		}
	}
	return "Rule", ""
}

// applyMode rewrites src for mode ("Rule", "Global" with target, "Direct").
func applyMode(src, mode, target string) (string, error) {
	rb, ok := findRouting(src)
	if !ok {
		return "", errors.New("the configuration has no routing section")
	}
	body := src[rb.open+1 : rb.close]
	lines := strings.Split(body, "\n")

	// Drop any previous marker line.
	kept := lines[:0:0]
	for _, l := range lines {
		if !markerRe.MatchString(l) {
			kept = append(kept, l)
		}
	}
	lines = kept
	if mode == "Rule" {
		return src[:rb.open+1] + strings.Join(lines, "\n") + src[rb.close:], nil
	}
	to := "direct"
	if mode == "Global" {
		if target == "" {
			return "", errors.New("global mode needs a target group")
		}
		to = target
	}

	// Insert after the leading (must) rules. Statements may span lines while
	// parentheses are open. A must rule after an ordinary one would be shadowed
	// by the catch-all, so refuse (doona: act.modeInterleaved).
	insertAt, indent := 0, "    "
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		insertAt = 1
	}
	seenOrdinary, firstStmt := false, true
	paren := 0
	var stmt strings.Builder
	stmtStart := 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if paren == 0 && (t == "" || strings.HasPrefix(t, "#")) {
			continue
		}
		if paren == 0 {
			stmtStart = i
			stmt.Reset()
			if firstStmt {
				indent = l[:len(l)-len(strings.TrimLeft(l, " \t"))]
				firstStmt = false
			}
		}
		stmt.WriteString(t + " ")
		paren += strings.Count(stripComment(t), "(") - strings.Count(stripComment(t), ")")
		if paren > 0 {
			continue
		}
		paren = 0
		s := strings.TrimSpace(stmt.String())
		if !ruleRe.MatchString(s) {
			continue // fallback:/default: and other settings
		}
		if mustRe.MatchString(s) {
			if seenOrdinary {
				return "", fmt.Errorf("a (must) rule follows an ordinary rule (%q); move it up before switching modes", lines[stmtStart])
			}
			insertAt = i + 1
		} else {
			seenOrdinary = true
		}
	}
	line := indent + "l4proto(tcp, udp) -> " + to + " " + modeMarker
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, line)
	out = append(out, lines[insertAt:]...)
	return src[:rb.open+1] + strings.Join(out, "\n") + src[rb.close:], nil
}

func stripComment(t string) string {
	if i := strings.Index(t, " #"); i >= 0 {
		return t[:i]
	}
	return t
}

// ---- Native API -----------------------------------------------------------

type sourceMeta struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	SHA      string `json:"content_sha256"`
	Writable bool   `json:"writable"`
}

type sourceContent struct {
	sourceMeta
	Content string `json:"content"`
}

// modeSource caches the routing source by content hash so the watch loop
// only refetches content after it changed.
type modeSource struct {
	src   sourceContent
	found bool
}

var modeCache = map[string]modeSource{} // sha -> source

func (c *client) routingSource(ctx context.Context) (sourceContent, error) {
	var cfg struct {
		Sources []sourceMeta `json:"sources"`
	}
	if err := c.do(ctx, "GET", "/config", nil, &cfg); err != nil {
		return sourceContent{}, err
	}
	// Main first, then includes, in listed order.
	ordered := make([]sourceMeta, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		if s.Kind == "main" {
			ordered = append(ordered, s)
		}
	}
	for _, s := range cfg.Sources {
		if s.Kind == "include" {
			ordered = append(ordered, s)
		}
	}
	for _, s := range ordered {
		if !s.Writable {
			continue
		}
		if cached, ok := modeCache[s.SHA]; ok {
			if cached.found {
				return cached.src, nil
			}
			continue
		}
		var full sourceContent
		if err := c.do(ctx, "GET", "/config/sources/"+url.PathEscape(s.ID), nil, &full); err != nil {
			return sourceContent{}, err
		}
		full.Writable = s.Writable
		_, found := findRouting(full.Content)
		modeCache[s.SHA] = modeSource{src: full, found: found}
		if found {
			return full, nil
		}
	}
	return sourceContent{}, errors.New("no writable configuration source has a routing section")
}

// currentMode reports the mode and the list the panel offers; an empty list
// hides the switch (configuration not writable or no routing section).
func currentMode(ctx context.Context, c *client) (string, []string) {
	src, err := c.routingSource(ctx)
	if err != nil {
		return "", []string{}
	}
	mode, _ := readMode(src.Content)
	return mode, clashModes
}

// setMode writes the mode and waits for honk's reload operation. target is
// the Global group; empty picks the first group, as doona does.
func setMode(c *client, mode, target string) error {
	mode = normalizeMode(mode)
	if mode != "Rule" && mode != "Global" && mode != "Direct" {
		return fmt.Errorf("unknown mode %q", mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, err := c.routingSource(ctx)
	if err != nil {
		return err
	}
	if mode == "Global" && target == "" {
		if _, t := readMode(src.Content); t != "" {
			target = t
		} else {
			var groups []groupSummary
			if err := c.do(ctx, "GET", "/groups", nil, &groups); err != nil {
				return err
			}
			if len(groups) == 0 {
				return errors.New("global mode needs a group, and none is configured")
			}
			target = groups[0].Name
		}
	}
	next, err := applyMode(src.Content, mode, target)
	if err != nil {
		return err
	}
	if next == src.Content {
		return nil
	}
	req, err := c.newRequest(ctx, "PUT", "/config/sources/"+url.PathEscape(src.ID), map[string]any{"content": next})
	if err != nil {
		return err
	}
	req.Header.Set("If-Match", `"`+src.SHA+`"`)
	var op struct {
		OperationID string `json:"operation_id"`
	}
	if err := c.send(req, &op); err != nil {
		return err
	}
	if op.OperationID == "" {
		return nil
	}
	return waitOperation(ctx, c, op.OperationID)
}
