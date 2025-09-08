# checkpoint-agent
This is checkpoint-agent for the transitional operator to run on each worker node of workload clusters. This watches the configured checkpoint paths and uploads to an s3 storage engine e.g Minio

The following are the environment variables required:
- CHECKPOINT_DIR
- MINIO_ENDPOINT
- MINIO_ACCESS_KEY
- MINIO_SECRET_KEY
- MINIO_BUCKET
- PULL_INTERVAL