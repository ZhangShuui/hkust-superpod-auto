---
name: slurm-info
description: "Gather and cache HKUST SuperPod SLURM cluster info (partitions, GPUs, nodes, QOS, problem nodes, container images). Run once to detect; returns cached result on subsequent runs. Re-run with --refresh to update."
---

# SLURM Cluster Info

Collect SuperPod cluster specs via SSH and save a structured reference document.

## Connection

- SSH target: `superpod` (alias from ~/.ssh/config, auto-synced by `spod`)
- User: `szhangfa`
- Prerequisite: VPN must be running (`spod vpn status` to check)

## Steps

### 1. Check for cached doc

Look for `~/.cache/slurm-info/cluster-info.md`.

- If `$ARGUMENTS` contains `--refresh`, skip to step 3 (force re-gather).
- If the file exists and is less than 7 days old, read and display it. Stop here.
- If the file exists but is older than 7 days, warn the user it may be stale and ask whether to refresh.

### 2. Verify VPN connectivity

```bash
timeout 3 bash -c 'echo > /dev/tcp/superpod.ust.hk/22' 2>/dev/null && echo "OK" || echo "FAIL"
```

If FAIL, tell the user: "VPN not connected. Run `spod vpn` first." Stop here.

### 3. Gather cluster data

Locate the gather script relative to this skill file. The script is at `skills/slurm-info/scripts/gather-cluster-info.sh` in the repo root (`~/wkspace/hkust-superpod-auto`).

Run it on SuperPod via SSH:

```bash
ssh superpod 'bash -l -s' < ~/wkspace/hkust-superpod-auto/skills/slurm-info/scripts/gather-cluster-info.sh
```

Capture the full stdout.

### 4. Parse and generate summary

Parse the raw output (structured with `=== SECTION ===` markers) and produce a polished markdown summary. Follow this template:

```markdown
# SuperPod Cluster Info

> Generated: <UTC timestamp> by `/slurm-info`

## Partitions

| Partition | Nodes | GPUs/Node | GPU Type | Memory/Node | Max Walltime | Oversubscribe |
|-----------|-------|-----------|----------|-------------|--------------|---------------|
| ... | ... | ... | ... | ... | ... | ... |

## Node Types

| Prefix | Count | CPUs | Memory | GPUs | State |
|--------|-------|------|--------|------|-------|
| ... | ... | ... | ... | ... | ... |

## Problem Nodes (drain/down/error)

| Node | State | Reason |
|------|-------|--------|
| ... | ... | ... |

**Recommended --exclude**: `dgx-XX,dgx-YY,...` (all problem nodes)

## QOS Limits

| QOS | Max Walltime | Max TRES/User | Max Jobs | Max Submit |
|-----|-------------|---------------|----------|------------|
| ... | ... | ... | ... | ... |

## My Account

- Account: `hdtaccuracy`
- Available partitions and QOS from association

## Container Images

| Path | Size | Modified |
|------|------|----------|
| ... | ... | ... |

## Current Jobs

| JobID | Name | Partition | State | Runtime | Nodes | GPUs |
|-------|------|-----------|-------|---------|-------|------|
| ... or "No running jobs" |

## Quick Reference

### Interactive session (srun)
```bash
srun --account hdtaccuracy \
     --exclude=<PROBLEM_NODES> \
     --partition normal --nodes 1 --gpus 2 \
     --container-image /project/hdtaccuracy/images/roll.img \
     --no-container-mount-home \
     --container-mounts /home/$USER:/home/$USER \
     --container-workdir /home/$USER \
     --container-remap-root \
     --container-writable \
     --container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1 \
     --container-save /project/hdtaccuracy/images/roll.img \
     --pty bash
```

### Batch submission (sbatch)
```bash
sbatch job.sh
```

### Common commands
- `squeue -u $USER` — my jobs
- `sinfo` — node status
- `savail -p normal` — GPU availability
- `squota` — CPU/GPU hours usage
- `scancel <jobid>` — cancel job
```

### 5. Save the summary

```bash
mkdir -p ~/.cache/slurm-info
```

Write the summary to `~/.cache/slurm-info/cluster-info.md`.

### 6. Display to user

Show the full summary and tell the user the file path where it was saved.

## Important

- Do NOT output raw script data. Only output the polished summary.
- The "Problem Nodes" and "Current Jobs" sections are point-in-time data — include them but note the timestamp.
- The "Recommended --exclude" list is critical — other skills (`/slurm-submit`) will consume this.
- Physical cores vs logical CPUs: SLURM `--cpus-per-task` counts physical cores. Note this if relevant.
