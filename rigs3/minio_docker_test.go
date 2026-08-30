//go:build docker

// The MinIO container the suite beside this file runs against.
//
//	go test -tags docker ./rigs3/
//
// It is ~a hundred lines that internal/dockerdb already has, and the
// duplication is deliberate. That package lives under the CLI's module, and a
// published adapter that imported it would require the whole of rig to build —
// which is the opposite of why this module exists. So this file borrows the
// two ideas it cannot do without, and nothing else: a container name qualified
// by a digest of RIG_DB_ISOLATE, so two checkouts of rig do not adopt each
// other's bucket, and a published port the kernel picks under isolation and
// reads back afterwards.
//
// The number in minioPort is dockerdb.PortS3MinIO, written out because this
// module cannot import the list it comes from. It is declared there so that
// every port a suite in this repository takes is still allocated from one
// place.
package rigs3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	// Pinned, so a run means the same thing on every machine — the way
	// electricsql/electric is pinned in internal/electrictest.
	minioImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z"
	minioName  = "rigS3-minio"
	// minioPort is dockerdb.PortS3MinIO. See this file's doc comment.
	minioPort = 55446
	// The throwaway pair. MinIO refuses a secret shorter than eight characters,
	// and nothing about this is a deployment.
	minioUser   = "rigrigrig"
	minioSecret = "rigrigrig"

	startWait = 60 * time.Second
)

var (
	once     sync.Once
	endpoint string
	startErr error

	// buckets numbers the ones each test makes, because a lifecycle rule is a
	// property of a bucket and two tests sharing one would be two tests
	// arguing about it.
	buckets atomic.Int64
)

// service brings MinIO up once for the package and reports where it answers.
func service(t *testing.T) string {
	t.Helper()

	once.Do(func() { endpoint, startErr = startMinIO() })
	if startErr != nil {
		t.Fatal(startErr)
	}
	return endpoint
}

// store builds a store against a bucket of this test's own, creating it first.
//
// The window is a parameter because one test is about a store refusing to
// exist; everywhere else it is zero, which skips the lifecycle read.
func store(t *testing.T, window time.Duration) (*Store, string) {
	t.Helper()

	bucket := newBucket(t)
	s, err := New(t.Context(), config(t, bucket, window))
	if err != nil {
		t.Fatalf("building a store against %s: %v", bucket, err)
	}
	return s, bucket
}

// config is what every store in this suite is built from.
func config(t *testing.T, bucket string, window time.Duration) Config {
	t.Helper()

	return Config{
		Bucket:          bucket,
		Endpoint:        service(t),
		AccessKeyID:     minioUser,
		SecretAccessKey: minioSecret,
		RestoreWindow:   window,
	}
}

// newBucket creates an empty bucket named after nothing in particular, so that
// tests running in parallel cannot see each other's objects or each other's
// lifecycle rules.
func newBucket(t *testing.T) string {
	t.Helper()

	name := fmt.Sprintf("rigs3-t%d", buckets.Add(1))

	// A store with no window reaches for nothing, so this is a client rather
	// than a round trip.
	s, err := New(t.Context(), Config{
		Bucket:          name,
		Endpoint:        service(t),
		AccessKeyID:     minioUser,
		SecretAccessKey: minioSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.api.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("creating bucket %s: %v", name, err)
	}
	return name
}

// tagsOf reads an object's tags, for the assertions that are about what the
// bucket can answer rather than about what the store returned.
func tagsOf(t *testing.T, s *Store, bucket, key string) map[string]string {
	t.Helper()

	out, err := s.api.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("reading the tags of %s: %v", key, err)
	}

	tags := make(map[string]string, len(out.TagSet))
	for _, tag := range out.TagSet {
		tags[*tag.Key] = *tag.Value
	}
	return tags
}

