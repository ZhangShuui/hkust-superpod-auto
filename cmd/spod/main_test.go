package main

import (
	"errors"
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

// A running autossh is only replaced on an explicit FREE from a cluster that
// answered. Killing it on anything less — an outage above all — leaves nothing
// to rebuild the tunnel once the network is back.
func TestRunningTunnelActionOnlyRebuildsOnExplicitFree(t *testing.T) {
	down := errors.New("exit status 255")
	cases := []struct {
		name           string
		stdout, stderr string
		err            error
		want           tunnelAction
	}{
		{"bound", "BOUND\n", "", nil, tunnelKeep},
		{"free", "FREE\n", "", nil, tunnelRebuild},
		{"free after rc noise", "module: command not found\nFREE\n", "", nil, tunnelRebuild},
		{"network down", "", "ssh: connect to host superpod.ust.hk port 22: Connection timed out", down, tunnelWait},
		{"rate limited", "", "kex_exchange_identification: read: Connection reset by peer", down, tunnelWait},
		{"mux hung past deadline", "", "", errors.New("signal: killed"), tunnelWait},
		{"no ss on the cluster", "UNKNOWN\n", "", nil, tunnelWait},
		{"empty reply", "", "", nil, tunnelWait},
		{"login rejected", "", "<itsc-id>@superpod.ust.hk: Permission denied (publickey).", down, tunnelStop},
	}
	for _, c := range cases {
		if got := runningTunnelAction(c.stdout, c.stderr, c.err); got != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, got)
		}
	}
}

