---
name: hkust-superpod-session
description: Manage persistent interactive sessions on HKUST SuperPod and HPC4 (VPN + SSH + tmux + SLURM allocation). Handles VPN check, tmux session reuse, SLURM resource allocation, and the per-cluster container/conda story.
---

# HKUST Cluster Session Management

Interactive work on **SuperPod** and **HPC4**. Despite the skill's name it
drives both — they share one VPN and one CLI (`spod`), but differ in scheduler
conventions and container support.

Full side-by-side: `~/wkspace/hkust-superpod-auto/skills/CLUSTERS.md`.

## Prerequisites

- VPN running: **`spod vpn`** (check with `spod vpn status`).
  Do not call `hkust-vpn.py` directly — `spod` owns the VPN lifecycle,
  auto-reconnects, and running a second manager makes both flap.
- `spod` on `$PATH` (`~/.local/bin/spod`; rebuild with
  `cd cmd/spod && go build -o ~/.local/bin/spod .`).
- MCP server `terminal` for long-lived interactive sessions.

## Connection details

| | SuperPod | HPC4 |
|---|---|---|
| Host | `superpod.ust.hk` | `hpc4.ust.hk` |
| SSH alias | `superpod` | `hpc4` |
| Attach | `spod` | `spod hpc4` |
| Login node(s) | `slogin-01`, `slogin-02` | `login4` |
| Account | from `.env` `SUPERPOD_USER` | from `.env` `HPC4_USER` |
| SLURM account | `hdtaccuracy` (also `visworld01`, `gzmcagent`) | `kanichen` (also `migrate`) |

Never hardcode the login name in a command — the `superpod`/`hpc4` aliases in
`~/.ssh/config` already carry it (auto-synced by `spod` on every run), and
`$USER` expands correctly on the remote side.

The two clusters are **independent accounts** with independent tunnels, relays
and tmux sessions. `spod` and `spod hpc4` can run side by side.

## Session lifecycle

### Step 1: Check VPN

```bash
spod vpn status
```

Or directly (ping is not proof — it can succeed without VPN routing):

```bash
timeout 3 bash -c 'echo > /dev/tcp/superpod.ust.hk/22' && echo OK || echo FAIL
timeout 3 bash -c 'echo > /dev/tcp/hpc4.ust.hk/22'     && echo OK || echo FAIL
```

If SuperPod works and HPC4 times out, it is the split-tunnel gotcha:
`VPN_HOSTS` in `.env` must include `hpc4.ust.hk`, and
`ip route get 143.89.184.3` must say `dev tun0`. `spod hpc4 <anything>` repairs
the route in place; no VPN restart needed.

If both fail: `spod vpn`.

### Step 2: Attach to a session

Prefer `spod`, which attaches to an existing remote tmux session or creates
one — the session (and any `srun` allocation inside it) survives a dropped
connection.

```bash
spod              # SuperPod: pick / create a session
spod hpc4         # HPC4: same, independent session set
spod ls           # list SuperPod sessions
spod hpc4 ls      # list HPC4 sessions
```

Via `mcp__terminal__create_session`, use `spod` as the command rather than a
raw `ssh` — for example `command: spod`, `args: ["hpc4"]`, `name: hpc4`.

**Reuse one session for the whole task.** Opening a fresh SSH connection per
command hammers the login node; on HPC4 a burst of connections triggers
`kex_exchange_identification` resets (stop for ~2 minutes and it clears), and
on SuperPod repeated failed logins can lock the ITSC account, which takes the
VPN down with it.

MOTD is suppressed on SuperPod via `~/.hushlogin`.

### Step 3: Load SLURM (SuperPod only)

```
send_command: module load slurm
```

HPC4 has slurm in `/usr/bin` — no module needed. Its `module` is Lmod over
Spack (`module avail` for cuda/gcc/miniconda3/openmpi/…).

### Step 4: Allocate a compute node

Check availability first (step 5 below), then:

