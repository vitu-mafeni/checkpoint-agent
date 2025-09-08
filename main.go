package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func getenv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getenvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}

func getenvDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

// computeSHA256 returns the SHA256 checksum of a file
func computeSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func main() {
	ctx := context.Background()

	checkpointDir := getenv("CHECKPOINT_DIR", "/var/lib/kubelet/checkpoints")
	minioEndpoint := getenv("MINIO_ENDPOINT", "192.168.28.111:30350")
	minioAccess := getenv("MINIO_ACCESS_KEY", "nephio1234")
	minioSecret := getenv("MINIO_SECRET_KEY", "secret1234")
	minioBucket := getenv("MINIO_BUCKET", "checkpoints")
	pullInterval := getenvDuration("PULL_INTERVAL", 1*time.Minute)

	minioClient, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioAccess, minioSecret, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalf("Failed to initialize MinIO client: %v", err)
	}

	exists, err := minioClient.BucketExists(ctx, minioBucket)
	if err != nil {
		log.Fatalf("Failed to check bucket: %v", err)
	}
	if !exists {
		if err := minioClient.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{}); err != nil {
			log.Fatalf("Failed to create bucket: %v", err)
		}
	}

	  // Upload existing files on startup
    files, err := os.ReadDir(checkpointDir)
    if err != nil {
        log.Fatalf("Failed to read checkpoint directory: %v", err)
    }

    for _, f := range files {
        if !f.IsDir() {
            go uploadFileIfChanged(ctx, minioClient, minioBucket, filepath.Join(checkpointDir, f.Name()))
        }
    }

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Failed to create watcher: %v", err)
	}
	defer watcher.Close()

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create == fsnotify.Create {
					go uploadFileIfChanged(ctx, minioClient, minioBucket, event.Name)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Watcher error:", err)
			}
		}
	}()

	if err := watcher.Add(checkpointDir); err != nil {
		log.Fatalf("Failed to add checkpoint directory to watcher: %v", err)
	}

	ticker := time.NewTicker(pullInterval)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			syncFromMinio(ctx, minioClient, minioBucket, checkpointDir)
		}
	}()

	log.Println("Checkpoint agent with checksum validation running...")
	select {}
}

func uploadFileIfChanged(ctx context.Context, client *minio.Client, bucket, filePath string) {
	fileName := filepath.Base(filePath)
	localHash, err := computeSHA256(filePath)
	if err != nil {
		log.Printf("Failed to compute hash for %s: %v", fileName, err)
		return
	}

	// Check if file exists in MinIO
	obj, err := client.GetObject(ctx, bucket, fileName, minio.GetObjectOptions{})
	if err == nil {
		defer obj.Close()
		hasher := sha256.New()
		if _, err := io.Copy(hasher, obj); err == nil {
			remoteHash := hex.EncodeToString(hasher.Sum(nil))
			if localHash == remoteHash {
				log.Printf("File %s is already up-to-date in MinIO, skipping", fileName)
				return
			}
		}
	}

	// Upload file
	_, err = client.FPutObject(ctx, bucket, fileName, filePath, minio.PutObjectOptions{})
	if err != nil {
		log.Printf("Failed to upload %s: %v", fileName, err)
	} else {
		log.Printf("Uploaded %s to MinIO bucket %s", fileName, bucket)
	}
}

func syncFromMinio(ctx context.Context, client *minio.Client, bucket, checkpointDir string) {
	log.Println("Syncing checkpoint files from MinIO...")
	objectCh := client.ListObjects(ctx, bucket, minio.ListObjectsOptions{})
	for obj := range objectCh {
		if obj.Err != nil {
			log.Println("ListObjects error:", obj.Err)
			continue
		}
		localPath := filepath.Join(checkpointDir, obj.Key)
		if _, err := os.Stat(localPath); os.IsNotExist(err) {
			log.Printf("Downloading missing file %s from MinIO", obj.Key)
			err := client.FGetObject(ctx, bucket, obj.Key, localPath, minio.GetObjectOptions{})
			if err != nil {
				log.Printf("Failed to download %s: %v", obj.Key, err)
			} else {
				log.Printf("Downloaded %s from MinIO", obj.Key)
			}
		} else {
			// Optional: compare checksum and update if different
			localHash, err := computeSHA256(localPath)
			if err != nil {
				log.Printf("Failed to compute hash for %s: %v", localPath, err)
				continue
			}
			objHandle, err := client.GetObject(ctx, bucket, obj.Key, minio.GetObjectOptions{})
			if err != nil {
				log.Printf("Failed to get object %s for hash check: %v", obj.Key, err)
				continue
			}
			hasher := sha256.New()
			if _, err := io.Copy(hasher, objHandle); err != nil {
				log.Printf("Failed to hash remote file %s: %v", obj.Key, err)
				objHandle.Close()
				continue
			}
			objHandle.Close()
			remoteHash := hex.EncodeToString(hasher.Sum(nil))
			if localHash != remoteHash {
				log.Printf("Updating file %s from MinIO (hash mismatch)", obj.Key)
				err := client.FGetObject(ctx, bucket, obj.Key, localPath, minio.GetObjectOptions{})
				if err != nil {
					log.Printf("Failed to download %s: %v", obj.Key, err)
				}
			}
		}
	}
}
