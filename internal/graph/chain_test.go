package graph

import (
	"slices"
	"testing"
)

func TestLongestChainFollowsGreatestWeight(t *testing.T) {
	t.Parallel()
	g := Graph{
		Nodes: []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}},
		Edges: []Edge{{"b", "a"}, {"c", "b"}, {"d", "a"}, {"e", "d"}, {"e", "c"}},
	}
	w := map[string]int64{"a": 1, "b": 1, "c": 1, "d": 5, "e": 1}
	ids, total, err := LongestChain(g, func(id string) int64 { return w[id] })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"a", "d", "e"}) || total != 7 {
		t.Errorf("chain = %v (%d), want [a d e] (7)", ids, total)
	}
	ids, total, err = LongestChain(g, func(string) int64 { return 1 })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"a", "b", "c", "e"}) || total != 4 {
		t.Errorf("chain by count = %v (%d), want [a b c e] (4)", ids, total)
	}
}

func TestLongestChainOfEmptyGraphIsEmpty(t *testing.T) {
	t.Parallel()
	ids, total, err := LongestChain(Graph{}, func(string) int64 { return 1 })
	if err != nil || len(ids) != 0 || total != 0 {
		t.Errorf("LongestChain(empty) = %v, %d, %v", ids, total, err)
	}
}