#### SuperPod — Pyxis/enroot container

```bash
LOCAL_IMAGE="/project/hdtaccuracy/images/roll.img"

srun --account hdtaccuracy \
     --partition normal --nodes 1 --gpus 2 \
     --time 04:00:00 \
     --container-image "$LOCAL_IMAGE" \
     --no-container-mount-home \
     --container-mounts /home/$USER:/home/$USER \
     --container-workdir /home/$USER \
     --container-remap-root \
     --container-writable \
     --container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1 \
     --container-save "$LOCAL_IMAGE" \
     --pty bash
```

Add `--exclude=<nodes>` only for nodes actually drained right now — check
`sinfo`, do not carry over a hardcoded list. (`dgx-31,dgx-30`, which older
versions of this skill always excluded, are currently healthy.)

#### HPC4 — no Pyxis; conda or Apptainer

```bash
srun --account kanichen \
     --partition gpu-a30 --nodes 1 \
     --gres=gpu:a30:2 --cpus-per-task 16 \
     --time 04:00:00 \
     --pty bash

# then, inside the allocation:
source ~/anaconda3/etc/profile.d/conda.sh && conda activate <env>
# or: apptainer exec --nv <image>.sif bash
```

**Two hard rules on HPC4:**

1. **`--gres=gpu:<type>:N`, never a bare `--gpus`.** With `--gpus` the site
   plugin prints `Notice: CPUs reduced to 0 (Maximum allowed for 0 GPU(s))`
   and gives you a *GPU-less* allocation that still starts. Types: `a30`,
   `l20`, `4090d`, `rtx5880`, `3090`, `6000ada` — matched to the partition
   (`gpu-a30`, `gpu-l20`, `gpu-rtx4090d`, `gpu-rtx5880`, `hpc3gpu-math1`,
   `hpc3gpu-math2`).
2. **No `--container-*` flags.** There is no enroot. Every Pyxis flag above is
   an error here; `srun --container` on HPC4 means an OCI bundle directory.

**Both clusters: `--account` is mandatory.** Omitting it fails with
`Please kindly add the --account … SLURM flag`.

**Walltime is capped by QOS, not the partition.** Partitions advertise
`infinite`; GPU QOS caps at 3 days on both clusters (HPC4 CPU QOS: 5 days,
`debug`: 4 hours). Over the cap fails at submit with
`QOSMaxWallDurationPerJobLimit`.

`srun` may block for a long time waiting for allocation — use
`timeout_ms=60000` and then poll `read_output` until the prompt appears.

### Step 5: Work on the compute node

The session is persistent — working directory, environment and running
processes survive across commands. On SuperPod you have root inside the
container; on HPC4 you are a normal user in the job step.

## Timeout guidelines

| Operation | timeout_ms |
|-----------|-----------|
| Simple commands (ls, cat, echo) | 5000 |
| Medium (pip install, git) | 30000 |
| Long (compilation, large clone) | 60000 |
| `srun` — SuperPod, loading a container | **2–5 min** — must poll |
| `srun` — HPC4, no container to load | usually seconds once resources free |
| Training runs | poll with `read_output` |

### Handling srun (critical on SuperPod)

Loading an enroot image takes 2–5 minutes and the MCP timeout maxes at 60s:

1. `send_command` with `timeout_ms=60000` — expect `is_complete: false`
2. Wait ~30s, then `read_output`
3. Repeat until a prompt appears (`root@dgx-XX:` or `<user>@dgx-XX:`)
4. Do NOT assume failure on the first empty `read_output` — be patient

HPC4 has no image to load, so a slow `srun` there means the queue, not a
container. Check `squeue -u $USER -o "%i %T %r"` for the pending reason.

### Handling problematic nodes

Signs: `srun` hangs far past 5 minutes, container/enroot errors, GPU errors on
startup.

**Do NOT keep retrying the same command.** Instead:

