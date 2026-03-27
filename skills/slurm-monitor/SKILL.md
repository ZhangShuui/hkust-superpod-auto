---
name: slurm-monitor
description: "Monitor SLURM jobs on HKUST SuperPod — check queue, read logs, inspect node status, track GPU usage. Read-only operations, safe to run anytime."
argument-hint: "[jobid] [--full] [--tail N] [--nodes] [--quota]"
allowed-tools: Read, Glob, Grep, Bash(ssh:*), Bash(timeout:*), Bash(cat:*), Bash(spod:*)
---

# SLURM Job Monitor

Monitor jobs and cluster status on SuperPod via SSH. All operations are **read-only**.

## Connection

- SSH target: `superpod`
- Prerequisite: VPN running

## Parse Arguments

From `$ARGUMENTS`, determine the operation mode:

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
ssh superpod 'module load slurm 2>/dev/null && squeue -u $USER -o "%i %j %P %T %M %l %D %C %b %N" --noheader'
```

If jobs found, display as a formatted table:

| JobID | Name | Partition | State | Runtime | Timelimit | Nodes | CPUs | GPUs | NodeList |
|-------|------|-----------|-------|---------|-----------|-------|------|------|----------|

### 1b. Check recently completed jobs (last 24h)

```bash
ssh superpod 'module load slurm 2>/dev/null && sacct -u $USER --starttime now-1day --format=JobID%10,JobName%20,Partition%10,State%12,ExitCode%8,Elapsed%12,NNodes%6,NCPUS%6,TRESUsageInTot%40 --noheader | head -20'
```

Display completed jobs, highlighting:
- **COMPLETED** — success
- **FAILED** / **CANCELLED** — needs attention
- **TIMEOUT** — job hit walltime limit
- **OUT_OF_MEMORY** — OOM, suggest reducing batch size

### 1c. Quick cluster health

```bash
ssh superpod 'module load slurm 2>/dev/null && sinfo -o "%P %a %F" --noheader'
```

Show partition availability as: `partition: A/I/O/T` (Allocated/Idle/Other/Total).

## Mode 2: Specific Job (`/slurm-monitor <jobid>`)

### 2a. Job status

```bash
ssh superpod "module load slurm 2>/dev/null && scontrol show job <JOBID> 2>/dev/null || sacct -j <JOBID> --format=JobID,JobName,Partition,State,ExitCode,Elapsed,NNodes,NCPUS,NodeList%30,Reason%50 --noheader"
```

Extract and display:
- State, runtime, timelimit
- Allocated nodes
- Exit code (if completed)
- Reason (if pending/failed)

### 2b. Find and read logs

Search for log files matching this job:

```bash
ssh superpod "find /home/$USER -maxdepth 3 -name '*<JOBID>*' -newer /home/$USER -mtime -7 2>/dev/null | head -10"
```

Also check standard locations:
```bash
ssh superpod "ls -la /home/$USER/*/logs/*<JOBID>* 2>/dev/null; ls -la /home/$USER/logs/*<JOBID>* 2>/dev/null"
```

For each log file found (`.out` and `.err`):
- If `--full`: show complete content
- If `--tail N`: show last N lines
- Default: show last 30 lines

```bash
ssh superpod "tail -<N> <LOG_PATH>"
```

### 2c. Training progress detection

If the log contains training output, try to extract:
- Current step / total steps
- Current loss value
- Learning rate
- Estimated time remaining
- Any errors or warnings

Look for common patterns:
```
step N/M, loss: X.XXX
Epoch N/M
train_loss: X.XXX
```

Report progress as a brief summary.

## Mode 3: Node Status (`/slurm-monitor --nodes`)

### 3a. Full node status

```bash
ssh superpod 'module load slurm 2>/dev/null && sinfo -N -o "%N %P %T %c %m %G %E" --noheader | sort'
```

### 3b. GPU availability by partition

```bash
ssh superpod 'module load slurm 2>/dev/null && savail -p normal 2>/dev/null; echo "---"; savail -p preempt 2>/dev/null'
```

### 3c. Problem nodes

```bash
ssh superpod 'module load slurm 2>/dev/null && sinfo -o "%N %T %E" --noheader | grep -iE "drain|down|error|fail"'
```

Output a recommended `--exclude` list for use with `srun`/`sbatch`.

## Mode 4: Quota (`/slurm-monitor --quota`)

```bash
ssh superpod 'module load slurm 2>/dev/null && squota 2>/dev/null'
```

Display GPU/CPU hours usage and remaining balance.

## Output Format

Always structure output as:

```
## SLURM Status — <timestamp>

### Running Jobs
<table or "No running jobs">

### Recent Completed
<table or "No recent jobs">

### Cluster Health
<partition availability summary>

### Alerts
- <any failed jobs, problem nodes, quota warnings>
```

## Safety

- This skill is **read-only**. It never modifies, submits, or cancels jobs.
- All commands are `squeue`, `sacct`, `sinfo`, `scontrol show`, `find`, `tail`, `cat` — no side effects.
- If the user asks to cancel or resubmit from within this skill, direct them to use `scancel` manually or `/slurm-submit`.
