---
name: slurm-submit
description: "Generate and submit SLURM batch jobs on HKUST SuperPod (Pyxis/Enroot containers) or HPC4 (conda/Apptainer). Handles per-cluster GPU request syntax, container mounts, env var injection, multi-GPU training, and bad node exclusion. Use for any non-interactive compute task."
argument-hint: "[superpod|hpc4] [description, e.g. 'train qwen3-8B with ROLL on 8 GPUs']"
---

# SLURM Job Submission

Generate and submit batch jobs via SSH. For interactive sessions, use
`/hkust-superpod-session` instead.

## Step 0: Pick the cluster — do this first

Read the leading word of `$ARGUMENTS`:

| Argument | Cluster | SSH alias (`$CLUSTER`) |
|----------|---------|------------------------|
| `hpc4` | HPC4 | `hpc4` |
| `superpod` or absent | SuperPod | `superpod` |

If the request mentions a container image path, a GPU model, or an account,
use that to sanity-check the choice and say which cluster you picked.

**The two clusters need structurally different job scripts.** Getting this
wrong does not produce a slow job, it produces an immediate hard error:

| | SuperPod | HPC4 |
|---|---|---|
| GPU request | `--gpus N` | **`--gres=gpu:<type>:N`** |
| Containers | Pyxis/enroot via `srun --container-image=…` | **no Pyxis** — conda or Apptainer *inside* the step |
| Partitions | `normal`, `preempt`, `cpu` | `intel`, `amd`, `gpu-a30`, `gpu-l20`, `gpu-rtx4090d`, `gpu-rtx5880`, `hpc3gpu-math1`, `hpc3gpu-math2` |
| Accounts | `hdtaccuracy`, `visworld01`, `gzmcagent`, … | `kanichen`, `migrate` |
| `module load slurm` | required | not needed |

Full reference: `~/wkspace/hkust-superpod-auto/skills/CLUSTERS.md`.

## Prerequisites Check

1. **VPN**: `timeout 3 bash -c 'echo > /dev/tcp/<HOST>/22' 2>/dev/null`
   (`superpod.ust.hk` / `hpc4.ust.hk`)
2. **Cluster info** (recommended): read `~/.cache/slurm-info/<cluster>.md` if it
   exists, for current problem nodes, partitions, GRES strings, QOS caps and
   image paths. Run `/slurm-info <cluster>` if it is missing or stale.
3. **No duplicate jobs**:
   `ssh $CLUSTER 'module load slurm 2>/dev/null; squeue -u $USER -o "%j %T" --noheader'`
   — warn if a job with the same name is already running.

## Step 1: Determine Job Parameters

| Parameter | How to decide | SuperPod default | HPC4 default |
|-----------|--------------|------------------|--------------|
| Job name | From task description | `job-<timestamp>` | `job-<timestamp>` |
| Account | **Mandatory.** From cluster-info assoc; ask if ambiguous | `hdtaccuracy` | `kanichen` |
| Nodes | 1 unless multi-node | `1` | `1` |
| GPUs | From model size / config | `--gpus 8` | `--gres=gpu:a30:4` |
| CPUs | ~16 per GPU | scheduler default | `--cpus-per-task 16` |
| Partition | Match the GPU you need | `normal` (`preempt` for best-effort) | `gpu-a30` |
| Time limit | Must fit the **QOS** cap, not the partition's `infinite` | `30:00:00` (≤3d) | `1-00:00:00` (≤3d GPU / 5d CPU) |
| Exclude nodes | From cluster-info problem nodes | none unless detected | none unless detected |
| Environment | Container or conda | `/project/<account>/images/roll.img` | conda env, ask which |

Do not hardcode an `--exclude` list. The historical `dgx-31,dgx-30` is stale —
both were healthy at last check. Derive it from `/slurm-info`.

### Walltime is capped by QOS

`--time` above the QOS `MaxWall` fails at submit with
`QOSMaxWallDurationPerJobLimit`. GPU QOS on HPC4 caps at 3 days
(`a30_qos`, `l20_qos`, `5880_qos`, `4090d_qos`); CPU QOS at 5 days
(`intel_qos`, `amd_qos`); `debug` at 4 hours. SuperPod's `normal_qos` caps at
3 days, `large_qos` at 7. `--qos` itself is optional — the association picks a
default.

### Environment Variables — Critical Gotcha (SuperPod)

**Env vars from the login node do NOT automatically enter the Pyxis
container.** You MUST pass them via `srun --export=VAR1,VAR2,…`.

