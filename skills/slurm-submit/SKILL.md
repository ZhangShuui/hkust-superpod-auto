---
name: slurm-submit
description: "Generate and submit SLURM batch jobs on HKUST SuperPod with Pyxis/Enroot containers. Handles container mounts, env var injection, multi-GPU training, and bad node exclusion. Use for any non-interactive compute task."
argument-hint: "[description of what to run, e.g. 'train qwen3-8B with ROLL on 8 GPUs']"
---

# SLURM Job Submission

Generate and submit batch jobs on SuperPod via SSH. For interactive sessions, use `/hkust-superpod-session` instead.

## Connection

- SSH target: `superpod`
- SLURM account: `hdtaccuracy`
- Prerequisite: VPN running, SSH accessible

## Prerequisites Check

Before proceeding, verify:

1. **VPN**: `timeout 3 bash -c 'echo > /dev/tcp/superpod.ust.hk/22' 2>/dev/null`
2. **Cluster info** (optional but recommended): Check if `~/.cache/slurm-info/cluster-info.md` exists. If yes, read it to get:
   - Current problem nodes → `--exclude` list
   - Available partitions and limits
   - Container image paths
3. **No duplicate jobs**: `ssh superpod 'module load slurm 2>/dev/null; squeue -u $USER -o "%j %T" --noheader'` — warn if a job with the same name is already running.

## Step 1: Determine Job Parameters

From `$ARGUMENTS` and conversation context, determine:

| Parameter | How to decide | Default |
|-----------|--------------|---------|
| Job name | From task description | `job-<timestamp>` |
| Nodes | 1 unless multi-node training | `1` |
| GPUs per node | From model size / training config | `8` (full node) |
| Partition | `normal` for ≤48h, `preempt` for best-effort | `normal` |
| Time limit | From expected duration | `30:00:00` |
| Container image | Ask user if ambiguous | `/project/hdtaccuracy/images/roll.img` |
| Exclude nodes | From cluster-info problem nodes + known bad | `dgx-31,dgx-30` |

### Container Mount Patterns

Always include these base mounts:
```
--container-mounts /home/$USER:/home/$USER
```

For project-specific data, add project mounts:
```
--container-mounts /project/hdtaccuracy:/project/hdtaccuracy,/home/$USER:/home/$USER
```

### Environment Variables — Critical Gotcha

**Env vars from the login node do NOT automatically enter the Pyxis container.** You MUST explicitly pass them via `srun --export=VAR1,VAR2,...`.

Common env vars that need passing:

| Variable | Purpose | How to set on login node |
|----------|---------|--------------------------|
| `WANDB_API_KEY` | W&B logging | Must exist in login shell env |
| `MASTER_ADDR` | Distributed training | Auto-computed inside script |
| `MASTER_PORT` | Distributed training | Auto-computed inside script |
| `HF_TOKEN` | HuggingFace downloads | Must exist in login shell env |
| Custom (`CONFIG_NAME`, etc.) | Per-experiment config | Set before srun or via `--export` |

## Step 2: Generate sbatch Script

Use this template, filling in parameters from Step 1:

```bash
#!/bin/bash
#SBATCH --job-name=<JOB_NAME>
#SBATCH --nodes=<NODES>
#SBATCH --gpus-per-node=<GPUS>
#SBATCH --ntasks-per-node=1
#SBATCH --exclude=<EXCLUDE_NODES>
#SBATCH --time=<TIME_LIMIT>
#SBATCH --account=hdtaccuracy
#SBATCH --partition=<PARTITION>
#SBATCH --output=logs/%x_%j.out
#SBATCH --error=logs/%x_%j.err

srun --export=<ENV_VARS_COMMA_SEPARATED> \
    --container-image=<CONTAINER_IMAGE> \
    --container-mounts=<MOUNTS> \
    --no-container-mount-home \
    --container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1 \
    --container-workdir=<WORKDIR> \
    --container-writable \
    bash -c '
set -euo pipefail
cd <WORKDIR>

<USER_COMMANDS>
'
```

### Template Rules

- **Always** include `--no-container-mount-home` + explicit `/home/$USER` mount — prevents default mount conflicts with Pyxis.
- **Always** include `--container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1` — avoids Pyxis default mount issues.
- **Always** create `logs/` dir before submission.
- **Never** include `--container-save` in batch jobs — causes conflicts with concurrent jobs and wastes time on save.
- **Always** include `--output` and `--error` with `%x` (job name) and `%j` (job ID) for log traceability.
- If multi-node: keep `--ntasks-per-node=1` and handle `MASTER_ADDR`/`MASTER_PORT` inside the bash -c block.

### Multi-node Training Addition

For multi-node jobs, add inside `bash -c`:
```bash
export MASTER_ADDR=$(scontrol show hostname $SLURM_NODELIST | head -n1)
export MASTER_PORT=$((29500 + RANDOM % 100))
```

## Step 3: Upload and Submit

```bash
# Write script to local temp file
cat > /tmp/<JOB_NAME>.sh << 'JOBSCRIPT'
<generated script content>
JOBSCRIPT

# Ensure logs dir exists on remote
ssh superpod "mkdir -p <PROJECT_DIR>/logs"

# Upload the script
scp /tmp/<JOB_NAME>.sh superpod:<PROJECT_DIR>/<JOB_NAME>.sh

# Submit
ssh superpod "cd <PROJECT_DIR> && module load slurm 2>/dev/null && sbatch <JOB_NAME>.sh"
```

Capture the job ID from sbatch output (format: `Submitted batch job <JOBID>`).

## Step 4: Post-submission Check

```bash
ssh superpod "module load slurm 2>/dev/null && squeue -j <JOBID> -o '%i %j %P %T %M %l %D %N %r' --noheader"
```

Report to user:
- Job ID
- State (PENDING/RUNNING)
- Allocated nodes (if running)
- Log file paths: `logs/<JOB_NAME>_<JOBID>.out` and `.err`

Suggest: "Use `/slurm-monitor <JOBID>` to check progress."

## Troubleshooting

### Job stuck in PENDING
```bash
ssh superpod "module load slurm 2>/dev/null && squeue -j <JOBID> -o '%r' --noheader"
```
Common reasons:
- `Priority` — waiting for resources, be patient
- `Resources` — not enough GPUs, try fewer GPUs or `--partition preempt`
- `ReqNodeNotAvail` — excluded too many nodes or maintenance window, check `sinfo`

### Job failed immediately
```bash
ssh superpod "module load slurm 2>/dev/null && sacct -j <JOBID> --format=JobID,State,ExitCode,Reason%50"
ssh superpod "tail -50 <PROJECT_DIR>/logs/<JOB_NAME>_<JOBID>.err"
```
Common causes:
- Container image not found — check path with `ls -la`
- Env var not exported — add to `--export` list
- OOM — reduce batch size or request more GPUs
- Node hardware error — add node to `--exclude` and resubmit

### Need to cancel
```bash
ssh superpod "module load slurm 2>/dev/null && scancel <JOBID>"
```
