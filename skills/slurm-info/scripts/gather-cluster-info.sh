#!/usr/bin/env bash
# Gather raw SLURM cluster data on SuperPod login node.
# Usage: ssh superpod 'bash -l -s' < gather-cluster-info.sh
# Output: structured sections separated by === SECTION === markers.
set -euo pipefail

module load slurm 2>/dev/null || true

echo "=== CLUSTER_NAME ==="
scontrol show config 2>/dev/null | awk -F'= ' '/ClusterName/{print $2}' || echo "unknown"

echo "=== PARTITION_OVERVIEW ==="
sinfo -o "%P %G %c %m %z %l %a %D %N" --noheader 2>/dev/null

echo "=== PARTITION_DETAILS ==="
scontrol show partition 2>/dev/null

echo "=== NODE_TYPES ==="
sinfo -N -o "%N %c %m %z %G %T" --noheader 2>/dev/null | sort -u -k1,1

echo "=== QOS_LIMITS ==="
sacctmgr show qos format=Name%12,MaxWall%14,MaxTRESPerUser%30,MaxJobsPerUser%12,MaxSubmitJobsPerUser%14 --noheader 2>/dev/null

echo "=== NODE_AVAILABILITY ==="
sinfo -o "%P %a %F" --noheader 2>/dev/null

echo "=== ACCOUNT_ASSOC ==="
sacctmgr show assoc where user=$USER format=Account%20,Partition%15,QOS%40,MaxTRESPerUser%30 --noheader 2>/dev/null

echo "=== MY_RUNNING_JOBS ==="
squeue -u $USER -o "%i %j %P %T %M %l %D %C %b %N %r" --noheader 2>/dev/null

echo "=== CONTAINER_IMAGES ==="
for dir in /project/*/images /home/$USER/*.img; do
  ls -lh "$dir"/*.img 2>/dev/null || true
done

echo "=== PROBLEM_NODES ==="
sinfo -o "%N %T %E" --noheader 2>/dev/null | grep -iE 'drain|down|error|fail' || echo "none"

echo "=== END ==="
