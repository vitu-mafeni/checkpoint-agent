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

examples
export PATH=$PATH:/usr/local/go/bin

# MinIO client and checkpoint settings
export CHECKPOINT_DIR=/var/lib/kubelet/checkpoints
export MINIO_ENDPOINT=192.168.28.111:30350
export MINIO_ACCESS_KEY=nephio1234
export MINIO_SECRET_KEY=secret1234
export MINIO_BUCKET=checkpoints
export PULL_INTERVAL=5s

# fault detection client settings
export CONTROLLER_URL=http://192.168.28.111:8090/heartbeat
export FAULT_DETECTION_INTERVAL=10s