| Variable | Purpose |
|----------|---------|
| `WANDB_API_KEY` | W&B logging |
| `HF_TOKEN` | HuggingFace downloads |
| `MASTER_ADDR` / `MASTER_PORT` | Distributed training (compute inside the script) |
| Custom (`CONFIG_NAME`, …) | Per-experiment config |

On HPC4 there is no container boundary, so the job step inherits the
submitting environment normally — but `--export` is still the explicit,
reproducible way to pass per-experiment variables.

## Step 2a: Generate sbatch script — SuperPod (Pyxis)

```bash
#!/bin/bash
#SBATCH --job-name=<JOB_NAME>
#SBATCH --nodes=<NODES>
#SBATCH --gpus-per-node=<GPUS>
#SBATCH --ntasks-per-node=1
#SBATCH --exclude=<EXCLUDE_NODES>       # omit the line entirely if none
#SBATCH --time=<TIME_LIMIT>
#SBATCH --account=<ACCOUNT>
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

### SuperPod template rules

- **Always** `--no-container-mount-home` plus an explicit `/home/$USER` mount —
  prevents default-mount conflicts with Pyxis.
- **Always** `--container-env=PYXI_DISABLE_DEFAULT_MOUNTS=1`.
- **Never** `--container-save` in a batch job — it conflicts with concurrent
  jobs and wastes time. (It is fine in an interactive `srun`.)
- Base mount: `--container-mounts /home/$USER:/home/$USER`; add
  `/project/<account>:/project/<account>` for project data.

## Step 2b: Generate sbatch script — HPC4 (conda / Apptainer)

```bash
#!/bin/bash
#SBATCH --job-name=<JOB_NAME>
#SBATCH --nodes=<NODES>
#SBATCH --ntasks-per-node=1
#SBATCH --gres=gpu:<GPU_TYPE>:<GPUS>    # NOT --gpus; see gotcha below
#SBATCH --cpus-per-task=<CPUS>
#SBATCH --exclude=<EXCLUDE_NODES>       # omit the line entirely if none
#SBATCH --time=<TIME_LIMIT>
#SBATCH --account=<ACCOUNT>
#SBATCH --partition=<PARTITION>
#SBATCH --output=logs/%x_%j.out
#SBATCH --error=logs/%x_%j.err

set -euo pipefail
cd <WORKDIR>

# conda (default) —
source ~/anaconda3/etc/profile.d/conda.sh
conda activate <ENV>

# — or Apptainer, replacing the two conda lines:
#   apptainer exec --nv --bind /project/<account>:/project/<account> \
#       <IMAGE>.sif bash -c '<USER_COMMANDS>'

srun --export=ALL <USER_COMMANDS>
```

### HPC4 template rules

- **`--gres=gpu:<type>:N`, never a bare `--gpus`.** With `--gpus`, HPC4's
  submit plugin prints `Notice: CPUs reduced to 0 (Maximum allowed for 0
  GPU(s))` and schedules the job as if it requested no GPU — it runs, and
  training then fails on a missing device. Valid types: `a30`, `l20`, `4090d`,
  `rtx5880`, `3090`, `6000ada`. Match the type to the partition.
- **No `--container-*` flags.** There is no enroot; `srun --container` on HPC4
  means an OCI bundle directory (and no OCI runtime is installed), not an
  enroot `.img`. Pasting the SuperPod template here is the single most likely
  failure.
- **Apptainer only runs inside the job step, never on `login4`** — the login
  node has `max_user_namespaces = 0` and no `starter-suid`, so containers
  cannot be created there at all. Put every `apptainer` call inside the
  `srun`/batch body:

  ```bash
  # Downloads go DIRECT. srun defaults to --export=ALL, so drop any proxy the
  # submitting shell carried in — the spod relay is for the AI APIs only, and
  # routing a multi-GB pull through it starves claude/codex and crawls.
  unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy

  # pull straight from the registry (HPC4 reaches docker.io/nvcr.io directly)
  apptainer build --sandbox $TMPDIR/img docker://nvcr.io/nvidia/pytorch:24.05-py3
  apptainer exec --nv --bind /project/<account> $TMPDIR/img python train.py
  ```

  `.sif` files cannot be *created* on HPC4 (`mksquashfs` runs through `proot`,
  and `ptrace_scope=3` blocks it) — build them off-cluster and copy them in, or
  work from sandbox directories. `apptainer exec --writable <sandbox>` persists
  installs, which is the closest thing to SuperPod's `--container-save`.
  If a mount hook fails on `/opt/knem-…`, add
  `--no-mount /opt/knem-1.1.4.90mlnx3`; that is a stale site bind path on some
  CPU nodes, not a problem with your image.
- Set `--cpus-per-task` explicitly (~16 per GPU); the GPU partitions have
  64 CPUs across 4–6 GPUs.
- `module` is Lmod over Spack — `module load cuda/12.4.0-uhdfj7w`,
  `miniconda3/24.3.0-quc3pyu`, etc. `module avail` lists what is there.

## Both: shared rules

- **Always** create `logs/` before submitting.
- **Always** use `--output` / `--error` with `%x` (job name) and `%j` (job ID).
- **Always** pass `--account` — omitting it fails with
  `Please kindly add the --account … SLURM flag`, which looks nothing like a
  quota error.
- Multi-node: keep `--ntasks-per-node=1` and compute rendezvous inside the step:

```bash
export MASTER_ADDR=$(scontrol show hostname $SLURM_NODELIST | head -n1)
export MASTER_PORT=$((29500 + RANDOM % 100))
```

## Step 3: Dry-run, upload, submit

Validate before spending a queue slot — this checks account, QOS, walltime and
GRES without submitting anything:

```bash
ssh $CLUSTER "module load slurm 2>/dev/null; sbatch --test-only <SAME_FLAGS> --wrap='true'"
```

Then:

```bash
# Write the script locally
cat > /tmp/<JOB_NAME>.sh << 'JOBSCRIPT'
<generated script content>
JOBSCRIPT

