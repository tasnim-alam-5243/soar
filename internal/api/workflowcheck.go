package api

import (
	"fmt"
	"regexp"
	"strings"
)

var nodeIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

var nodeTypes = map[string]bool{
	"action": true, "branch": true, "parallel": true, "foreach": true, "wait": true,
	"approval": true, "set": true, "subworkflow": true, "end": true,
}

const maxNodes = 500

// validateDefinition is the structural part of workflow validation (spec section 6):
// node shape, unique ids, edges and branch targets that exist, no cycles, and the
// reserved "agent" node rejected. CEL compilation, parameter schemas and limits
// need the engine and arrive in Step 6, which will replace this function.
func validateDefinition(def map[string]any) []FieldError {
	var v verr
	rawNodes, ok := def["nodes"].([]any)
	if !ok || len(rawNodes) == 0 {
		v.add("definition.nodes", "must be a non-empty array")
		return v.errs
	}
	if len(rawNodes) > maxNodes {
		v.add("definition.nodes", "at most %d nodes are allowed", maxNodes)
		return v.errs
	}

	ids := map[string]bool{}
	type nodeRef struct {
		id    string
		index int
		m     map[string]any
	}
	var nodes []nodeRef
	for i, rn := range rawNodes {
		path := fmt.Sprintf("definition.nodes[%d]", i)
		m, ok := rn.(map[string]any)
		if !ok {
			v.add(path, "must be an object")
			continue
		}
		id, _ := m["id"].(string)
		switch {
		case id == "":
			v.add(path+".id", "is required")
			continue
		case !nodeIDRe.MatchString(id):
			v.add(path+".id", "must start with a letter and use only letters, digits and _ (max 64)")
		case ids[id]:
			v.add(path+".id", "duplicate node id %q", id)
		}
		ids[id] = true
		nodes = append(nodes, nodeRef{id: id, index: i, m: m})

		typ, _ := m["type"].(string)
		action, _ := m["action"].(string)
		if typ == "" && action != "" {
			typ = "action"
		}
		switch {
		case typ == "agent":
			v.add(path+".type", "node type %q is reserved and not enabled yet", "agent")
		case typ == "":
			v.add(path+".type", "is required (or give an action)")
		case !nodeTypes[typ]:
			v.add(path+".type", "unknown node type %q", typ)
		case typ == "action" && action == "":
			v.add(path+".action", "is required for action nodes")
		}
	}

	graph := map[string][]string{}
	link := func(from, to, path string) {
		if !ids[to] {
			v.add(path, "refers to unknown node %q", to)
			return
		}
		graph[from] = append(graph[from], to)
	}

	if rawEdges, present := def["edges"]; present {
		edges, ok := rawEdges.([]any)
		if !ok {
			v.add("definition.edges", "must be an array of \"from -> to\" strings")
		}
		for i, re := range edges {
			path := fmt.Sprintf("definition.edges[%d]", i)
			s, ok := re.(string)
			parts := strings.Split(s, "->")
			if !ok || len(parts) != 2 {
				v.add(path, "must look like \"from -> to\"")
				continue
			}
			from, to := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			if !ids[from] {
				v.add(path, "refers to unknown node %q", from)
				continue
			}
			link(from, to, path)
		}
	}

	for _, n := range nodes {
		path := fmt.Sprintf("definition.nodes[%d]", n.index)
		if typ, _ := n.m["type"].(string); typ != "branch" {
			continue
		}
		targets := func(raw any, p string) {
			list, ok := raw.([]any)
			if !ok {
				v.add(p, "must be an array of node ids")
				return
			}
			for j, t := range list {
				ts, ok := t.(string)
				if !ok {
					v.add(fmt.Sprintf("%s[%d]", p, j), "must be a node id")
					continue
				}
				link(n.id, ts, fmt.Sprintf("%s[%d]", p, j))
			}
		}
		if cases, ok := n.m["cases"].([]any); ok {
			for j, c := range cases {
				cm, ok := c.(map[string]any)
				if !ok {
					v.add(fmt.Sprintf("%s.cases[%d]", path, j), "must be an object")
					continue
				}
				if next, present := cm["next"]; present {
					targets(next, fmt.Sprintf("%s.cases[%d].next", path, j))
				}
			}
		}
		if def, present := n.m["default"]; present {
			targets(def, path+".default")
		}
	}

	if len(v.errs) == 0 && hasCycle(graph) {
		v.add("definition.edges", "the workflow contains a cycle; loops are only allowed through foreach")
	}
	return v.errs
}

func hasCycle(g map[string][]string) bool {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		for _, m := range g[n] {
			switch color[m] {
			case grey:
				return true
			case white:
				if visit(m) {
					return true
				}
			}
		}
		color[n] = black
		return false
	}
	for n := range g {
		if color[n] == white && visit(n) {
			return true
		}
	}
	return false
}
