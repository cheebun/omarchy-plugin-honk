package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxLogEntries = 300

var logLevels = []string{"error", "warn", "info", "debug", "trace"}

func parseLogLevel(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "warning" {
		s = "warn"
	}
	for _, l := range logLevels {
		if l == s {
			return s, true
		}
	}
	return "", false
}

type logRecord struct {
	TS      string         `json:"ts"`
	Level   string         `json:"level"`
	Target  *string        `json:"target"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

// logJSON is sbdbar's log entry: raw is what the panel renders, message the
// plain text it searches.
type logJSON struct {
	Level   string `json:"level"`
	Seq     string `json:"seq"`
	Message string `json:"message"`
	Raw     string `json:"raw"`
}

// logsSession follows GET /logs (SSE: stream.ready, then replay, then live).
// honk discloses only audited messages, so the stream is quieter than
// sing-box's console. Records are batched briefly to keep QML updates cheap.
func logsSession(out *lineWriter, level string) func(context.Context, *client) (bool, error) {
	return func(ctx context.Context, c *client) (bool, error) {
		req, err := c.newRequest(ctx, "GET", "/logs?level="+url.QueryEscape(level), nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := c.http.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			var buf [4096]byte
			n, _ := resp.Body.Read(buf[:])
			return false, parseAPIError(resp.StatusCode, buf[:n])
		}

		events := make(chan sseEvent, 64)
		errc := make(chan error, 1)
		go func() { errc <- readSSE(resp.Body, events) }()

		delivered := false
		var batch []logJSON
		seq := 0
		flush := time.NewTicker(250 * time.Millisecond)
		defer flush.Stop()
		for {
			select {
			case <-ctx.Done():
				return delivered, nil
			case err := <-errc:
				return delivered, err
			case ev := <-events:
				if ev.Event == "stream.ready" {
					delivered = true
					batch = nil
					out.emit(map[string]any{"type": "logs", "online": true, "reset": true, "entries": []logJSON{}})
					continue
				}
				var rec logRecord
				if json.Unmarshal([]byte(ev.Data), &rec) != nil || rec.Message == "" {
					continue
				}
				seq++
				batch = append(batch, logEntry(rec, seq))
			case <-flush.C:
				if len(batch) == 0 {
					continue
				}
				if len(batch) > maxLogEntries {
					batch = batch[len(batch)-maxLogEntries:]
				}
				out.emit(map[string]any{"type": "logs", "online": true, "reset": false, "entries": batch})
				batch = nil
			}
		}
	}
}

// logEntry renders one record as "HH:MM:SS target: message key=value ...".
func logEntry(r logRecord, seq int) logJSON {
	msg := r.Message
	if len(r.Fields) > 0 {
		keys := make([]string, 0, len(r.Fields))
		for k := range r.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, r.Fields[k]))
		}
		msg += " " + strings.Join(parts, " ")
	}
	target := str(r.Target)
	clock := r.TS
	if t, err := time.Parse(time.RFC3339Nano, r.TS); err == nil {
		clock = t.Local().Format("15:04:05")
	}
	raw := clock + " " + msg
	if target != "" {
		raw = clock + " " + target + ": " + msg
	}
	return logJSON{Level: r.Level, Seq: fmt.Sprint(seq), Message: msg, Raw: raw}
}

type sseEvent struct {
	Event string
	ID    string
	Data  string
}

// readSSE parses text/event-stream frames; comment lines (heartbeats) are skipped.
func readSSE(r interface{ Read([]byte) (int, error) }, events chan<- sseEvent) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ev sseEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Event != "" || len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				events <- ev
			}
			ev, data = sseEvent{}, nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "id:"):
			ev.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("log stream closed by honk")
}
