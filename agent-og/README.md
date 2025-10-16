

# checkpoint-agent
This is checkpoint-agent for the transitional operator to run on each worker node of workload clusters. This watches the configured checkpoint paths and uploads to an s3 storage engine e.g Minio

The following are the environment variables required:
- CHECKPOINT_DIR
- MINIO_ENDPOINT
- MINIO_ACCESS_KEY
- MINIO_SECRET_KEY
- MINIO_BUCKET
- PULL_INTERVAL


---

# MinIO client and checkpoint settings
export CHECKPOINT_DIR=/var/lib/kubelet/checkpoints
export MINIO_ENDPOINT=52.221.201.236:32757
export MINIO_ACCESS_KEY=nephio1234
export MINIO_SECRET_KEY=secret1234
export MINIO_BUCKET=checkpoints
export PULL_INTERVAL=5s

# Only applies for AWS clusters
export AWS_REGION=ap-southeast-1

# Fault detection client settings
export CONTROLLER_URL=http://52.221.201.236:8090/heartbeat
export FAULT_DETECTION_INTERVAL=5s



Please check full access guide and intergrating with the transition operator USER-GUIDE file [HERE](https://github.com/vitu-mafeni/transition-operator/blob/main/USER-GUIDE.md)