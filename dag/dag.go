package dag

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type Node[T any] interface {
	Dependencies() []T
	NeedsRun() bool
	Run() error
}

// Orderer is an optional interface for nodes that have order-only deps.
// An order-only edge constrains scheduling — the referenced node must complete
// before this one — but does NOT pull the referenced node into the DAG. The
// edge is honored only when the referenced node is already in the graph via
// some regular Dependencies() path.
//
// Use case: a destructive operation (e.g. cleaning a shared resource) wants
// to run after consumers but should not force them when invoked alone.
type Orderer[T any] interface {
	OrderDependencies() []T
}

type Semaphore struct {
	c chan struct{}
}

func NewSemaphore(n int) *Semaphore {
	c := make(chan struct{}, n)
	for range n {
		c <- struct{}{}
	}
	return &Semaphore{c: c}
}

func (s *Semaphore) wait()   { <-s.c }
func (s *Semaphore) signal() { s.c <- struct{}{} }

// Hooks carries optional callbacks fired at key points during execution.
// All fields are optional; nil means no-op.
type Hooks[T any] struct {
	// OnSkip is called when a node is skipped because NeedsRun() returned false.
	OnSkip func(T)
	// OnRun is called immediately before Run() is invoked on a node.
	OnRun func(T)
}

type step[T Node[T]] struct {
	n        T
	upstream []*step[T]
	done     chan struct{}
	status   error
}

func (s *step[T]) wait() error {
	<-s.done
	return s.status
}

func (s *step[T]) run(sem *Semaphore, h *Hooks[T]) {
	defer close(s.done)

	for _, u := range s.upstream {
		if err := u.wait(); err != nil {
			s.status = err
			return
		}
	}

	if !s.n.NeedsRun() {
		if h != nil && h.OnSkip != nil {
			h.OnSkip(s.n)
		}
		return
	}

	if sem != nil {
		sem.wait()
		defer sem.signal()
	}

	if h != nil && h.OnRun != nil {
		h.OnRun(s.n)
	}

	s.status = s.n.Run()
}

// Graph is a fully-resolved execution graph. Build one with Build, then run it with Run.
type Graph[T Node[T]] struct {
	root  *step[T]
	steps map[any]*step[T]
}

// Build resolves all transitive dependencies of root and returns the execution
// graph without running any nodes. All Dependencies() calls happen here, so any
// side effects of resolution (e.g. writing pattern rule functions to a script
// file) are complete before Build returns.
//
// After regular dep resolution, nodes that implement Orderer get a second pass:
// each OrderDependencies() entry that's already in the graph becomes an extra
// upstream edge. Order-only edges that point at nodes outside the graph are
// dropped silently — that's the whole point of "order-only".
func Build[T Node[T]](root T) (*Graph[T], error) {
	steps := make(map[any]*step[T])
	s, err := buildGraph(steps, nil, root)
	if err != nil {
		return nil, err
	}
	if err := addOrderOnlyEdges(steps); err != nil {
		return nil, err
	}
	return &Graph[T]{root: s, steps: steps}, nil
}

// addOrderOnlyEdges adds upstream edges for Orderer nodes whose order-only
// dependencies are already in the graph. Detects cycles introduced by these
// new edges (the regular cycle check during buildGraph doesn't see them).
func addOrderOnlyEdges[T Node[T]](steps map[any]*step[T]) error {
	for _, st := range steps {
		orderer, ok := any(st.n).(Orderer[T])
		if !ok {
			continue
		}
		for _, dep := range orderer.OrderDependencies() {
			depStep, ok := steps[any(dep)]
			if !ok {
				continue // referenced node isn't in the DAG; skip
			}
			st.upstream = append(st.upstream, depStep)
		}
	}
	return detectCycle(steps)
}

// detectCycle reports an error if the upstream graph contains a cycle.
// Used after order-only edges are added; buildGraph's chain-based check
// only sees the regular dependency walk.
func detectCycle[T Node[T]](steps map[any]*step[T]) error {
	const (
		white = iota
		gray
		black
	)
	color := make(map[*step[T]]int, len(steps))
	var visit func(s *step[T], path []*step[T]) error
	visit = func(s *step[T], path []*step[T]) error {
		switch color[s] {
		case gray:
			var sb strings.Builder
			sb.WriteString("cycle detected: ")
			cycleStart := 0
			for i, p := range path {
				if p == s {
					cycleStart = i
					break
				}
			}
			for _, p := range path[cycleStart:] {
				fmt.Fprintf(&sb, "%v -> ", any(p.n))
			}
			fmt.Fprintf(&sb, "%v", any(s.n))
			return errors.New(sb.String())
		case black:
			return nil
		}
		color[s] = gray
		path = append(path, s)
		for _, u := range s.upstream {
			if err := visit(u, path); err != nil {
				return err
			}
		}
		color[s] = black
		return nil
	}
	for _, st := range steps {
		if color[st] == white {
			if err := visit(st, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run executes all nodes in the graph. parallelism <= 0 means unlimited.
// An optional Hooks value may be passed to observe execution events.
func (g *Graph[T]) Run(parallelism int, hooks ...Hooks[T]) error {
	if g.root == nil {
		return nil
	}
	var h *Hooks[T]
	if len(hooks) > 0 {
		h = &hooks[0]
	}
	var sem *Semaphore
	if parallelism > 0 {
		sem = NewSemaphore(parallelism)
	}
	var wg sync.WaitGroup
	for _, st := range g.steps {
		wg.Add(1)
		go func(st *step[T]) {
			defer wg.Done()
			st.run(sem, h)
		}(st)
	}
	wg.Wait()
	return g.root.wait()
}

// Execute builds and runs the DAG rooted at root.
// It is a convenience wrapper around Build + Run.
func Execute[T Node[T]](root T, parallelism int, hooks ...Hooks[T]) error {
	g, err := Build(root)
	if err != nil {
		return err
	}
	return g.Run(parallelism, hooks...)
}

func buildGraph[T Node[T]](steps map[any]*step[T], chain []any, n T) (*step[T], error) {
	key := any(n)
	for i, c := range chain {
		if c == key {
			var sb strings.Builder
			sb.WriteString("cycle detected: ")
			for _, c := range chain[i:] {
				fmt.Fprintf(&sb, "%v -> ", c)
			}
			fmt.Fprintf(&sb, "%v", key)
			return nil, errors.New(sb.String())
		}
	}

	if s, ok := steps[key]; ok {
		return s, nil
	}

	chain = append(chain, key)

	var upstreams []*step[T]
	for _, dep := range n.Dependencies() {
		ds, err := buildGraph(steps, chain, dep)
		if err != nil {
			return nil, err
		}
		if ds != nil {
			upstreams = append(upstreams, ds)
		}
	}

	s := &step[T]{
		n:        n,
		upstream: upstreams,
		done:     make(chan struct{}),
	}
	steps[key] = s
	return s, nil
}
