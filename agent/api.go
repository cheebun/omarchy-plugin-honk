package main

import (
	"context"
	"math"
	"net/url"
	"strings"
)

// Native API response shapes, restricted to the fields the agent reads.
// Contract: docs/reference/openapi/native_api_openapi.yaml.

type versionDoc struct {
	API struct {
		Major int `json:"major"`
	} `json:"api"`
	Engine struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"engine"`
}

type runtimeDoc struct {
	Lifecycle struct {
		State         string `json:"state"`
		UptimeSeconds u64    `json:"uptime_seconds"`
	} `json:"lifecycle"`
	Datapath struct {
		Kind string `json:"kind"`
	} `json:"datapath"`
	Traffic struct {
		Connections struct {
			TCP   *int64 `json:"tcp"`
			UDP   *int64 `json:"udp"`
			Total *int64 `json:"total"`
		} `json:"connections"`
		Bytes struct {
			Upload   u64 `json:"upload"`
			Download u64 `json:"download"`
		} `json:"bytes"`
		Rates *struct {
			Upload   u64 `json:"upload_bytes_per_second"`
			Download u64 `json:"download_bytes_per_second"`
		} `json:"rates"`
	} `json:"traffic"`
	Process struct {
		CPUPercent *float64 `json:"cpu_percent"`
	} `json:"process"`
	Degradations []struct {
		Component string `json:"component"`
		Message   string `json:"message"`
	} `json:"degradations"`
}

type memoryDoc struct {
	Process *struct {
		RSS u64 `json:"rss_bytes"`
	} `json:"process"`
	Cgroup *struct {
		Current u64 `json:"current_bytes"`
	} `json:"cgroup"`
	Kernel *struct {
		EBPF u64 `json:"ebpf_bytes"`
	} `json:"kernel"`
}

type groupSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type groupMember struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // node | group
}

type groupSelection struct {
	MemberID     string `json:"member_id"`
	ResolvedLeaf string `json:"resolved_leaf_node_id"`
	Source       string `json:"source"`
}

type groupDetail struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Policy struct {
		Kind string `json:"kind"`
	} `json:"policy"`
	Members []groupMember `json:"members"`
	Runtime struct {
		Selection struct {
			TCP *groupSelection `json:"tcp"`
		} `json:"selection"`
	} `json:"runtime"`
	Capabilities struct {
		CanSelect   bool `json:"can_select"`
		CanOverride bool `json:"can_override"`
	} `json:"capabilities"`
}

type healthObs struct {
	Transport   string   `json:"transport"`
	Purpose     string   `json:"purpose"`
	State       string   `json:"state"`
	LatencyMs   *float64 `json:"latency_ms"`
	MovingAvgMs *float64 `json:"moving_avg_ms"`
}

type nodeDoc struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Protocol *string     `json:"protocol"`
	Health   []healthObs `json:"health"`
}

type nodeList struct {
	Nodes      []nodeDoc `json:"nodes"`
	NextCursor *string   `json:"next_cursor"`
}

// nodeDelay picks the TCP data-path latency the way the groups tab shows it:
// the URLTest moving average when present, else the last sample. 0 = unknown,
// -1 = measured unavailable.
func nodeDelay(n nodeDoc) int32 {
	unavailable := false
	for _, h := range n.Health {
		if h.Transport != "tcp" || (h.Purpose != "data" && h.Purpose != "shared") {
			continue
		}
		if h.State == "unavailable" {
			unavailable = true
			continue
		}
		if h.State != "healthy" {
			continue
		}
		v := h.MovingAvgMs
		if v == nil {
			v = h.LatencyMs
		}
		if v != nil {
			return int32(math.Max(1, math.Round(*v)))
		}
	}
	if unavailable {
		return -1
	}
	return 0
}

func (c *client) groups(ctx context.Context) ([]groupDetail, error) {
	var list []groupSummary
	if err := c.do(ctx, "GET", "/groups", nil, &list); err != nil {
		return nil, err
	}
	out := make([]groupDetail, 0, len(list))
	for _, g := range list {
		var d groupDetail
		if err := c.do(ctx, "GET", "/groups/"+url.PathEscape(g.ID), nil, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (c *client) nodes(ctx context.Context) (map[string]nodeDoc, error) {
	byID := map[string]nodeDoc{}
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/nodes?limit=1000"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var nl nodeList
		if err := c.do(ctx, "GET", path, nil, &nl); err != nil {
			return nil, err
		}
		for _, n := range nl.Nodes {
			byID[n.ID] = n
		}
		if nl.NextCursor == nil || *nl.NextCursor == "" {
			break
		}
		cursor = *nl.NextCursor
	}
	return byID, nil
}

// findGroup resolves a group by name (the panel addresses groups by name).
func findGroup(groups []groupDetail, name string) (groupDetail, bool) {
	for _, g := range groups {
		if g.Name == name {
			return g, true
		}
	}
	return groupDetail{}, false
}

func findMember(g groupDetail, name string) (groupMember, bool) {
	for _, m := range g.Members {
		if m.Name == name {
			return m, true
		}
	}
	for _, m := range g.Members {
		if strings.EqualFold(m.Name, name) {
			return m, true
		}
	}
	return groupMember{}, false
}
