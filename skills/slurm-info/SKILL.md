---
name: slurm-info
description: "Gather and cache SLURM cluster info (partitions, GPUs, GRES types, QOS caps, accounts, problem nodes, container images) for HKUST SuperPod or HPC4. Run once to detect; returns cached result on subsequent runs. Re-run with --refresh to update."
argument-hint: "[superpod|hpc4] [--refresh]"
---

# SLURM Cluster Info

Collect a cluster's specs via SSH and save a structured reference document.
Works against **SuperPod** and **HPC4**, which differ substantially — see
`~/wkspace/hkust-superpod-auto/skills/CLUSTERS.md` for the side-by-side.

## Step 0: Pick the cluster

Read the leading word of `$ARGUMENTS`:

| Argument | Cluster | SSH alias | Cache file |
|----------|---------|-----------|------------|
| `hpc4` | HPC4 | `hpc4` | `~/.cache/slurm-info/hpc4.md` |
| `superpod` or absent | SuperPod | `superpod` | `~/.cache/slurm-info/superpod.md` |

Call the chosen alias `$CLUSTER` and hostname `$HOST` (`superpod.ust.hk` /
`hpc4.ust.hk`) for the rest of these steps. The two caches are separate —
never let one cluster's numbers answer a question about the other.

> Legacy note: an older version of this skill wrote a single
> `~/.cache/slurm-info/cluster-info.md`. If that file exists, treat it as the
> SuperPod cache and rename it to `superpod.md` on first run.

## Step 1: Check for cached doc

- If `$ARGUMENTS` contains `--refresh`, skip to step 3 (force re-gather).
- If the cache file exists and is less than 7 days old, read and display it. Stop here.
- If it exists but is older than 7 days, warn the user it may be stale and ask whether to refresh.

## Step 2: Verify VPN connectivity

```bash
timeout 3 bash -c 'echo > /dev/tcp/$HOST/22' 2>/dev/null && echo "OK" || echo "FAIL"
```

If FAIL, tell the user: "VPN not connected. Run `spod vpn` first."
If only HPC4 fails while SuperPod works, the cause is almost always the
split-tunnel gotcha — `VPN_HOSTS` in `.env` must list `hpc4.ust.hk`, and
`ip route get 143.89.184.3` must say `dev tun0`. Stop here either way.

## Step 3: Gather cluster data

One script serves both clusters; it self-detects which one it landed on.

```bash
ssh $CLUSTER 'bash -l -s' < ~/wkspace/hkust-superpod-auto/skills/slurm-info/scripts/gather-cluster-info.sh
```

Capture the full stdout. Sections are marked `=== SECTION ===`.

The `SITE_FEATURES` section is the one that decides which job template other
skills may use — it reports whether `savail`, `enroot`, `apptainer` and
`srun --container-image` exist. Do not infer these from the cluster name.

## Step 4: Parse and generate summary

Produce a polished markdown summary following this template. Fill
`<CLUSTER>` with `SuperPod` or `HPC4`.

```markdown
# <CLUSTER> Cluster Info

> Generated: <UTC timestamp> by `/slurm-info <cluster>` — login node <LOGIN_NODE>

## Site Features

| Feature | Present |
|---------|---------|
| `savail` | yes/no |
| `squota` | yes/no |
| enroot / Pyxis (`srun --container-image`) | yes/no |
| Apptainer / Singularity | yes/no |

**Job style**: `Pyxis container` (enroot present) or `conda / Apptainer` (absent).

## Partitions

| Partition | Nodes | GPUs/Node | GRES string | Memory/Node | CPUs | Walltime | Avail |
|-----------|-------|-----------|-------------|-------------|------|----------|-------|
| ... | ... | ... | `gpu:a30:4` | ... | ... | ... | ... |

Record the **exact GRES string** per partition — on HPC4 it is required
verbatim in `--gres=gpu:<type>:N`.

## Node Types

| Prefix | Count | CPUs | Memory | GPUs | State |
|--------|-------|------|--------|------|-------|

## Problem Nodes (drain/down/error)

| Node | State | Reason |
|------|-------|--------|

**Recommended --exclude**: `<comma-separated>` (or "none" — do not invent one)

## QOS Limits

| QOS | MaxWall | MaxTRES/User | MaxJobs | MaxSubmit |
|-----|---------|--------------|---------|-----------|

Note the walltime cap that applies to the user's default QOS — partitions
advertise `infinite`, but the QOS is what actually rejects the job.

## My Accounts

| Account | Partitions | QOS |
|---------|-----------|-----|

`--account` is **mandatory** on both clusters.

## Container Images / Environments

| Path | Size | Modified |
|------|------|----------|

Plus available conda envs.

## Current Jobs

| JobID | Name | Partition | State | Runtime | Nodes | GPUs |
|-------|------|-----------|-------|---------|-------|------|

## Quick Reference

<Emit the interactive-session snippet for THIS cluster only — see below.>

### Common commands
- `squeue -u $USER` — my jobs
- `sinfo` — node status
- `squota` — usage
- `scancel <jobid>` — cancel job
- SuperPod only: `savail -p normal` — GPU availability
```

### Quick-reference snippet — SuperPod (Pyxis)

```bash
srun --account <ACCOUNT> \
     --partition normal --nodes 1 --gpus 2 \
     --time 02:00:00 \
     --container-image /project/<ACCOUNT>/images/roll.img \
     --no-container-mount-home \
     --container-mounts /home/$USER:/home/$USER \
     --container-workdir /home/$USER \
     --container-remap-root \
     --container-writable \
     --container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1 \
     --container-save /project/<ACCOUNT>/images/roll.img \
     --pty bash
```

### Quick-reference snippet — HPC4 (no Pyxis)

```bash
srun --account <ACCOUNT> \
     --partition gpu-a30 --nodes 1 \
     --gres=gpu:a30:2 --cpus-per-task 16 \
     --time 02:00:00 \
     --pty bash
# then, inside the allocation:
#   conda activate <env>
#   # or: apptainer exec --nv /path/to/image.sif python train.py
```

Never emit the Pyxis flags for HPC4 — there is no enroot there and every
`--container-*` flag except a bare `--container` is a hard error.

## Step 5: Save the summary

```bash
mkdir -p ~/.cache/slurm-info
```

Write to `~/.cache/slurm-info/<cluster>.md`.

## Step 6: Display to user

Show the full summary and the saved path. If the user asked about "the
cluster" without naming one and both caches exist, say which one you read.

## Important

- Do NOT output raw script data. Only the polished summary.
- "Problem Nodes" and "Current Jobs" are point-in-time — include the timestamp.
- The "Recommended --exclude" list feeds `/slurm-submit`. If nothing is
  drained, say "none" rather than carrying over a stale list; the historical
  `dgx-31,dgx-30` exclude is no longer accurate.
- Physical cores vs logical CPUs: SLURM `--cpus-per-task` counts physical cores.
