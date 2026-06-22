# spod rider — 多登录节点故障转移登录 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 spod 加一个 `spod rider` 子命令，让借用 provider SOCKS 的同事自动连到“relay 活的”SuperPod 登录节点（slogin-01 挂时自动跟到 slogin-02），无需手动改 IP。

**Architecture:** 复用 spod 现有的 rider 基建（`SPOD_NO_VPN` / `SUPERPOD_SSH_PROXY` / `ensureSSHConfig` / `ensurePorts` / `cmdInteractive`）。新增一个纯逻辑函数 `pickActiveNode`（可单元测，注入探测函数）、一个真实探测函数 `probeRiderNode`（ssh 进候选节点 `curl 127.0.0.1:<relayPort>` 判 `405`）、一个 `riderConnect` 组装器，以及 `main()` switch 里一个 `case "rider"`。

**Tech Stack:** Go 1.23（标准库 only，无外部依赖）；`ssh` + `nc`（SOCKS ProxyCommand）；`go test`。

## Global Constraints

- Go 1.22+；`package main`；**无外部 Go 依赖**（仅标准库）—— 摘自 CLAUDE.md。
- 所有改动集中在 `cmd/spod/main.go`（+ 新建 `cmd/spod/main_test.go`）。
- 不改 provider 侧行为：无参 `spod` 及其他子命令必须保持原样。
- relay 端口算法固定 `18000 + uid%1000`（与现有 `ensurePorts` 一致），探测命令在远端自算，不在本地依赖 `relayPort`。
- 输出统一用现有 helper：`info/ok/warn/fail`（写 stderr）。

---

## File Structure

- **Modify** `cmd/spod/main.go`：
  - 新增 `pickActiveNode()`（纯逻辑，选活节点）
  - 新增 `probeRiderNode()`（真实 ssh+curl 探测）
  - 新增 `riderConnect()`（组装：校验→探测→设 host→连接）
  - `main()` switch 新增 `case "rider"`
  - `cmdHelp()` 增一行 rider 说明
- **Create** `cmd/spod/main_test.go`：`pickActiveNode` 的单元测（注入 fake probe）
- **Modify** `.env.example`：新增 rider 侧配置注释块

---

### Task 1: `pickActiveNode` 选活节点逻辑（TDD）

**Files:**
- Modify: `cmd/spod/main.go`（在 `ensurePorts()` 之后，约 `main.go:460` 后新增函数）
- Test: `cmd/spod/main_test.go`（新建）

**Interfaces:**
- Produces: `func pickActiveNode(candidates []string, retries int, delay time.Duration, probe func(node string) bool) string` —— 按序对每个候选重试 `retries` 次，任一次 `probe(node)==true` 即返回该 node；全部失败返回 `""`。`delay` 是同一节点两次探测间的迟滞（测试传 0）。

- [ ] **Step 1: 写失败测试**

新建 `cmd/spod/main_test.go`：

