package main

import (
	"testing"
	"time"
)

func TestPickActiveNode_FirstAlive(t *testing.T) {
	probe := func(node string) bool { return node == "10.0.0.1" }
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, time.Duration(0), probe)
	if got != "10.0.0.1" {
		t.Fatalf("want 10.0.0.1, got %q", got)
	}
}

func TestPickActiveNode_FailoverToSecond(t *testing.T) {
	probe := func(node string) bool { return node == "10.0.0.2" }
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, time.Duration(0), probe)
	if got != "10.0.0.2" {
		t.Fatalf("want 10.0.0.2 (failover), got %q", got)
	}
}

func TestPickActiveNode_NoneAlive(t *testing.T) {
	probe := func(node string) bool { return false }
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, time.Duration(0), probe)
	if got != "" {
		t.Fatalf("want empty (none alive), got %q", got)
	}
}

func TestPickActiveNode_RetriesFirstBeforeFailover(t *testing.T) {
	// 首选节点前两次失败、第三次成功 → 应坚持返回首选，不切第二个。
	calls := 0
	probe := func(node string) bool {
		if node == "10.0.0.1" {
			calls++
			return calls == 3
		}
		return true // second node always alive
	}
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, time.Duration(0), probe)
	if got != "10.0.0.1" {
		t.Fatalf("want 10.0.0.1 after retry, got %q", got)
	}
	if calls != 3 {
		t.Fatalf("want 3 probes on first node, got %d", calls)
	}
}
