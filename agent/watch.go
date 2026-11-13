package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
)

// watchSession emits what the bar and the overview/groups tabs need, in the
// line shapes sbdbar's QML reads: hello, status, groups and mode.
//
// honk's native API has no push for runtime counters, so status is polled
// every interval; groups (with node latencies) every groupsEvery. While
// keepFlows is set it also re-pins flow recording "on", because honk resets
// runtime recorder settings on every restart and configuration activation.
func watchSession(out *lineWriter, interval time.Duration, keepFlows bool) func(context.Context, *client) (bool, error) {
	const groupsEvery = 5 * time.Second
	const flowsEvery = 30 * time.Second
	return func(ctx context.Context, c *client) (bool, error) {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var ver versionDoc
		err := c.do(rctx, "GET", "/version", nil, &ver)
		cancel()
		if err != nil {
			return false, err
		}
		out.emit(map[string]any{"type": "hello", "version": ver.Engine.Version, "apiVersion": ver.API.Major})
		lastMode := ""
		emitMode := func(ctx context.Context) {
			mode, list := currentMode(ctx, c)
			key := mode + "|" + strings.Join(list, ",")
			if key != lastMode {
				lastMode = key
				out.emit(map[string]any{"type": "mode", "mode": mode, "modeList": list})
			}
		}
		emitMode(ctx)

		delivered := false
		var lastGroups []byte
		var lastGroupsAt, lastFlowsAt time.Time
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			line, err := statusLine(rctx, c)
			if err != nil {
				cancel()
				return delivered, err
			}
			delivered = true
			out.emit(line)

			if time.Since(lastGroupsAt) >= groupsEvery {
				lastGroupsAt = time.Now()
				gl, err := groupsLine(rctx, c)
				if err != nil {
					cancel()
					return delivered, err
				}
				// Skip unchanged repeats, as the sbdbar agent does.
				b, _ := json.Marshal(gl)
				if !bytes.Equal(b, lastGroups) {
					lastGroups = b
					out.emit(gl)
				}
			}
			emitMode(rctx)
			if keepFlows && time.Since(lastFlowsAt) >= flowsEvery {
				lastFlowsAt = time.Now()
				ensureFlowRecording(rctx, c)
			}
			cancel()

			select {
			case <-ctx.Done():
				return delivered, nil
			case <-tick.C:
			}
		}
	}
}

// statusLine keeps sbdbar's field names. honk reports TCP/UDP connection
// counts rather than inbound/outbound, and CPU rather than goroutines; the
// QML labels say so.
func statusLine(ctx context.Context, c *client) (map[string]any, error) {
	var rt runtimeDoc
	if err := c.do(ctx, "GET", "/runtime?detail=summary", nil, &rt); err != nil {
		return nil, err
	}
	var mem memoryDoc
	_ = c.do(ctx, "GET", "/runtime/memory", nil, &mem)

	tr := rt.Traffic
	var up, down int64
	if tr.Rates != nil {
		up, down = tr.Rates.Upload.V, tr.Rates.Download.V
	}
	deref := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	line := map[string]any{
		"type":             "status",
		"online":           true,
		"apiType":          "honk-native",
		"state":            rt.Lifecycle.State,
		"datapath":         rt.Datapath.Kind,
		"memory":           int64(0), // process RSS
		"memoryCgroup":     int64(0), // service cgroup total
		"memoryEbpf":       int64(0), // eBPF maps (kernel)
		"cpuPercent":       nil,
		"connectionsTcp":   deref(tr.Connections.TCP),
		"connectionsUdp":   deref(tr.Connections.UDP),
		"connectionsCount": deref(tr.Connections.Total),
		"trafficAvailable": tr.Rates != nil,
		"uploadRate":       up,
		"downloadRate":     down,
		"uploadTotal":      tr.Bytes.Upload.V,
		"downloadTotal":    tr.Bytes.Download.V,
		"uptime":           "",
		"degradations":     len(rt.Degradations),
	}
	if mem.Process != nil && mem.Process.RSS.Valid {
		line["memory"] = mem.Process.RSS.V
	}
	if mem.Cgroup != nil && mem.Cgroup.Current.Valid {
		line["memoryCgroup"] = mem.Cgroup.Current.V
	}
	if mem.Kernel != nil && mem.Kernel.EBPF.Valid {
		line["memoryEbpf"] = mem.Kernel.EBPF.V
	}
	if rt.Process.CPUPercent != nil {
		line["cpuPercent"] = *rt.Process.CPUPercent
	}
	if rt.Lifecycle.UptimeSeconds.Valid {
		line["uptime"] = formatDuration(time.Duration(rt.Lifecycle.UptimeSeconds.V) * time.Second)
	}
	return line, nil
}

