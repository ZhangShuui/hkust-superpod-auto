package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestUpsertSSHBlock_AppendsWhenMissing(t *testing.T) {
	existing := "Host github.com\n    User git\n"
	got := upsertSSHBlock(existing, "hpc4", "Host hpc4\n    User me")
	if !strings.Contains(got, "Host github.com") {
		t.Fatal("clobbered an unrelated host block")
	}
	if !strings.Contains(got, "Host hpc4\n    User me") {
		t.Fatalf("hpc4 block not appended:\n%s", got)
	}
}

func TestUpsertSSHBlock_ReplacesStaleBlockOnly(t *testing.T) {
	existing := "Host superpod\n    User old\n\nHost hpc4\n    User stale\n\nHost other\n    User keep\n"
	got := upsertSSHBlock(existing, "hpc4", "Host hpc4\n    User fresh")
	if strings.Contains(got, "User stale") {
		t.Fatalf("stale hpc4 block survived:\n%s", got)
	}
	for _, want := range []string{"Host superpod\n    User old", "Host other\n    User keep", "User fresh"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestUpsertSSHBlock_Idempotent(t *testing.T) {
	desired := "Host hpc4\n    User me"
	once := upsertSSHBlock("Host other\n    User keep\n", "hpc4", desired)
	twice := upsertSSHBlock(once, "hpc4", desired)
	if once != twice {
		t.Fatalf("second call churned the file:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// A rejected login must never be retried: enough failed attempts lock the ITSC
// account, and that takes the VPN down with it.
func TestIsFatalSSHErr(t *testing.T) {
	fatal := []string{
		"<itsc-id>@hpc4.ust.hk: Permission denied (publickey,password).",
		"Received disconnect: Too many authentication failures",
		"Host key verification failed.",
	}
	for _, msg := range fatal {
		if !isFatalSSHErr(msg) {
			t.Errorf("should not be retried: %q", msg)
		}
	}
	retryable := "Connection reset by peer"
	if isFatalSSHErr(retryable) {
		t.Errorf("should still be retried: %q", retryable)
	}
}

// SuperPod keeps its historical /tmp names so a tunnel started by an older
// binary is still found; HPC4 must not share a single one of them.
func TestTargetTmpPathsAreDistinct(t *testing.T) {
	sp, hpc := targets["superpod"], targets["hpc4"]
	for _, base := range []string{"tunnel.lock", "tunnel.log", "socks.lock"} {
		if sp.tmp(base) == hpc.tmp(base) {
			t.Fatalf("%s collides across clusters: %s", base, sp.tmp(base))
		}
		if want := filepath.Join(os.TempDir(), "spod-"+base); sp.tmp(base) != want {
			t.Fatalf("SuperPod path moved: want %s, got %s", want, sp.tmp(base))
		}
	}
	if sp.defSocks == hpc.defSocks {
		t.Fatalf("both clusters default to SOCKS port %s", sp.defSocks)
	}
}
