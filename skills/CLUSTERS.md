# Cluster Reference — SuperPod vs HPC4

Shared fact sheet for every skill in this repo. Read it before generating any
SLURM command: **the two clusters run different schedulers' worth of
conventions**, and a command that works on one silently fails on the other.

Verified 2026-09-01 by probing both login nodes. Point-in-time rows (drained
nodes, accounts, images) are marked — re-check those with `/slurm-info`.

## Picking a cluster

Every skill takes an optional leading cluster word, mirroring the `spod` CLI:

| You type | Cluster | SSH alias | spod prefix |
|----------|---------|-----------|-------------|
| (nothing) | SuperPod | `superpod` | `spod` |
| `superpod ...` | SuperPod | `superpod` | `spod` |
| `hpc4 ...` | HPC4 | `hpc4` | `spod hpc4` |

Both aliases are written into `~/.ssh/config` by `ensureSSHConfig()` on every
`spod` run. Both need the same VPN, and `VPN_HOSTS` in `.env` must list
**both** hostnames or the missing one resolves but never connects.

## Side-by-side

| | **SuperPod** | **HPC4** |
|---|---|---|
| Host | `superpod.ust.hk` | `hpc4.ust.hk` (143.89.184.3) |
| Login node(s) | `slogin-01`, `slogin-02` | `login4` (only one) |
| Account system | ITSC SuperPod allocation | **separate account** — same ITSC id, different grant |
| `--account` | **required** | **required** |
| Accounts available *(point-in-time)* | `hdtaccuracy`, `visworld01`, `gzmcagent`, `msccsit2024` | `kanichen`, `migrate` |
| Partitions | `normal`, `preempt`, `cpu` | `intel`, `amd`, `gpu-a30`, `gpu-l20`, `gpu-rtx4090d`, `gpu-rtx5880`, `hpc3gpu-math1`, `hpc3gpu-math2` |
| GPU request | `--gpus N` | **`--gres=gpu:<type>:N`** — see gotcha below |
| GPU hardware | 8× per node (DGX) | a30 ×4, l20 ×4, 4090d ×6, rtx5880 ×6, 3090 ×8, 6000ada ×8 |
| Node names | `dgx-NN` | `cpuNN`, `gpuNN`, `hpc3gpuNN` |
| Containers | **Pyxis/enroot** — `srun --container-image=…img` | **no Pyxis** — Apptainer/Singularity, run inside the job |
| `module` | `module load slurm` first | Lmod + Spack (`/opt/shared/spack`); slurm is in `/usr/bin`, no module needed |
| `savail` | yes | **absent** — use `sinfo`/`squeue` instead |
| `squota` | yes | yes (different table layout) |
| Home | `/home/$USER` | `/home/$USER` (NFS, 200 GB) |
| Project | `/project/<account>` | `/project/<pi-account>` |
| Node runtime | `~/.local/…` / conda | `~/.local/node24/bin` (independent Node 24) |
| Reverse tunnel / relay | per-UID ports, `spod tunnel` | per-UID ports, `spod hpc4 tunnel` |

## QOS walltime caps

Partitions advertise `infinite`; the **QOS** is what actually caps you.

**SuperPod** *(point-in-time)*

| QOS | MaxWall | MaxTRES/user | MaxJobs | MaxSubmit |
|-----|---------|--------------|---------|-----------|
| `normal_qos` | 3-00:00:00 | — | 8 | 10 |
| `normal_debug_qos` | 02:00:00 | cpu=56, gpu=2 | 1 | 1 |
| `large_qos` | 7-00:00:00 | — | 4 | 5 |
| `large_debug_qos` | 02:00:00 | gpu=16, node=2 | 1 | 1 |
| `preempt_qos` | — (best effort, preemptible) | — | — | — |
| `cpu_qos` | — | — | 28 | — |

**HPC4** *(point-in-time)*

| QOS | MaxWall | MaxTRES/user | MaxJobs | MaxSubmit |
|-----|---------|--------------|---------|-----------|
| `intel_qos` | 5-00:00:00 | cpu=256 | 10 | 15 |
| `amd_qos` | 5-00:00:00 | cpu=3072 | 240 | 250 |
| `a30_qos` | 3-00:00:00 | gpu=16 | 8 | 10 |
| `l20_qos` | 3-00:00:00 | gpu=8 | 2 | 4 |
| `5880_qos` | 3-00:00:00 | gpu=12 | 8 | 10 |
| `4090d_qos` | 3-00:00:00 | gpu=2 | 2 | 4 |
| `debug` / `debug_qos` | 04:00:00 | gpu=2, node=2 | 1 | 1 |

Exceeding a cap fails at submit with
`QOSMaxWallDurationPerJobLimit` / `Job violates accounting/QOS policy`.
`--qos` itself is optional — the association picks a default.

## Cluster-specific gotchas

### HPC4: `--gpus` silently zeroes your CPUs
`sbatch --gpus=1` on HPC4 prints
`Notice: CPUs reduced to 0 (Maximum allowed for 0 GPU(s))` — the site's submit
plugin counts GPUs from `--gres`, not `--gpus`, so a `--gpus`-only job is
scheduled as if it asked for no GPU. **Always write `--gres=gpu:<type>:N`** on
HPC4 (`gpu:a30:4`, `gpu:l20:2`, `gpu:4090d:1`, `gpu:rtx5880:2`, `gpu:3090:8`,
`gpu:6000ada:8`). SuperPod's `--gpus N` is fine and is the normal form there.

### HPC4 has no Pyxis — the SuperPod srun template will not run
There is no `enroot`, and `srun --container` on HPC4 means an *OCI bundle
directory*, not an enroot `.img`. Every SuperPod flag below is a hard error on
HPC4: `--container-image`, `--container-mounts`, `--container-save`,
`--container-remap-root`, `--no-container-mount-home`, `--container-writable`,
`--container-env`. Use one of these on HPC4 instead:
- **conda** (`~/anaconda3`, or `module load miniconda3/24.3.0-quc3pyu`) — the
  default for most work;
- **Apptainer/Singularity** — `apptainer exec --nv image.sif …` *inside* the
  job step, not as srun flags.

### Both: `--account` is mandatory
Omitting it fails with `Please kindly add the --account … SLURM flag`, not with
a scheduler-native error, so it will not look like a quota problem.

### HPC4: no `savail`
`savail` is a SuperPod-local wrapper. On HPC4 read availability from
`sinfo -o "%P %N %T %G"` / `sinfo -o "%P %a %F"`.

### Both: login nodes are for editing and submitting only
No training, no `python train.py`, no builds heavy enough to notice. SuperPod
enforces this and will kill offenders; treat HPC4's `login4` the same way.

### Currently drained nodes *(point-in-time — re-check, do not hardcode)*
- SuperPod: none.
- HPC4: `cpu65-68`, `gpu18` (maintenance); `gpu08`, `gpu09`, `gpu15` (hardware
  maintenance).

The `--exclude=dgx-31,dgx-30` that older versions of these skills hardcoded is
stale; derive the exclude list from `/slurm-info` instead.

## Connecting

```bash
spod vpn status            # one VPN, reports both clusters
spod            / spod hpc4            # tmux session on either
spod ssh        / spod hpc4 ssh        # raw SSH, no tmux
spod get PATH   / spod hpc4 get PATH   # MD5-verified pull to Windows Downloads
spod socks      / spod hpc4 socks      # local :1080 vs :1081
```

Never hardcode the username in a skill — read `SUPERPOD_USER` / `HPC4_USER`
from `.env`, or just let `$USER` expand on the remote side.
