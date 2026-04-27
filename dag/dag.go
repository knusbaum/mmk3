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
func Build[T Node[T]](root T) (*Graph[T], error) {
	steps := make(map[any]*step[T])
	s, err := buildGraph(steps, nil, root)
	if err != nil {
		return nil, err
	}
	return &Graph[T]{root: s, steps: steps}, nil
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