// Between Restart= attempts the unit reads "activating". An autossh started in
// that window takes the remote port, and every retry of the unit becomes a
// login that fails ExitOnForwardFailure.
func TestUnitOwnsTunnelWhileRestarting(t *testing.T) {
	for _, st := range []string{"active", "activating", "deactivating", "reloading"} {
		if !unitOwnsTunnel(st) {
			t.Errorf("%q: the unit still owns the tunnel, spod must not start its own", st)
		}
	}
	for _, st := range []string{"inactive", "failed", "maintenance", ""} {
		if unitOwnsTunnel(st) {
			t.Errorf("%q: systemd will not bring it back, spod should manage the tunnel", st)
		}
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

func TestParsePushLine(t *testing.T) {
	it, valid := parsePushLine("1700000000\trun7/logs\t4096\t/project/foo/a.bin")
	if !valid {
		t.Fatal("valid queue line rejected")
	}
	if it.ts != 1700000000 || it.sub != "run7/logs" || it.size != 4096 || it.path != "/project/foo/a.bin" {
		t.Fatalf("wrong fields: %+v", it)
	}
	if it, valid := parsePushLine("1700000000\t-\t0\t/a/b"); !valid || it.sub != "" {
		t.Fatalf("'-' should mean no subdir, got %+v (valid=%v)", it, valid)
	}
	for _, bad := range []string{
		"",
		"   ",
		"1700000000\t-\t0",                // too few fields
		"1700000000\t-\t0\trelative/path", // not absolute
		"notanumber\t-\t0\t/a/b",          // bogus timestamp
		"1700000000\t-\t0\t/a/b\textra",   // too many fields
	} {
		if _, valid := parsePushLine(bad); valid {
			t.Errorf("accepted malformed line %q", bad)
		}
	}
}

// The queue lives in a cluster home that may be shared with other people on the
// same account, so a subdirectory out of it must never escape the download dir.
func TestSanitizeSubStaysInsideDest(t *testing.T) {
	cases := map[string]string{
		"run7":                "run7",
		"run7/logs":           "run7/logs",
		"../../etc":           "etc",
		"/etc/cron.d":         "etc/cron.d",
		"a/../../../b":        "a/b",
		"./x//y":              "x/y",
		"~":                   "",
		"":                    "",
		"a/b/c/d/e/f/g/h/i/j": "a/b/c/d/e/f/g/h", // depth cap
	}
	for in, want := range cases {
		if got := sanitizeSub(in); got != want {
			t.Errorf("sanitizeSub(%q) = %q, want %q", in, got, want)
		}
		if got := sanitizeSub(in); filepath.IsAbs(got) || strings.Contains(got, "..") {
			t.Errorf("sanitizeSub(%q) = %q escapes dest", in, got)
		}
	}
}

// parallelFetch flattens a batch into one directory, so same-named files have
// to land in different batches instead of racing into one local file.
func TestBatchBySubSplitsDuplicateBasenames(t *testing.T) {
	items := []pushItem{
		{sub: "", path: "/a/x.bin"},
		{sub: "", path: "/b/x.bin"},
		{sub: "run7", path: "/c/y.bin"},
		{sub: "", path: "/d/z.bin"},
	}
	batches := batchBySub(items)
	if len(batches) != 3 {
		t.Fatalf("want 3 batches (2 for the duplicate + 1 subdir), got %d: %+v", len(batches), batches)
	}
	for _, b := range batches {
		seen := map[string]bool{}
		for _, it := range b.items {
			base := filepath.Base(it.path)
			if seen[base] {
				t.Errorf("batch %q holds %q twice", b.sub, base)
			}
			seen[base] = true
		}
	}
}

func TestDedupePush(t *testing.T) {
	items := []pushItem{
		{sub: "", path: "/a/x", raw: "old"},
		{sub: "", path: "/a/x", raw: "new"},
		{sub: "run7", path: "/a/x", raw: "other-dest"},
	}
	got := dedupePush(items)
	if len(got) != 2 {
		t.Fatalf("want 2 unique (sub,path) pairs, got %d", len(got))
	}
	if got[0].raw != "old" {
		t.Errorf("dedupe should keep the first (inflight) copy, got %q", got[0].raw)
	}
}

func TestDisplayPath(t *testing.T) {
	if got := displayPath("/mnt/c/Users/Win11/Downloads"); got != `C:\Users\Win11\Downloads` {
		t.Errorf("got %q", got)
	}
	if got := displayPath("/home/me/dl"); got != "/home/me/dl" {
		t.Errorf("non-WSL path should pass through, got %q", got)
	}
}

func TestAssignNamesSeparatesCollidingBasenames(t *testing.T) {
	items := assignNames([]pushItem{
		{sub: "", path: "/out/decoded_e24b_random/compare.png"},
		{sub: "", path: "/out/decoded_e24b_fixed/compare.png"},
		{sub: "", path: "/out/e25_plane.png"},
		{sub: "run7", path: "/other/compare.png"},
	})
	want := map[string]string{
		"/out/decoded_e24b_random/compare.png": "decoded_e24b_random__compare.png",
		"/out/decoded_e24b_fixed/compare.png":  "decoded_e24b_fixed__compare.png",
		"/out/e25_plane.png":                   "e25_plane.png",
		// A different sub is a different directory, so it keeps the plain name.
		"/other/compare.png": "compare.png",
	}
	for _, it := range items {
		if it.name != want[it.path] {
			t.Errorf("%s → %q, want %q", it.path, it.name, want[it.path])
		}
	}
}

func TestAssignNamesIsStableAcrossRuns(t *testing.T) {
	// A resumed transfer must land on the same sidecar, so the same queue has
	// to yield the same names every time.
	in := []pushItem{
		{sub: "", path: "/a/r1/loss.png"},
		{sub: "", path: "/a/r2/loss.png"},
		{sub: "", path: "/a/r3/loss.png"},
	}
	first, second := assignNames(in), assignNames(in)
	for i := range first {
		if first[i].name != second[i].name {
			t.Fatalf("run 1 named %s %q, run 2 %q", first[i].path, first[i].name, second[i].name)
		}
		if first[i].name == filepath.Base(first[i].path) {
			t.Errorf("%s kept the colliding basename %q", first[i].path, first[i].name)
		}
	}
}

func TestFreeNameFallsBackToDigest(t *testing.T) {
	// Every ancestor prefix is spoken for, so it must still produce something
	// unique — and derived only from the path, so a retry picks the same one.
	taken := map[string]bool{
		"x.bin": true, "d__x.bin": true, "c__d__x.bin": true,
		"b__c__d__x.bin": true, "a__b__c__d__x.bin": true,
	}
	got := freeName("/a/b/c/d/x.bin", taken)
	if taken[got] {
		t.Fatalf("freeName returned a name already taken: %q", got)
	}
	if again := freeName("/a/b/c/d/x.bin", taken); again != got {
		t.Errorf("not deterministic: %q then %q", got, again)
	}
}

func TestHumanAgeSpansDays(t *testing.T) {
	// humanDuration is a stopwatch and gives up past 99h ("--:--"); a queue
	// nobody drained for four days has to read as four days.
	for _, c := range []struct {
		secs int64
		want string
	}{
		{30, "30 秒"},
		{90, "1 分钟"},
		{3600, "1 小时"},
		{4 * 86400, "4 天"},
	} {
		if got := humanAge(c.secs); got != c.want {
			t.Errorf("humanAge(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// One drain, one folder — and the name has to be free. Merging into a folder
// that is already there lets an unrelated batch's same-named file be treated
// as a partial download to resume.
func TestNewBatchDirStepsPastExistingFolders(t *testing.T) {
	dest := t.TempDir()
	now := time.Date(2026, 9, 22, 15, 30, 0, 0, time.Local)

	first := newBatchDir(dest, "spod", now)
	if want := filepath.Join(dest, "spod-20260922-1530"); first != want {
		t.Fatalf("want %s, got %s", want, first)
	}
	if err := os.MkdirAll(first, 0755); err != nil {
		t.Fatal(err)
	}
	if got, want := newBatchDir(dest, "spod", now), first+"-2"; got != want {
		t.Fatalf("second drain in the same minute: want %s, got %s", want, got)
	}
	// The two clusters drain into the same Downloads folder.
	if got, want := newBatchDir(dest, "spod-hpc4", now), filepath.Join(dest, "spod-hpc4-20260922-1530"); got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

// A retry must land in the folder it started in (the .spodget sidecar is
// there), but only while that is still a sane place to write.
func TestResumeBatchDirOnlyWhenUsable(t *testing.T) {
	tgt = targets["superpod"]
	t.Setenv("TMPDIR", t.TempDir())
	dest := t.TempDir()
	dir := filepath.Join(dest, "spod-20260922-1530")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	saveBatchDir(dir)
	if got := resumeBatchDir(dest); got != dir {
		t.Fatalf("interrupted drain should resume into %s, got %q", dir, got)
	}
	if got := resumeBatchDir(t.TempDir()); got != "" {
		t.Errorf("-o moved elsewhere: no partials there, got %q", got)
	}

	// Stale: nobody came back for a day, so a later push gets its own folder
	// instead of being filed under an old date.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(batchStatePath(), old, old); err != nil {
		t.Fatal(err)
	}
	if got := resumeBatchDir(dest); got != "" {
		t.Errorf("day-old claim should be forgotten, got %q", got)
	}

	// Folder deleted by hand — nothing left to resume into.
	saveBatchDir(dir)
	os.RemoveAll(dir)
	if got := resumeBatchDir(dest); got != "" {
		t.Errorf("missing folder, got %q", got)
	}

	// A settled drain forgets the folder, so the next push opens a new one.
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	saveBatchDir(dir)
	saveBatchDir("")
	if got := resumeBatchDir(dest); got != "" {
		t.Errorf("settled drain should forget the folder, got %q", got)
	}
}

// Everything spod installs ON a cluster has to name the command that drains
// THAT cluster's queue. A helper on HPC4 telling the user to run "spod recv"
// points them at SuperPod's queue, which drains fine and reports success while
// the HPC4 files it was about sit there untouched.
func TestClusterSideTextNamesItsOwnRecvCommand(t *testing.T) {
	defer func(prev *target) { tgt = prev }(tgt)

	tgt = targets["superpod"]
	spScript, spNote, spVer := pushHelperScript(), agentNote(), pushHelperVersion()
	tgt = targets["hpc4"]
	hpcScript, hpcNote, hpcVer := pushHelperScript(), agentNote(), pushHelperVersion()

	for name, text := range map[string]string{
		"superpod helper": spScript, "superpod note": spNote,
		"hpc4 helper": hpcScript, "hpc4 note": hpcNote,
	} {
		if strings.Contains(text, "__SPOD_RECV__") {
			t.Errorf("%s still has an unrendered placeholder", name)
		}
	}
	for name, text := range map[string]string{"helper": spScript, "note": spNote} {
		if !strings.Contains(text, "spod recv") {
			t.Errorf("superpod %s never names spod recv", name)
		}
	}
	// "spod hpc4 recv" does not contain "spod recv", so any hit is a leftover.
	for name, text := range map[string]string{"helper": hpcScript, "note": hpcNote} {
		if strings.Contains(text, "spod recv") {
			t.Errorf("hpc4 %s points at SuperPod's queue", name)
		}
		if !strings.Contains(text, "spod hpc4 recv") {
			t.Errorf("hpc4 %s never names spod hpc4 recv", name)
		}
	}
	// The marker gates re-deployment: same marker on both clusters would leave
	// whichever was installed first in place on the other one.
	if spVer == hpcVer {
		t.Errorf("both clusters share helper version %s", spVer)
	}
}
