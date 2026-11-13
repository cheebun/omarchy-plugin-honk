package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// runAction performs one native API call and returns the JSON result the
// panel expects: {"success": bool, "status"?: int, "error"?: string}.
func runAction(c *client, name string, args []string) map[string]any {
	timeout := 5 * time.Second
	if name == "urltest" {
		// Each node job may take up to honk's 30 s probe deadline.
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var err error
	switch name {
	case "select":
		if len(args) < 2 {
			return failure(errors.New("usage: select <group> <member>"))
		}
		err = selectMember(ctx, c, args[0], args[1])
	case "unpin":
		if len(args) < 1 {
			return failure(errors.New("usage: unpin <group>"))
		}
		err = unpin(ctx, c, args[0])
	case "set-mode":
		if len(args) < 1 {
			return failure(errors.New("usage: set-mode <mode>"))
		}
		target := ""
		if len(args) > 1 {
			target = args[1]
		}
		if err = setMode(c, args[0], target); err == nil {
			return map[string]any{"success": true, "mode": normalizeMode(args[0])}
		}
	case "urltest":
		group := ""
		if len(args) > 0 {
			group = args[0]
		}
		err = urlTest(ctx, c, group)
	case "close":
		if len(args) < 1 {
			return failure(errors.New("usage: close <id>"))
		}
		err = c.do(ctx, "DELETE", "/connections/"+url.PathEscape(args[0]), nil, nil)
	case "close-all":
		err = c.do(ctx, "DELETE", "/connections?all=true", nil, nil)
	default:
		return failure(fmt.Errorf("unknown action %q", name))
	}
	if err != nil {
		return failure(err)
	}
	return map[string]any{"success": true}
}

// selectMember replaces a Selector's choice or pins an automatic group's
// member, for both TCP and UDP (as Clash and doona do).
func selectMember(ctx context.Context, c *client, groupName, memberName string) error {
	groups, err := c.groups(ctx)
	if err != nil {
		return err
	}
	g, ok := findGroup(groups, groupName)
	if !ok {
		return fmt.Errorf("group %q not found", groupName)
	}
	m, ok := findMember(g, memberName)
	if !ok {
		return fmt.Errorf("%q is not a member of %q", memberName, groupName)
	}
	return c.do(ctx, "PUT", "/groups/"+url.PathEscape(g.ID)+"/selection",
		map[string]any{"member_id": m.ID, "network": "both"}, nil)
}

// unpin returns an automatic group to automatic choice.
func unpin(ctx context.Context, c *client, groupName string) error {
	groups, err := c.groups(ctx)
	if err != nil {
		return err
	}
	g, ok := findGroup(groups, groupName)
	if !ok {
		return fmt.Errorf("group %q not found", groupName)
	}
	return c.do(ctx, "DELETE", "/groups/"+url.PathEscape(g.ID)+"/selection?network=both", nil, nil)
}

// urlTest measures every node of one group (or of all groups). honk runs a
// group job's members one after another under a single 30 s deadline, so a
// few unreachable nodes starve the rest; one job per node, four at a time
// (the active-job limit), finishes the whole set. Nested group members are
// covered by their own nodes.
func urlTest(ctx context.Context, c *client, groupName string) error {
	groups, err := c.groups(ctx)
	if err != nil {
		return err
	}
	var targets []groupDetail
	if groupName == "" {
		targets = groups
	} else {
		g, ok := findGroup(groups, groupName)
		if !ok {
			return fmt.Errorf("group %q not found", groupName)
		}
		targets = []groupDetail{g}
	}
	nodes, err := c.nodes(ctx)
	if err != nil {
		return err
	}
	ids := probeNodeIDs(targets, nodes)
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := probeNode(ctx, c, id); err != nil {
				mu.Lock()
				failed = append(failed, nodes[id].Name+": "+errorMessage(err))
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	if len(failed) > 0 {
		return fmt.Errorf("URL test failed for %s", strings.Join(failed, "; "))
	}
	return nil
}

// probeNodeIDs lists the distinct proxy nodes that are direct members of the
// targets, skipping built-in direct/block (they have no check to run).
func probeNodeIDs(targets []groupDetail, nodes map[string]nodeDoc) []string {
	seen := map[string]bool{}
	var ids []string
	for _, g := range targets {
		for _, m := range g.Members {
			if m.Kind != "node" || seen[m.ID] || m.Name == "direct" || m.Name == "block" {
				continue
			}
			if _, ok := nodes[m.ID]; !ok {
				continue
			}
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// probeNode runs one HTTP probe of a node, retrying admission on 429/503.
// A probe that measures the node unavailable is a result, not a failure.
func probeNode(ctx context.Context, c *client, nodeID string) error {
	body := map[string]any{
		"target":     map[string]any{"type": "node", "node_id": nodeID},
		"kind":       "http",
		"transport":  []string{"tcp"},
		"ip_version": "ipv4",
		"warmth":     "warm",
	}
	for {
		var op struct {
			OperationID string `json:"operation_id"`
		}
		err := c.do(ctx, "POST", "/probes", body, &op)
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == 429 || ae.Status == 503) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err != nil {
			return err
		}
		return waitOperation(ctx, c, op.OperationID)
	}
}

// waitOperation polls GET /operations/{id} until it leaves queued/running.
func waitOperation(ctx context.Context, c *client, id string) error {
	for {
		var st struct {
			Status string `json:"status"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := c.do(ctx, "GET", "/operations/"+url.PathEscape(id), nil, &st); err != nil {
			return err
		}
		switch st.Status {
		case "succeeded":
			return nil
		case "failed":
			if st.Error != nil {
				return fmt.Errorf("%s (%s)", st.Error.Message, st.Error.Code)
			}
			return errors.New("operation failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func failure(err error) map[string]any {
	return map[string]any{"success": false, "status": statusCode(err), "error": errorMessage(err)}
}