type groupItemJSON struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Delay int32  `json:"delay"`
}

type groupJSON struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Selected   string          `json:"selected"`
	Selectable bool            `json:"selectable"`
	Pinned     bool            `json:"pinned"`
	Items      []groupItemJSON `json:"items"`
}

func groupsLine(ctx context.Context, c *client) (map[string]any, error) {
	groups, err := c.groups(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodes(ctx)
	if err != nil {
		return nil, err
	}
	return buildGroupsLine(groups, nodes), nil
}

// buildGroupsLine maps native groups onto sbdbar's groups line. A nested group
// member shows the protocol and latency of the leaf it currently resolves to.
func buildGroupsLine(groups []groupDetail, nodes map[string]nodeDoc) map[string]any {
	byID := map[string]groupDetail{}
	for _, g := range groups {
		byID[g.ID] = g
	}
	out := make([]groupJSON, 0, len(groups))
	activeNode := ""
	for _, g := range groups {
		items := make([]groupItemJSON, 0, len(g.Members))
		for _, m := range g.Members {
			item := groupItemJSON{Name: m.Name}
			if m.Kind == "group" {
				sub := byID[m.ID]
				item.Type = sub.Policy.Kind
				if sub.Runtime.Selection.TCP != nil {
					item.Delay = nodeDelay(nodes[sub.Runtime.Selection.TCP.ResolvedLeaf])
				}
			} else {
				n := nodes[m.ID]
				if n.Protocol != nil {
					item.Type = *n.Protocol
				}
				if item.Type == "" {
					item.Type = m.Name // built-in direct / block
				}
				item.Delay = nodeDelay(n)
			}
			items = append(items, item)
		}
		gj := groupJSON{
			Name:       g.Name,
			Type:       g.Policy.Kind,
			Selectable: g.Capabilities.CanSelect || g.Capabilities.CanOverride,
			Items:      items,
		}
		if sel := g.Runtime.Selection.TCP; sel != nil {
			if m, ok := memberByID(g, sel.MemberID); ok {
				gj.Selected = m.Name
			}
			gj.Pinned = sel.Source == "override"
		}
		if activeNode == "" {
			activeNode = gj.Selected
		}
		out = append(out, gj)
	}
	return map[string]any{"type": "groups", "groups": out, "activeNode": activeNode}
}

func memberByID(g groupDetail, id string) (groupMember, bool) {
	for _, m := range g.Members {
		if m.ID == id {
			return m, true
		}
	}
	return groupMember{}, false
}

// ensureFlowRecording pins record_flows "on" unless it already is, or the
// configuration prohibits it (native_api.record_flows: false).
func ensureFlowRecording(ctx context.Context, c *client) {
	var st struct {
		Recording struct {
			Flows struct {
				Allowed bool   `json:"allowed"`
				Mode    string `json:"mode"`
			} `json:"flows"`
		} `json:"recording"`
	}
	if c.do(ctx, "GET", "/runtime/settings", nil, &st) != nil {
		return
	}
	if !st.Recording.Flows.Allowed || st.Recording.Flows.Mode == "on" {
		return
	}
	_ = c.do(ctx, "PATCH", "/runtime/settings", map[string]any{"record_flows": "on"}, nil)
}
