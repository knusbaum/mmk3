package dag

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// testNode is a simple in-memory Node for testing.
type testNode struct {
	name      string
	deps      []*testNode
	orderDeps []*testNode
	needsRun  bool
	runErr    error
	runCount  atomic.Int32
}

func (n *testNode) Dependencies() []*testNode      { return n.deps }
func (n *testNode) OrderDependencies() []*testNode { return n.orderDeps }
func (n *testNode) NeedsRun() (bool, error)         { return n.needsRun, nil }
func (n *testNode) Run() error {
	n.runCount.Add(1)
	return n.runErr
}

func node(name string, needsRun bool, deps ...*testNode) *testNode {
	return &testNode{name: name, needsRun: needsRun, deps: deps}
}

func TestLinearChain(t *testing.T) {
	a := node("a", true)
	b := node("b", true, a)
	c := node("c", true, b)

	if err := Execute(c, 0); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{a, b, c} {
		if n.runCount.Load() != 1 {
			t.Errorf("node %s: expected 1 run, got %d", n.name, n.runCount.Load())
		}
	}
}

func TestSkipsWhenNeedsRunFalse(t *testing.T) {
	a := node("a", false)
	b := node("b", false, a)

	if err := Execute(b, 0); err != nil {
		t.Fatal(err)
	}
	if a.runCount.Load() != 0 {
		t.Errorf("a should not have run")
	}
	if b.runCount.Load() != 0 {
		t.Errorf("b should not have run")
	}
}

// Each node owns its NeedsRun() decision: an upstream running does not
// force a downstream node to run if the downstream says it's not needed.
func TestNoCascadeWhenNeedsRunFalse(t *testing.T) {
	a := node("a", true)
	b := node("b", false, a)

	if err := Execute(b, 0); err != nil {
		t.Fatal(err)
	}
	if a.runCount.Load() != 1 {
		t.Errorf("a should have run once, got %d", a.runCount.Load())
	}
	if b.runCount.Load() != 0 {
		t.Errorf("b should not have run (NeedsRun=false), got %d", b.runCount.Load())
	}
}

func TestDiamondDedup(t *testing.T) {
	// a is a dependency of both b and c; should only run once.
	a := node("a", true)
	b := node("b", true, a)
	c := node("c", true, a)
	d := node("d", true, b, c)

	if err := Execute(d, 4); err != nil {
		t.Fatal(err)
	}
	if a.runCount.Load() != 1 {
		t.Errorf("a should run exactly once, got %d", a.runCount.Load())
	}
}

func TestFailurePropagates(t *testing.T) {
	boom := errors.New("boom")
	a := node("a", true)
	a.runErr = boom
	b := node("b", true, a)

	err := Execute(b, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("expected boom, got %v", err)
	}
	if b.runCount.Load() != 0 {
		t.Errorf("b should not have run after a failed")
	}
}

func TestCycleDetection(t *testing.T) {
	a := node("a", true)
	b := node("b", true, a)
	// create a cycle: a depends on b
	a.deps = []*testNode{b}

	err := Execute(a, 0)
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestOrderOnlyEdgeIgnoredWhenTargetNotInGraph(t *testing.T) {
	// `image` has an order-only edge to `consumer`, but only `image` is the
	// root. `consumer` shouldn't be pulled into the DAG.
	consumer := node("consumer", true)
	image := node("image", true)
	image.orderDeps = []*testNode{consumer}

	g, err := Build(image)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := g.steps[any(consumer)]; ok {
		t.Error("consumer should not be in the DAG when only image is the root")
	}
	if err := g.Run(0); err != nil {
		t.Fatal(err)
	}
	if image.runCount.Load() != 1 {
		t.Errorf("image runCount: got %d, want 1", image.runCount.Load())
	}
	if consumer.runCount.Load() != 0 {
		t.Errorf("consumer should not run when not in graph; runCount=%d", consumer.runCount.Load())
	}
}

func TestOrderOnlyEdgeAppliedWhenTargetInGraph(t *testing.T) {
	// `consumer` and `image` both reachable from `all`; `image` order-only
	// depends on `consumer`. Verify `image` runs after `consumer`.
	consumer := node("consumer", true)
	image := node("image", true)
	image.orderDeps = []*testNode{consumer}
	all := node("all", true, consumer, image)

	var order []string
	var mu sync.Mutex
	hooks := Hooks[*testNode]{
		OnRun: func(n *testNode) {
			mu.Lock()
			order = append(order, n.name)
			mu.Unlock()
		},
	}
	if err := Execute(all, 1, hooks); err != nil {
		t.Fatal(err)
	}
	// Order should be: consumer, image, all (or consumer, image at minimum).
	consumerIdx, imageIdx := -1, -1
	for i, name := range order {
		switch name {
		case "consumer":
			consumerIdx = i
		case "image":
			imageIdx = i
		}
	}
	if consumerIdx < 0 || imageIdx < 0 {
		t.Fatalf("expected both consumer and image to run; got %v", order)
	}
	if !(consumerIdx < imageIdx) {
		t.Errorf("expected consumer to run before image; got %v", order)
	}
}

func TestOrderOnlyCycleDetected(t *testing.T) {
	// Regular: A -> B (A depends on B). Order-only: B -> A. Cycle!
	a := node("a", true)
	b := node("b", true)
	a.deps = []*testNode{b}
	b.orderDeps = []*testNode{a}

	_, err := Build(a)
	if err == nil {
		t.Fatal("expected cycle error from order-only edge")
	}
}

func TestParallelism(t *testing.T) {
	// Wide fan-in: many independent nodes all feeding one root.
	root := node("root", true)
	for i := range 10 {
		_ = i
		dep := node("dep", true)
		root.deps = append(root.deps, dep)
	}

	if err := Execute(root, 4); err != nil {
		t.Fatal(err)
	}
	if root.runCount.Load() != 1 {
		t.Errorf("root should run once")
	}
}