```go
package main

import (
	"testing"
	"time"
)

func TestPickActiveNode_FirstAlive(t *testing.T) {
	probe := func(node string) bool { return node == "10.0.0.1" }
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, 0, probe)
	if got != "10.0.0.1" {
		t.Fatalf("want 10.0.0.1, got %q", got)
	}
}

func TestPickActiveNode_FailoverToSecond(t *testing.T) {
	probe := func(node string) bool { return node == "10.0.0.2" }
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, 0, probe)
	if got != "10.0.0.2" {
		t.Fatalf("want 10.0.0.2 (failover), got %q", got)
	}
}

func TestPickActiveNode_NoneAlive(t *testing.T) {
	probe := func(node string) bool { return false }
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, 0, probe)
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
	got := pickActiveNode([]string{"10.0.0.1", "10.0.0.2"}, 3, 0, probe)
	if got != "10.0.0.1" {
		t.Fatalf("want 10.0.0.1 after retry, got %q", got)
	}
	if calls != 3 {
		t.Fatalf("want 3 probes on first node, got %d", calls)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `cd cmd/spod && go test ./... -run TestPickActiveNode -v`
Expected: 编译失败 `undefined: pickActiveNode`

- [ ] **Step 3: 实现 `pickActiveNode`**

在 `cmd/spod/main.go` 的 `ensurePorts()` 函数之后新增：

```go
// pickActiveNode returns the first candidate node whose relay is live, retrying
// each node up to `retries` times (with `delay` between attempts) before moving
// on. Returns "" when no candidate's relay is reachable. `probe` is injected so
// the selection logic is unit-testable without real SSH.
func pickActiveNode(candidates []string, retries int, delay time.Duration, probe func(node string) bool) string {
	for _, node := range candidates {
		for attempt := 0; attempt < retries; attempt++ {
			if probe(node) {
				return node
			}
			if attempt < retries-1 {
				time.Sleep(delay)
			}
		}
	}
	return ""
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `cd cmd/spod && go test ./... -run TestPickActiveNode -v`
Expected: PASS（4 个用例全过）

- [ ] **Step 5: 提交**

```bash
git add cmd/spod/main.go cmd/spod/main_test.go
git commit -m "feat(spod): add pickActiveNode failover selection logic"
```

---

### Task 2: 真实探测 + `riderConnect` + 子命令接线

**Files:**
- Modify: `cmd/spod/main.go`（新增 `probeRiderNode`、`riderConnect`；`main()` switch 加 `case "rider"`，约 `main.go:2311` 的 `case "ssh"` 之后）

**Interfaces:**
- Consumes: `pickActiveNode`（Task 1）；现有 `envOr`、`ensureSSHConfig`、`cmdInteractive`、`attachOrCreate`、`fullName`、`info/ok/fail`。
- Produces: `func probeRiderNode(node string) bool`；`func riderConnect(sessionArg string)`；`spod rider [session]` 子命令。

- [ ] **Step 1: 实现 `probeRiderNode`**

在 `cmd/spod/main.go` 的 `pickActiveNode` 之后新增。探测命令在**远端**自算 relay 端口（与 `ensurePorts` 同算法），所以本地无需先知道端口：

```go
// probeRiderNode reports whether `node` has a LIVE relay: it ssh's into the node
// (through the provider's SOCKS, if configured) and asks the local relay to reach
// OpenAI. The remote self-computes its relay port (18000+uid%1000) so this is
// self-contained. A 405 (relay reached OpenAI) means live; anything else = dead.
func probeRiderNode(node string) bool {
	user := envOr("SUPERPOD_USER", "")
	proxy := os.Getenv("SUPERPOD_SSH_PROXY")
	remote := `p=$((18000 + $(id -u)%1000)); ` +
		`curl -sf -x http://127.0.0.1:$p --max-time 8 -o /dev/null ` +
		`-w '%{http_code}' https://chatgpt.com/backend-api/codex/responses`
	sshArgs := []string{
		"-o", "ConnectTimeout=8",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/spod-rider-%r@%h:%p",
		"-o", "ControlPersist=60",
	}
	if proxy != "" {
		sshArgs = append(sshArgs, "-o", fmt.Sprintf("ProxyCommand=nc -X 5 -x %s %%h %%p", proxy))
	}
	target := node
	if user != "" {
		target = user + "@" + node
	}
	sshArgs = append(sshArgs, target, remote)
	out, err := exec.Command("ssh", sshArgs...).Output()
	return err == nil && strings.TrimSpace(string(out)) == "405"
}
```

- [ ] **Step 2: 实现 `riderConnect`**

在 `probeRiderNode` 之后新增：

```go
// riderConnect connects as a "rider": it borrows the provider's VPN/tunnel/relay
// (SPOD_NO_VPN), probes the candidate login nodes for the one whose relay is live,
// points the `superpod` ssh alias at it, then hands off to the normal connect path.
func riderConnect(sessionArg string) {
	// Rider has no local tun0 and must not build its own tunnel/socks — it adopts
	// the provider's. SPOD_NO_VPN short-circuits ensureVPN(); we simply never call
	// ensureTunnel()/ensureSocks() here.
	os.Setenv("SPOD_NO_VPN", "1")

	if os.Getenv("SUPERPOD_SSH_PROXY") == "" {
		fail("rider 模式需经 provider 的 SOCKS：请在 .env 配 SUPERPOD_SSH_PROXY=<provider-ip>:1080")
		os.Exit(1)
	}
	if envOr("SUPERPOD_USER", "") == "" {
		fail("请在 .env 配 SUPERPOD_USER（SuperPod 账号，如 szhangfa）")
		os.Exit(1)
	}

	candidates := strings.Fields(os.Getenv("SUPERPOD_HOSTS"))
	if len(candidates) == 0 {
		candidates = []string{"10.22.4.12", "10.22.4.13"} // slogin-01 优先，slogin-02 兜底
	}

	info(fmt.Sprintf("探测活节点（relay 上游存活）：%s", strings.Join(candidates, ", ")))
	node := pickActiveNode(candidates, 3, 2*time.Second, probeRiderNode)
	if node == "" {
		fail("活节点未就绪（provider 隧道可能在重连），稍等重试")
		os.Exit(1)
	}
	ok(fmt.Sprintf("活节点：%s（relay 通）", node))

	// Point the `superpod` alias at the chosen node, then reuse the normal path.
	os.Setenv("SUPERPOD_HOST", node)
	ensureSSHConfig()

	if sessionArg == "" {
		cmdInteractive()
	} else {
		attachOrCreate(fullName(sessionArg))
	}
}
```

- [ ] **Step 3: 接线 `case "rider"`**

在 `cmd/spod/main.go` 的 `main()` switch 中，`case "ssh":` 块之后、`case "":` 之前插入：

```go
	case "rider":
		sessionArg := ""
		if len(args) > 1 {
			sessionArg = args[1]
		}
		riderConnect(sessionArg)
