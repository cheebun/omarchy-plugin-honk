package main

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"
)

const maxConnections = 150

type connDoc struct {
	ID       string   `json:"id"`
	Pname    *string  `json:"pname"`
	State    string   `json:"state"`
	Outbound *string  `json:"outbound"`
	Chain    []string `json:"chain"`
	RuleExpr *string  `json:"rule_expression"`
	Domain   *string  `json:"domain"`
	Dst      *string  `json:"dst"`
	Src      *string  `json:"src"`
	Up       u64      `json:"upload_bytes"`
	Down     u64      `json:"download_bytes"`
	UpRate   u64      `json:"upload_bytes_per_second"`
	DownRate u64      `json:"download_bytes_per_second"`
}

type connListDoc struct {
	TCP      []connDoc `json:"tcp"`
	UDP      []connDoc `json:"udp"`
	TotalTCP int       `json:"total_tcp"`
	TotalUDP int       `json:"total_udp"`
}

// connectionsSession polls GET /connections and emits sbdbar's connections
// snapshot. honk leaves per-connection rates null for userspace connections,
// so rates come from byte deltas between polls.
func connectionsSession(out *lineWriter, interval time.Duration) func(context.Context, *client) (bool, error) {
	return func(ctx context.Context, c *client) (bool, error) {
		t := newConnTracker()
		names := map[string]string{}
		var namesAt time.Time
		delivered := false
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// Chains are group/node IDs; refresh the ID -> name table now and then.
			if time.Since(namesAt) > 10*time.Second {
				if n, err := nameTable(rctx, c); err == nil {
					names, namesAt = n, time.Now()
				}
			}
			var doc connListDoc
			err := c.do(rctx, "GET", "/connections?type=all&limit=1000&detail=full", nil, &doc)
			cancel()
			if err != nil {
				return delivered, err
			}
			delivered = true
			list := t.update(doc, time.Now(), names)
			total := doc.TotalTCP + doc.TotalUDP
			if len(list) > maxConnections {
				list = list[:maxConnections]
			}
			out.emit(map[string]any{"type": "connections", "online": true, "connections": list, "totalCount": total})
			select {
			case <-ctx.Done():
				return delivered, nil
			case <-tick.C:
			}
		}
	}
}

type connSeen struct {
	first    time.Time
	at       time.Time
	up, down int64
}

type connTracker struct {
	seen map[string]connSeen
}

func newConnTracker() *connTracker { return &connTracker{seen: map[string]connSeen{}} }

type connJSON struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	Destination string `json:"destination"`
	Network     string `json:"network"`
	Status      string `json:"status"`
	Inbound     string `json:"inbound"`
	Outbound    string `json:"outbound"`
	Chain       string `json:"chain"`
	Route       string `json:"route"`
	Rule        string `json:"rule"`
	Process     string `json:"process"`
	UpRate      string `json:"upRate"`
	DownRate    string `json:"downRate"`
	UpTotal     string `json:"upTotal"`
	DownTotal   string `json:"downTotal"`
	Upload      int64  `json:"upload"`
	Download    int64  `json:"download"`
	CreatedAt   int64  `json:"createdAt"`
}

// update folds one poll into the tracker and returns the snapshot newest first.
func (t *connTracker) update(doc connListDoc, now time.Time, names map[string]string) []connJSON {
	next := map[string]connSeen{}
	list := make([]connJSON, 0, len(doc.TCP)+len(doc.UDP))
	add := func(c connDoc, network string) {
		prev, ok := t.seen[c.ID]
		s := connSeen{first: now, at: now, up: c.Up.V, down: c.Down.V}
		var upRate, downRate int64
		if ok {
			s.first = prev.first
			if dt := now.Sub(prev.at).Seconds(); dt > 0 {
				upRate = int64(float64(max(0, c.Up.V-prev.up)) / dt)
				downRate = int64(float64(max(0, c.Down.V-prev.down)) / dt)
			}
		}
		if c.UpRate.Valid {
			upRate = c.UpRate.V
		}
		if c.DownRate.Valid {
			downRate = c.DownRate.V
		}
		next[c.ID] = s
		list = append(list, connToJSON(c, network, upRate, downRate, s.first, names))
	}
	for _, c := range doc.TCP {
		add(c, "TCP")
	}
	for _, c := range doc.UDP {
		add(c, "UDP")
	}
	t.seen = next
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt > list[j].CreatedAt
		}
		return list[i].ID < list[j].ID
	})
	return list
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nameTable maps group and node IDs to names for chain display.
func nameTable(ctx context.Context, c *client) (map[string]string, error) {
	names := map[string]string{}
	var groups []groupSummary
	if err := c.do(ctx, "GET", "/groups", nil, &groups); err != nil {
		return nil, err
	}
	for _, g := range groups {
		names[g.ID] = g.Name
	}
	nodes, err := c.nodes(ctx)
	if err != nil {
		return nil, err
	}
	for id, n := range nodes {
		names[id] = n.Name
	}
	return names, nil
}

func connToJSON(c connDoc, network string, upRate, downRate int64, first time.Time, names map[string]string) connJSON {
	dst := str(c.Dst)
	host := str(c.Domain)
	if host == "" {
		host = dst
		if h, _, err := net.SplitHostPort(dst); err == nil {
			host = h
		}
	}
	outbound := str(c.Outbound)
	if outbound == "" {
		outbound = "direct"
	}
	hops := make([]string, 0, len(c.Chain))
	for _, id := range c.Chain {
		if n, ok := names[id]; ok {
			id = n
		}
		hops = append(hops, id)
	}
	chain := strings.Join(hops, " → ")
	if chain == "" {
		chain = outbound
	}
	status := c.State
	if status != "" {
		status = strings.ToUpper(status[:1]) + status[1:]
	}
	return connJSON{
		ID:          c.ID,
		Host:        host,
		Destination: dst,
		Network:     network,
		Status:      status,
		Inbound:     str(c.Src),
		Outbound:    outbound,
		Chain:       chain,
		Route:       outbound,
		Rule:        str(c.RuleExpr),
		Process:     str(c.Pname),
		UpRate:      "↑ " + formatBytes(upRate) + "/s",
		DownRate:    "↓ " + formatBytes(downRate) + "/s",
		UpTotal:     "↑ " + formatBytes(c.Up.V),
		DownTotal:   "↓ " + formatBytes(c.Down.V),
		Upload:      c.Up.V,
		Download:    c.Down.V,
		CreatedAt:   first.UnixMilli(),
	}
}
