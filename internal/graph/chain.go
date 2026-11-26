package graph

// LongestChain returns the dependency chain of g with the greatest total weight, its ids in
// order (each node depends on the one before it), and that total. weight gives each node's
// weight by id. Ties go to the chain that ends, and then runs through, nodes earlier in
// dependency order. It returns no ids for an empty graph.
func LongestChain(g Graph, weight func(id string) int64) ([]string, int64, error) {
	order, _, err := g.order()
	if err != nil {
		return nil, 0, err
	}
	deps := map[string][]string{}
	for _, e := range g.Edges {
		deps[e.From] = append(deps[e.From], e.To)
	}
	pos := map[string]int{}
	for k, i := range order {
		pos[g.Nodes[i].ID] = k
	}
	best := map[string]int64{}
	prev := map[string]string{}
	end := ""
	for _, i := range order {
		id := g.Nodes[i].ID
		var top int64
		via := ""
		for _, d := range deps[id] {
			if via == "" || best[d] > top || (best[d] == top && pos[d] < pos[via]) {
				top, via = best[d], d
			}
		}
		best[id] = top + weight(id)
		if via != "" {
			prev[id] = via
		}
		if end == "" || best[id] > best[end] {
			end = id
		}
	}
	if end == "" {
		return nil, 0, nil
	}
	var ids []string
	for id := end; id != ""; id = prev[id] {
		ids = append([]string{id}, ids...)
	}
	return ids, best[end], nil
}
