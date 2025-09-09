package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

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

	// clientset, err := GetKubeClient()
	// if err != nil {
	// 	log.Fatalf("Failed to initialize Kubernetes client: %v", err)
	// }

	// // Now clientset can be used to create pods, list nodes, etc.
	// log.Println("Kubernetes client initialized successfully")

	// pod, err := CreateRestorePod2(clientset, "default", "nginx-restored", "nginx:alpine", "checkpoint-test-pod_default-nginx-2025-09-08T14:20:56Z.tar")
	// if err != nil {
	// 	log.Fatalf("Failed to create restore pod: %v", err)
	// }
	// fmt.Println("Restore pod created:", pod.Name)

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

	// Wait until kubelet completes writing the file
	if err := waitForCompleteFile(filePath, 2*time.Second, 500*time.Millisecond); err != nil {
		log.Printf("Skipping upload, file %s not ready: %v", filePath, err)
		return
	}

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

// CreateRestorePod creates a pod that restores a container from a checkpoint
func CreateRestorePod(
	clientset *kubernetes.Clientset,
	namespace, podName, checkpointFile string,
) (*corev1.Pod, error) {

	// This assumes you have a restore image with runc installed
	restoreImage := "vitu1/restore-runc:v0.1"

	pod := &corev1.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			// Run only once for restore
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "restore",
					Image: restoreImage,
					SecurityContext: &corev1.SecurityContext{
						Privileged: ptrBool(true), // Needed for runc restore
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "checkpoint-volume",
							MountPath: "/checkpoints",
							ReadOnly:  true,
						},
					},
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf("runc restore --image-path /checkpoints/%s --id %s && tail -f /dev/null",
							checkpointFile, podName),
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "checkpoint-volume",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/var/lib/kubelet/checkpoints", // path on node where file is
						},
					},
				},
			},
		},
	}

	createdPod, err := clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, meta.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create restore pod: %w", err)
	}

	return createdPod, nil
}

func ptrBool(b bool) *bool { return &b }

func CreateRestorePod2(
	clientset *kubernetes.Clientset,
	namespace, podName, originalImage, checkpointFile string,
) (*corev1.Pod, error) {

	restoreImage := "vitu1/restore-runc:v0.4"
	imageRef := originalImage
	if !strings.Contains(imageRef, "/") {
		imageRef = "docker.io/library/" + imageRef
	}

	pod := &corev1.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "restore",
					Image: restoreImage,
					SecurityContext: &corev1.SecurityContext{
						Privileged: ptrBool(true),
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "checkpoint-volume",
							MountPath: "/checkpoints",
							ReadOnly:  false,
						},
						{
							Name:      "containerd-sock",
							MountPath: "/run/containerd/containerd.sock",
							ReadOnly:  true,
						},
						{
							Name:      "bundle",
							MountPath: "/bundle",
							ReadOnly:  false,
						},
						{
							Name:      "rootfs",
							MountPath: "/rootfs",
							ReadOnly:  false,
						},
					},
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf(`
# create working directories
mkdir -p /bundle/rootfs /bundle/work

# extract checkpoint
tar -xf /checkpoints/%s -C /bundle/work

# pull original image and export filesystem
ctr images pull %s
ctr images export /bundle/basefs.tar %s
tar -xf /bundle/basefs.tar -C /bundle/rootfs

# apply rootfs diff from checkpoint
tar -xf /bundle/work/rootfs-diff.tar -C /bundle/rootfs

# generate minimal config.json from spec.dump
cat /bundle/work/spec.dump | jq '. | {
  ociVersion: .ociVersion,
  process: .process,
  root: { path: "/rootfs" },
  mounts: .mounts,
  linux: .linux,
  annotations: .annotations,
  linux: { namespaces: .linux.namespaces }
}' > /bundle/config.json

# run runc restore
runc restore %s --bundle /bundle || echo 'Restore warnings ignored'

# keep container alive
tail -f /dev/null
`, checkpointFile, imageRef, imageRef, podName),
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "checkpoint-volume",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/var/lib/kubelet/checkpoints",
						},
					},
				},
				{
					Name: "containerd-sock",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/run/containerd/containerd.sock",
							Type: ptrHostPathType(corev1.HostPathSocket),
						},
					},
				},
				{
					Name: "bundle",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "rootfs",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	createdPod, err := clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, meta.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create restore pod: %w", err)
	}

	return createdPod, nil
}

// GetKubeClient initializes a Kubernetes client.
// - Inside a pod: uses in-cluster config
// - Outside: uses KUBECONFIG env var or ~/.kube/config
func GetKubeClient() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	// Try in-cluster first
	config, err = rest.InClusterConfig()
	if err == nil {
		// In-cluster: works inside a pod
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, err
		}
		return clientset, nil
	}

	// Fallback to kubeconfig outside the cluster
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

/*
func RestoreContainerFromCheckpoint(ctx context.Context, image string, checkpointPath string, podNamespace, podName, podUID string) error {
	client, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		return fmt.Errorf("failed to connect to containerd: %w", err)
	}
	defer client.Close()

	// Ensure the image is present (containerd needs it for layers)
	img, err := client.Pull(ctx, image, containerd.WithPullUnpack)
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", image, err)
	}

	// Generate a safe container ID (namespace-podname-uid truncated)
	base := fmt.Sprintf("%s-%s-%s", podNamespace, podName, podUID)
	base = strings.ToLower(strings.ReplaceAll(base, "_", "-"))
	if len(base) > 63 {
		base = base[:63] // containerd ID length limit
	}
	containerID := base

	// Load the checkpoint
	checkpoint, err := client.CheckpointService().Get(ctx, checkpointPath)
	if err != nil {
		return fmt.Errorf("failed to load checkpoint %s: %w", checkpointPath, err)
	}

	// Create container from checkpoint
	ctr, err := client.NewContainer(
		ctx,
		containerID,
		containerd.WithNewSnapshot(containerID+"-snap", img),
		containerd.WithNewSpec(oci.WithImageConfig(img)),
		containerd.WithCheckpoint(checkpoint),
	)
	if err != nil {
		return fmt.Errorf("failed to create container from checkpoint: %w", err)
	}
	defer ctr.Delete(ctx, containerd.WithSnapshotCleanup)

	// Create a new task
	task, err := ctr.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		return fmt.Errorf("failed to create task: %w", err)
	}
	defer task.Delete(ctx)

	// Start task
	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("failed to start task: %w", err)
	}

	fmt.Printf("✅ Restored container %s from checkpoint %s\n", containerID, checkpointPath)
	return nil
}
*/

func ptrHostPathType(t corev1.HostPathType) *corev1.HostPathType {
	return &t
}

// waitForCompleteFile waits until the file size stabilizes for a given duration
func waitForCompleteFile(filePath string, stableFor time.Duration, checkInterval time.Duration) error {
	var lastSize int64 = -1
	stableSince := time.Now()

	for {
		info, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				// File may not be created yet
				time.Sleep(checkInterval)
				continue
			}
			return fmt.Errorf("failed to stat file %s: %w", filePath, err)
		}

		currentSize := info.Size()

		if currentSize != lastSize {
			lastSize = currentSize
			stableSince = time.Now()
		} else if time.Since(stableSince) >= stableFor {
			return nil // file is stable
		}

		time.Sleep(checkInterval)
	}
}