// expireAfter puts a lifecycle rule on the bucket, which is the configuration
// New exists to refuse.
func expireAfter(t *testing.T, s *Store, bucket string, days int32) {
	t.Helper()

	_, err := s.api.PutBucketLifecycleConfiguration(t.Context(), &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: []types.LifecycleRule{{
				ID:         aws.String("expire"),
				Status:     types.ExpirationStatusEnabled,
				Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("")},
				Expiration: &types.LifecycleExpiration{Days: aws.Int32(days)},
			}},
		},
	})
	if err != nil {
		t.Fatalf("putting a %d-day lifecycle rule on %s: %v", days, bucket, err)
	}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// startMinIO brings the container up and waits until it answers its own health
// endpoint. Nothing tears it down: the container is left warm, the way every
// other suite in this repository leaves its own.
func startMinIO() (string, error) {
	ctx := context.Background()

	bin, err := runtimeBin()
	if err != nil {
		return "", err
	}
	name := qualify(minioName)

	// Objects left behind by an earlier run would make these pass for the wrong
	// reason, and a lifecycle rule left behind would make one fail for one.
	_ = exec.Command(bin, "rm", "-f", "-v", name).Run()

	out, err := exec.Command(bin, "run", "--detach",
		"--name", name,
		"--publish", publish(minioPort, 9000),
		"--env", "MINIO_ROOT_USER="+minioUser,
		"--env", "MINIO_ROOT_PASSWORD="+minioSecret,
		minioImage,
		"server", "/data",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("start the object store: %w\n%s", err, out)
	}

	port, err := publishedPort(bin, name)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitReady(ctx, url); err != nil {
		logs, _ := exec.Command(bin, "logs", "--tail", "40", name).CombinedOutput()
		return "", fmt.Errorf("%w\n%s", err, logs)
	}
	return url, nil
}

// runtimeBin picks a container engine the way internal/dockerdb does: docker,
// then podman.
func runtimeBin() (string, error) {
	for _, bin := range []string{"docker", "podman"} {
		if path, err := exec.LookPath(bin); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no container engine found; this suite needs docker or podman")
}

// qualify keeps this checkout's container away from another checkout's, by
// digesting the same variable internal/dockerdb reads.
func qualify(name string) string {
	token := strings.TrimSpace(os.Getenv("RIG_DB_ISOLATE"))
	if token == "" {
		return name
	}
	sum := sha256.Sum256([]byte(token))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// publish pins the port when this is the only checkout and lets the kernel
// choose when it is not — a port from the registry as a request rather than a
// requirement, which is the rule internal/dockerdb/isolate.go states.
func publish(host, container int) string {
	if strings.TrimSpace(os.Getenv("RIG_DB_ISOLATE")) != "" {
		return fmt.Sprintf("127.0.0.1::%d", container)
	}
	return fmt.Sprintf("127.0.0.1:%d:%d", host, container)
}

// publishedPort asks the engine which port the container really got, which
// under isolation is the only way to know.
func publishedPort(bin, name string) (int, error) {
	out, err := exec.Command(bin, "port", name, "9000/tcp").Output()
	if err != nil {
		return 0, fmt.Errorf("read the object store's port: %w", err)
	}

	// One line per binding, each `address:port`.
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	_, port, ok := strings.Cut(line, ":")
	if !ok {
		return 0, fmt.Errorf("the object store published no port that could be read back: %q", out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil {
		return 0, fmt.Errorf("the object store published %q, which is not a port", port)
	}
	return n, nil
}

// waitReady polls MinIO's own liveness endpoint. Answering the socket is not
// enough: the server binds before it has a data directory it will serve from,
// and a request in the gap fails as a connection reset.
func waitReady(ctx context.Context, url string) error {
	deadline := time.Now().Add(startWait)
	client := &http.Client{Timeout: 5 * time.Second}

	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/minio/health/live", nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("status %d", res.StatusCode)
		} else {
			last = err.Error()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("the object store was not ready within %s (last answer: %s)", startWait, last)
}