ssh $CLUSTER "mkdir -p <PROJECT_DIR>/logs"
scp /tmp/<JOB_NAME>.sh $CLUSTER:<PROJECT_DIR>/<JOB_NAME>.sh
ssh $CLUSTER "cd <PROJECT_DIR> && module load slurm 2>/dev/null; sbatch <JOB_NAME>.sh"
```

Capture the job ID from `Submitted batch job <JOBID>`.

## Step 4: Post-submission Check

```bash
ssh $CLUSTER "module load slurm 2>/dev/null; squeue -j <JOBID> -o '%i %j %P %T %M %l %D %N %b %r' --noheader"
```

Report the cluster, job ID, state, allocated nodes, and the log paths
`logs/<JOB_NAME>_<JOBID>.out` / `.err`.

On HPC4, check the `%b` (TRES-per-node) column shows the GPUs you asked for —
if it is empty, the `--gres` was wrong and the job will run without a GPU.

Suggest: "Use `/slurm-monitor <cluster> <JOBID>` to check progress."

## Troubleshooting

### `Please kindly add the --account … SLURM flag`
Both clusters require `--account`. Pick one the user is associated with
(`sacctmgr show assoc where user=$USER format=Account,Partition,QOS`).

### `QOSMaxWallDurationPerJobLimit` / `Job violates accounting/QOS policy`
`--time` exceeds the QOS cap, or you are over MaxJobs/MaxSubmit for that QOS.
Shorten the walltime or switch QOS.

### Job runs but sees no GPU (HPC4)
Almost always `--gpus` instead of `--gres=gpu:<type>:N`. Check with
`squeue -o "%i %b"` — an empty TRES-per-node confirms it.

### `--container-image: unrecognized option` / enroot errors (HPC4)
The SuperPod Pyxis template leaked into an HPC4 job. Regenerate with the
Step 2b template.

### Job stuck in PENDING
```bash
ssh $CLUSTER "module load slurm 2>/dev/null; squeue -j <JOBID> -o '%r' --noheader"
```
- `Priority` — waiting for resources, be patient
- `Resources` — not enough GPUs; try fewer, another GPU partition, or
  `--partition preempt` (SuperPod)
- `ReqNodeNotAvail` — excluded too many nodes, or a maintenance window; check
  `sinfo`. HPC4 keeps several nodes `drained* maintenance` at any time.
- `QOSMaxJobsPerUserLimit` — already at the concurrent-job cap

### Job failed immediately
```bash
ssh $CLUSTER "module load slurm 2>/dev/null; sacct -j <JOBID> --format=JobID,State,ExitCode,Reason%50"
ssh $CLUSTER "tail -50 <PROJECT_DIR>/logs/<JOB_NAME>_<JOBID>.err"
```
Common causes: image/env path wrong (`ls -la` it), env var not exported, OOM,
or a node hardware fault (add to `--exclude` and resubmit).

### Need to cancel
```bash
ssh $CLUSTER "module load slurm 2>/dev/null; scancel <JOBID>"
```
