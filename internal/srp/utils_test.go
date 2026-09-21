package srp

import (
	"bytes"
	"testing"
)

func TestPadTo(t *testing.T) {
	// Normal padding
	b1 := []byte{0x01, 0x02}
	res1 := padTo(b1, 4)
	if len(res1) != 4 || !bytes.Equal(res1, []byte{0x00, 0x00, 0x01, 0x02}) {
		t.Fatalf("padTo 正常填充失败: got %v", res1)
	}

	// Exact length
	res2 := padTo(b1, 2)
	if len(res2) != 2 || !bytes.Equal(res2, b1) {
		t.Fatalf("padTo 等长失败: got %v", res2)
	}

	// Over-length (should not panic)
	res3 := padTo(b1, 1)
	if len(res3) != 2 || !bytes.Equal(res3, b1) {
		t.Fatalf("padTo 超长防御失败: got %v", res3)
	}
}