1. `send_control` `ctrl+c`
2. Note which node was allocated
3. Add it to `--exclude` and retry
4. If a node is consistently broken, mention it — but keep exclude lists
   derived from `sinfo`, not baked into this file

## Checking availability before `srun`

```bash
# Both clusters
sinfo                              # node states (idle/alloc/mix/drain)
squeue -u $USER                    # my jobs — make sure nothing is already allocated
squota                             # usage (layout differs per cluster)

# SuperPod only
savail -p normal                   # GPU availability wrapper

# HPC4 (no savail — read it off sinfo)
sinfo -p gpu-a30,gpu-l20,gpu-rtx4090d,gpu-rtx5880 -o "%P %N %T %G %C" --noheader
```

`idle` = fully free, `mixed` = some GPUs left, `allocated` = none.

## Important rules

- **NEVER run GPU/compute workloads on a login node** — SuperPod kills them and
  you can get banned. Treat HPC4's `login4` the same way.
- **Login nodes are for**: editing files, git, submitting jobs, checking queue.
- **Always `srun`** (or `/slurm-submit`) before anything heavy.
- **Keep the session alive** — the allocation dies when the shell exits. This is
  why `spod`'s tmux sessions matter.
- SuperPod container changes persist via `--container-save`; HPC4 has no
  equivalent, so persist work in `$HOME` / `/project` or a rebuilt `.sif`.
- Check `sinfo` for drained nodes *before* launching, not after a failure.

## Compute node quick reference

```bash
nvidia-smi                      # GPU status — verify you actually got GPUs
python train.py                 # run training
pip install <package>           # SuperPod: root in container. HPC4: use conda env
```

On HPC4, run `nvidia-smi` immediately after allocation. An empty device list
means the `--gres` was wrong (see the `--gpus` gotcha above).

## Troubleshooting

### Cannot reach the cluster
```
ssh: connect to host … port 22: Connection timed out
```
→ `spod vpn status`, then `spod vpn`. If only HPC4 fails, check `VPN_HOSTS`
and `ip route get 143.89.184.3`.

### `srun` pending forever
→ `squeue -u $USER -o "%i %T %r"` for the reason; `savail` (SuperPod) or
`sinfo` (HPC4) for capacity. Try fewer GPUs, another GPU partition, or
`--partition preempt` on SuperPod.

### Session died
→ Reattach with `spod` / `spod hpc4`; the tmux session and its allocation are
usually still there. If the allocation is gone, re-run `srun`. SuperPod
container state is preserved in the `.img`.

### Permission denied in container (SuperPod)
→ Ensure `--container-remap-root` is present.

### `--container-image: unrecognized option` (HPC4)
→ You pasted the SuperPod template. Use the HPC4 form in Step 4.

### Claude / Codex on the cluster cannot reach its API
→ Both clusters need the reverse tunnel + relay: `spod tunnel` /
  `spod hpc4 tunnel`. The remote `~/.bashrc` wraps only `claude` and `codex`
  with the proxy; git/pip/npm go direct. HPC4 reaches the internet directly but
  the AI API endpoints are 403 there, so the tunnel is required, not optional.

### Downloads must not go through the relay

Keep it that way. The relay tunnels back to the local Clash and out over the
VPN, where a single flow tops out near 255 KB/s, so a container pull, a big
`pip install` or a dataset download sent through it is both glacially slow and
the quickest way to starve `claude`/`codex` of the channel they need.

A plain shell is already clean, but `srun` defaults to `--export=ALL`, so
whatever the submitting shell carries follows you onto the compute node. Before
anything that downloads, be explicit:

```bash
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy
```

Check with `env | grep -i proxy` if a download is inexplicably slow.

## Related skills

- `/slurm-info [cluster]` — cache current partitions, GRES strings, QOS caps
- `/slurm-monitor [cluster] [jobid]` — read-only queue/log/node inspection
- `/slurm-submit [cluster] <task>` — generate and submit a batch job
