#!/usr/bin/env bash
# Gather raw SLURM cluster data on a HKUST login node.
#
# Usage:
#   ssh superpod 'bash -l -s' < gather-cluster-info.sh
#   ssh hpc4     'bash -l -s' < gather-cluster-info.sh
#
# The script self-detects which cluster it landed on, so the same file works
# for both. Output: sections separated by === SECTION === markers.
set -uo pipefail

# SuperPod needs `module load slurm`; HPC4 ships slurm in /usr/bin and has no
# such module. Either way, don't let a missing module abort the run.
module load slurm 2>/dev/null || true

echo "=== CLUSTER_NAME ==="
scontrol show config 2>/dev/null | awk -F'= ' '/ClusterName/{print $2}' | tr -d ' ' || echo "unknown"

echo "=== LOGIN_NODE ==="
hostname

echo "=== SITE_FEATURES ==="
# Which site-local wrappers and container runtimes exist. Consumers use this to
# decide between the Pyxis (SuperPod) and Apptainer/conda (HPC4) job templates.
for b in savail squota enroot apptainer singularity module; do
  if command -v "$b" >/dev/null 2>&1 || [ "$(type -t "$b" 2>/dev/null)" = function ]; then
    echo "$b: yes"
  else
    echo "$b: no"
  fi
done
echo -n "srun_container_image: "
srun --help 2>&1 | grep -q -- '--container-image' && echo yes || echo no

echo "=== PARTITION_OVERVIEW ==="
sinfo -o "%P %G %c %m %z %l %a %D %N" --noheader 2>/dev/null

echo "=== PARTITION_DETAILS ==="
scontrol show partition 2>/dev/null

echo "=== NODE_TYPES ==="
sinfo -N -o "%N %c %m %z %G %T" --noheader 2>/dev/null | sort -u -k1,1

echo "=== GRES_TYPES ==="
# Distinct GPU models, so callers can build a correct --gres=gpu:<type>:N
# (mandatory on HPC4, where a bare --gpus is scheduled as zero GPUs).
sinfo -o "%P %G" --noheader 2>/dev/null | awk '{print $2}' | grep -v '^(null)$' | sort -u

echo "=== QOS_LIMITS ==="
sacctmgr show qos format=Name%20,MaxWall%14,MaxTRESPerUser%30,MaxJobsPerUser%12,MaxSubmitJobsPerUser%14 --noheader 2>/dev/null

echo "=== NODE_AVAILABILITY ==="
sinfo -o "%P %a %F" --noheader 2>/dev/null

echo "=== ACCOUNT_ASSOC ==="
sacctmgr show assoc where user="$USER" format=Account%20,Partition%15,QOS%40,MaxTRESPerUser%30 --noheader 2>/dev/null

echo "=== MY_RUNNING_JOBS ==="
squeue -u "$USER" -o "%i %j %P %T %M %l %D %C %b %N %r" --noheader 2>/dev/null

echo "=== CONTAINER_IMAGES ==="
# SuperPod: enroot .img bundles under /project/<account>/images.
# HPC4: Apptainer .sif files, usually in $HOME or the PI project dir.
for pat in /project/*/images/*.img "$HOME"/*.img /project/*/*.sif /project/*/images/*.sif "$HOME"/*.sif; do
  ls -lh $pat 2>/dev/null
done
echo "--- conda envs ---"
ls -1 "$HOME"/anaconda3/envs "$HOME"/miniconda3/envs 2>/dev/null

echo "=== PROBLEM_NODES ==="
sinfo -o "%N %T %E" --noheader 2>/dev/null | grep -iE 'drain|down|error|fail' || echo "none"

echo "=== END ==="