```

- [ ] **Step 4: 编译 + 跑全部单元测**

Run: `cd cmd/spod && go build -o ~/.local/bin/spod . && go test ./... -v`
Expected: build 成功（exit 0）；`TestPickActiveNode*` 全 PASS

- [ ] **Step 5: 本机模拟 rider 集成验证**

provider 本机就有 SOCKS（`0.0.0.0:1080`），可用 `127.0.0.1:1080` 扮 rider 复现同事路径：

Run:
```bash
SUPERPOD_SSH_PROXY=127.0.0.1:1080 SUPERPOD_USER=szhangfa \
SUPERPOD_HOSTS="10.22.4.12 10.22.4.13" \
spod rider --dry 2>&1 | head
```
（注：`--dry` 不存在，此步用真实 `spod rider` 但在出现 tmux 菜单/连接时 Ctrl-C 即可。）实际验证：
```bash
SUPERPOD_SSH_PROXY=127.0.0.1:1080 SUPERPOD_USER=szhangfa \
SUPERPOD_HOSTS="10.22.4.12 10.22.4.13" spod rider
```
Expected: 打印 `探测活节点 ...` → `活节点：10.22.4.12（relay 通）`，随后进入 tmux 会话菜单/会话（确认落到 slogin-01）。验证完 `exit`/`Ctrl-C` 退出。

反向用例（首选不可用应切次选）：
```bash
SUPERPOD_SSH_PROXY=127.0.0.1:1080 SUPERPOD_USER=szhangfa \
SUPERPOD_HOSTS="10.22.99.99 10.22.4.12" spod rider
```
Expected: 首选 `10.22.99.99` 探测失败（重试 3 次），切到 `10.22.4.12` 并连入。

- [ ] **Step 6: 提交**

```bash
git add cmd/spod/main.go
git commit -m "feat(spod): add 'spod rider' that auto-connects to the live-relay node"
```

---

### Task 3: 帮助文本 + `.env.example` + 文档

**Files:**
- Modify: `cmd/spod/main.go`（`cmdHelp()` 增一行）
- Modify: `.env.example`（rider 配置块）

**Interfaces:**
- Consumes: 无新接口；仅文档/帮助文本。

- [ ] **Step 1: `cmdHelp()` 增加 rider 行**

定位 `cmdHelp()` 里命令列表（如含 `{"spod ssh", "裸 SSH（不用 tmux）"}` 的那段，约 `main.go:2206`），在合适位置加一行：

```go
		{"spod rider", "借用 provider 的 SOCKS，自动连到 relay 活的登录节点"},
```
（按该列表实际的结构体/切片字面量格式对齐；若是 `[][2]string` 就用 `{"spod rider", "..."}`。）

- [ ] **Step 2: `.env.example` 增加 rider 配置块**

在 `.env.example` 末尾追加：

```bash
# ── Rider 模式（借用他人 SuperPod 隧道）──────────────────────────
# 同事用：经 provider 暴露的 SOCKS 连到 SuperPod，spod rider 自动选 relay 活的节点。
# SUPERPOD_SSH_PROXY=192.168.77.179:1080   # provider 的 SOCKS（必填）
# SUPERPOD_USER=szhangfa                    # SuperPod 账号（必填）
# SUPERPOD_HOSTS="10.22.4.12 10.22.4.13"    # 候选登录节点，留空用内置默认
```

- [ ] **Step 3: 编译确认无语法错误**

Run: `cd cmd/spod && go build -o ~/.local/bin/spod . && spod --help | grep rider`
Expected: build 成功；`--help` 输出含 `spod rider` 行

- [ ] **Step 4: 提交**

```bash
git add cmd/spod/main.go .env.example
git commit -m "docs(spod): document 'spod rider' in help and .env.example"
```

---

## Self-Review

**1. Spec coverage**（对照 design doc）：
- 子命令 `spod rider` → Task 2 Step 3 ✓
- `pickActiveNode` 探测+迟滞重试 → Task 1 ✓
- 判活用 `405`（relay 端到端）→ Task 2 Step 1（`probeRiderNode`）✓
- 候选 env / 默认 `10.22.4.12/.13` → Task 2 Step 2 ✓
- rider 跳过 VPN/隧道/socks（`SPOD_NO_VPN`）→ Task 2 Step 2 ✓
- 全不活报错不乱连 → Task 2 Step 2 ✓
- SSH 复用（ControlMaster/Path）→ Task 2 Step 1（探测）+ 现有 `ensureSSHConfig`（登录）✓
- provider 零改动 → 仅新增 case，其他不动 ✓
- rider 配置汇总 → Task 3 `.env.example` ✓

**2. Placeholder scan:** Task 2 Step 5 的 `--dry` 已标注“不存在”，仅作说明；实际验证命令是真实可跑的。其余步骤均为完整代码/命令，无 TBD。

**3. Type consistency:** `pickActiveNode(candidates []string, retries int, delay time.Duration, probe func(string) bool) string` 在 Task 1 定义、Task 2 `riderConnect` 调用，签名一致；`probeRiderNode(node string) bool` 作为 `probe` 传入，类型匹配 ✓。
