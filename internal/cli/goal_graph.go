package cli

import (
	"context"
	"fmt"

	"github.com/msidorkevich/mill/internal/graph"
)

// runGoalGraph prints a goal's task graph: a lane drawing, or a Mermaid flowchart with --mermaid.
func runGoalGraph(ctx context.Context, env Env, args []string) int {
	fs := newFlags(env, "goal graph")
	asMermaid := fs.Bool("mermaid", false, "print a Mermaid flowchart (renders in GitHub Markdown)")
	id, code, ok := goalArg(env, "goal graph", "mill goal graph <goal> [--mermaid]", fs, args)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "mill goal graph: %v\n", err)
		return ExitError
	}
	_, _, s, err := openProject(ctx, env)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	v, err := loadGoalView(ctx, s, id)
	if err != nil {
		return fail(err)
	}
	if len(v.Tasks) == 0 {
		fmt.Fprintf(env.Stdout, "no tasks yet (the plan is %s): approve it with `mill plan approve %s`\n", v.Goal.Status, id)
		return ExitOK
	}
	var g graph.Graph
	for _, t := range v.Tasks {
		g.Nodes = append(g.Nodes, graph.Node{ID: t.LocalID, Label: t.LocalID, Status: t.Status})
		for _, d := range t.DependsOn {
			g.Edges = append(g.Edges, graph.Edge{From: t.LocalID, To: d})
		}
	}
	render := graph.Terminal
	if *asMermaid {
		render = graph.Mermaid
	}
	if err := render(env.Stdout, g); err != nil {
		return fail(err)
	}
	return ExitOK
}
