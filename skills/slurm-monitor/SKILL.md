---
name: slurm-monitor
description: "Monitor SLURM jobs on HKUST SuperPod or HPC4 — check queue, read logs, inspect node status, track GPU usage. Read-only operations, safe to run anytime."
argument-hint: "[superpod|hpc4] [jobid] [--full] [--tail N] [--nodes] [--quota]"
allowed-tools: Read, Glob, Grep, Bash(ssh:*), Bash(timeout:*), Bash(cat:*), Bash(spod:*)
---

# SLURM Job Monitor

Monitor jobs and cluster status via SSH. All operations are **read-only**.

## Step 0: Pick the cluster

Read the leading word of `$ARGUMENTS`:

| Argument | Cluster | SSH alias (`$CLUSTER`) | Hostname |
|----------|---------|------------------------|----------|
| `hpc4` | HPC4 | `hpc4` | `hpc4.ust.hk` |
| `superpod` or absent | SuperPod | `superpod` | `superpod.ust.hk` |

Everything below uses `$CLUSTER`. The clusters have separate accounts, jobs
and queues — never report one's numbers under the other's name, and always
label the output with which cluster it came from.

Two mechanical differences show up throughout:

- **`module load slurm`** is needed on SuperPod, is a no-op on HPC4 (slurm
  lives in `/usr/bin`). Keeping `module load slurm 2>/dev/null` in the
  command is harmless on both, so the snippets below all carry it.
- **`savail` exists only on SuperPod.** On HPC4, derive availability from
  `sinfo` instead. Full table in
  `~/wkspace/hkust-superpod-auto/skills/CLUSTERS.md`.

Prerequisite for both: VPN running (`spod vpn status`).

## Parse remaining arguments

| Input | Mode |
|-------|------|
| `<jobid>` (numeric) | Monitor specific job |
| `--nodes` | Show node status and availability |
| `--quota` | Show CPU/GPU hours usage |
| (empty) | Overview: my jobs + recent completed |

Options:
- `--tail N` — show last N lines of log (default: 30)
- `--full` — show complete log output

## Mode 1: Overview (no arguments)

### 1a. Check running/pending jobs

```bash
ssh $CLUSTER 'module load slurm 2>/dev/null; squeue -u $USER -o "%i %j %P %T %M %l %D %C %b %N" --noheader'
```

Display as a formatted table:

| JobID | Name | Partition | State | Runtime | Timelimit | Nodes | CPUs | GPUs | NodeList |
|-------|------|-----------|-------|---------|-----------|-------|------|------|----------|

If the GPU column is empty on an HPC4 job that was meant to use GPUs, that is
the `--gpus` gotcha, not a display bug: HPC4 only counts GPUs requested via
`--gres=gpu:<type>:N`, so a `--gpus`-only job really was scheduled with none.

### 1b. Check recently completed jobs (last 24h)

```bash
ssh $CLUSTER 'module load slurm 2>/dev/null; sacct -u $USER --starttime now-1day --format=JobID%10,JobName%20,Partition%14,State%12,ExitCode%8,Elapsed%12,NNodes%6,NCPUS%6,TRESUsageInTot%40 --noheader | head -20'
```

Highlight:
- **COMPLETED** — success
- **FAILED** / **CANCELLED** — needs attention
- **TIMEOUT** — hit walltime. On both clusters the binding limit is usually the
  **QOS** MaxWall, not the partition (partitions advertise `infinite`).
- **OUT_OF_MEMORY** — OOM, suggest reducing batch size

### 1c. Quick cluster health

```bash
ssh $CLUSTER 'module load slurm 2>/dev/null; sinfo -o "%P %a %F" --noheader'
```

Show partition availability as `partition: A/I/O/T` (Allocated/Idle/Other/Total).

## Mode 2: Specific Job (`/slurm-monitor [cluster] <jobid>`)

### 2a. Job status

```bash
ssh $CLUSTER "module load slurm 2>/dev/null; scontrol show job <JOBID> 2>/dev/null || sacct -j <JOBID> --format=JobID,JobName,Partition,State,ExitCode,Elapsed,NNodes,NCPUS,NodeList%30,Reason%50 --noheader"
```

Extract: state, runtime, timelimit, allocated nodes, exit code, reason.

### 2b. Find and read logs

```bash
ssh $CLUSTER "find /home/\$USER -maxdepth 3 -name '*<JOBID>*' -mtime -7 2>/dev/null | head -10"
```

Also check standard locations:
```bash
ssh $CLUSTER "ls -la /home/\$USER/*/logs/*<JOBID>* 2>/dev/null; ls -la /home/\$USER/logs/*<JOBID>* 2>/dev/null"
```

For each `.out` / `.err` found:
- `--full`: complete content
- `--tail N`: last N lines
- default: last 30 lines

```bash
ssh $CLUSTER "tail -<N> <LOG_PATH>"
```

### 2c. Training progress detection

If the log contains training output, extract current step / total, loss,
learning rate, ETA, and any errors. Common patterns:

```
step N/M, loss: X.XXX
Epoch N/M
train_loss: X.XXX
```

Report progress as a brief summary.

## Mode 3: Node Status (`--nodes`)

### 3a. Full node status

```bash
ssh $CLUSTER 'module load slurm 2>/dev/null; sinfo -N -o "%N %P %T %c %m %G %E" --noheader | sort'
```

### 3b. GPU availability

**SuperPod** (has the `savail` wrapper):
```bash
ssh superpod 'module load slurm 2>/dev/null; savail -p normal 2>/dev/null; echo "---"; savail -p preempt 2>/dev/null'
```

**HPC4** (no `savail` — read it off `sinfo`):
```bash
ssh hpc4 'sinfo -p gpu-a30,gpu-l20,gpu-rtx4090d,gpu-rtx5880,hpc3gpu-math1,hpc3gpu-math2 -o "%P %N %T %G %C" --noheader'
```
Idle nodes in a GPU partition are free capacity; `mixed` nodes have some GPUs
left, `allocated` have none. Confirm a specific node with
`scontrol show node <N>` and compare `AllocTRES` against `CfgTRES`.

### 3c. Problem nodes

```bash
ssh $CLUSTER 'module load slurm 2>/dev/null; sinfo -o "%N %T %E" --noheader | grep -iE "drain|down|error|fail"'
```

Output a recommended `--exclude` list from what you actually see. Do not carry
over a hardcoded list from a previous run.

## Mode 4: Quota (`--quota`)

```bash
ssh $CLUSTER 'module load slurm 2>/dev/null; squota 2>/dev/null'
```

Present on both clusters, but the table layouts differ: SuperPod reports
GPU/CPU hours against the allocation; HPC4 prints a per-account, per-partition
box-drawing table (one block per account, e.g. `kanichen` and `migrate`).
Strip the ANSI escapes before reformatting HPC4's output.

## Output Format

```
## SLURM Status — <CLUSTER> — <timestamp>

### Running Jobs
<table or "No running jobs">

### Recent Completed
<table or "No recent jobs">

### Cluster Health
<partition availability summary>

### Alerts
- <failed jobs, problem nodes, quota warnings>
```

Always name the cluster in the header.

## Safety

- This skill is **read-only**. It never modifies, submits, or cancels jobs.
- All commands are `squeue`, `sacct`, `sinfo`, `scontrol show`, `savail`,
  `squota`, `find`, `tail`, `cat` — no side effects.
- If the user asks to cancel or resubmit, direct them to `scancel` or
  `/slurm-submit`.
- Login nodes are for inspection only on both clusters — never run anything
  heavier than `tail` from here.
