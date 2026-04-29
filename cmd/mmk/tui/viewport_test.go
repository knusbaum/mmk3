package tui

import (
	"testing"
)

func TestClampDagOffsets(t *testing.T) {
	// 200 wide × 50 tall drawing; 80x24 viewport.
	m := model{
		dagW:   200,
		dagH:   50,
		width:  80,
		height: 24,
	}

	// Out-of-range high → clamped to max.
	m.dagX, m.dagY = 99999, 99999
	m.clampDagOffsets()
	visW, visH := m.dagVisibleSize()
	if got, want := m.dagX, m.dagW-visW; got != want {
		t.Fatalf("dagX clamp high: got %d want %d", got, want)
	}
	if got, want := m.dagY, m.dagH-visH; got != want {
		t.Fatalf("dagY clamp high: got %d want %d", got, want)
	}

	// Negative → clamped to 0.
	m.dagX, m.dagY = -10, -10
	m.clampDagOffsets()
	if m.dagX != 0 || m.dagY != 0 {
		t.Fatalf("negative offsets not clamped: x=%d y=%d", m.dagX, m.dagY)
	}

	// Drawing smaller than viewport → both axes clamp to 0.
	m = model{dagW: 20, dagH: 10, width: 80, height: 24}
	m.dagX, m.dagY = 5, 5
	m.clampDagOffsets()
	if m.dagX != 0 || m.dagY != 0 {
		t.Fatalf("small drawing should clamp to 0: x=%d y=%d", m.dagX, m.dagY)
	}
}
