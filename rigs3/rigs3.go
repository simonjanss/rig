// Package rigs3 keeps a rig project's uploads in S3, or in anything speaking
// its API.
//
// It is [blob.Store] and both of its optional halves — [blob.Signer] and
// [blob.Marker] — and it is a module of its own for one reason: the AWS SDK.
// [github.com/simonjanss/rig/files] depends on the runtime and a UUID, and
// every generated application imports it, so a project keeping its uploads in
// memory must not compile an S3 client to do it. Depending on this module is
// therefore the same decision as writing `backend: s3` in rig.yaml, and it is
// made in one place — the `files.gen.go` that `server-go` writes.
//
// The name has an `s3` in it twice on purpose. This package's whole job is to
// import [github.com/aws/aws-sdk-go-v2/service/s3], and a package called `s3`
// importing `s3` would have to alias one of them in every file.
//
// # What the bucket has to allow
//
// Reading and writing objects, and reading and writing their tags: the
// checksum and the deleted mark are both object tags, for reasons given on
// [Store.Put] and [Store.Mark]. [New] also reads the bucket's lifecycle
// configuration once, at startup, and refuses to build a store when a rule
// would expire objects sooner than the restore window says they are
// restorable.
package rigs3

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/simonjanss/rig/files/blob"
)

// Config is what a store needs to know about the bucket it writes to.
//
// The credentials are values rather than the names of environment variables,
// which is the opposite of how rig.yaml carries them: `files.s3.access_key_env`
// names a variable because rig.yaml is a file people commit, and reading it is
// the generated code's job. This struct is the other side of that read, and a
// program building a store by hand should not have to invent an environment to
// pass a string through.
type Config struct {
	// Bucket is the bucket objects go in. Required: there is no default worth
	// guessing and a wrong one is somebody else's data.
	Bucket string

	// Region is the bucket's region. Required for AWS, which will not guess
	// one; with an Endpoint set it defaults to us-east-1, which is what
	// S3-compatible services that have no regions expect to be told.
	Region string

	// Endpoint points at something other than AWS — MinIO, R2, a test double.
	//
	// Setting it also turns on path-style addressing, because virtual-host
	// style asks DNS to resolve `<bucket>.<endpoint>` and almost nothing but
	// AWS answers that. It is not a separate setting for the same reason: there
	// is no combination of the two worth writing down.
	Endpoint string

	// AccessKeyID and SecretAccessKey are static credentials. Both empty means
	// the AWS default chain — an instance profile, IRSA, SSO, the environment —
	// which is what a real deployment usually wants and what a laptop with
	// `aws configure` already has.
	AccessKeyID     string
	SecretAccessKey string

	// RestoreWindow is `files.restore_window`: how long a deleted file stays
	// restorable, and therefore how long its bytes have to survive the delete.
	//
	// It is here so that [New] can refuse a bucket whose own lifecycle rule
	// would take them sooner. Zero means the check is skipped, which is what a
	// program that is not rig's generated wiring should pass.
	RestoreWindow time.Duration
}

// keyPrefix is what every key [blob.Key] derives starts with.
//
// It is spelled out rather than derived because it is compared against bucket
// lifecycle rules, where a prefix is a string and not a function.
// TestTheKeyPrefixIsStillRight is what keeps the two in step.
const keyPrefix = "files/"

// The tags rig writes on every object it stores.
//
// Tags rather than user metadata, and that is not a preference: metadata is
// fixed when an upload begins and the checksum is not known until the last byte
// has gone past. See [Store.Put].
const (
	// tagChecksum carries the hex SHA-256 [blob.Info] reports.
	tagChecksum = "rig-sha256"
	// tagState is live or deleted, mirroring the row. See [Store.Mark].
	tagState = "rig-state"
	// tagMarkedAt is when the state was last written, in RFC 3339.
	tagMarkedAt = "rig-marked-at"
)

// defaultRegion is what an endpoint-addressed service is told when the
// configuration names no region. MinIO and most of the compatible services have
// no regions at all and accept this one; the SDK will not sign without any.
const defaultRegion = "us-east-1"

// Store is a bucket, as [blob.Store].
type Store struct {
	api      *s3.Client
	uploader *transfermanager.Client
	presign  *s3.PresignClient
	bucket   string
}

// The three interfaces this store is, checked here so that dropping one is a
// compile error rather than a type assertion that quietly stops matching.
var (
	_ blob.Store  = (*Store)(nil)
	_ blob.Marker = (*Store)(nil)
	_ blob.Signer = (*Store)(nil)
)

// New builds a store, and refuses rather than returning one that cannot keep
// the promises rig makes about it.
//
// It fails on a missing bucket, on a bucket that names neither a region nor an
// endpoint, and — the one worth knowing about — on a bucket whose lifecycle
// configuration would expire objects sooner than [Config.RestoreWindow] says
// they are restorable. That last one costs a round trip at startup and buys the
// failure arriving on the first deploy instead of a month later, when somebody
// undeletes a file and gets back a row pointing at nothing.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("rigs3: no bucket")
	}
	region := cfg.Region
	if region == "" {
		if cfg.Endpoint == "" {
			return nil, errors.New("rigs3: no region, and no endpoint to imply one")
		}
		region = defaultRegion
	}

	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("rigs3: the AWS configuration could not be loaded: %w", err)
	}

	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	})

	s := &Store{
		api:      api,
		uploader: transfermanager.New(api),
		presign:  s3.NewPresignClient(api),
		bucket:   cfg.Bucket,
	}

	if cfg.RestoreWindow > 0 {
		if err := s.checkLifecycle(ctx, cfg.RestoreWindow); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// checkLifecycle refuses a bucket that would delete the bytes before the
// restore window closes.
//
// The rule this exists for is the one somebody adds on purpose: tag the object
// deleted, and let the bucket expire it without rig having to. Set that expiry
// to seven days while `files.restore_window` is thirty and a restore inside the
// window succeeds and hands back a row pointing at nothing — the exact failure
// the sweeper's two rules were written to avoid, arriving through the bucket
// instead. rig cannot enforce a policy on a bucket it does not own, so it reads
// it and refuses.
//
// What it cannot check, and what the error therefore says: S3 counts those days
// from when the object was *written*, not from when it was marked deleted. A
// rule that only just outlives the window still expires a file uploaded a year
// ago the moment it is deleted. A rule comfortably longer than the window, or
// no rule at all, is the configuration that holds.
func (s *Store) checkLifecycle(ctx context.Context, window time.Duration) error {
	out, err := s.api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "NoSuchLifecycleConfiguration" {
			return nil
		}
		return fmt.Errorf("rigs3: the lifecycle configuration of bucket %s could not be read: %w", s.bucket, err)
	}

	if days, short := tooShort(out.Rules, window); short {
		return fmt.Errorf(
			"rigs3: bucket %s expires objects after %d days and files.restore_window is %s, "+
				"so a file restored inside its window would point at nothing; "+
				"the lifecycle rule has to outlive the window — and by more than a day, "+
				"because S3 counts those days from when an object was written rather than "+
				"from when it was deleted",
			s.bucket, days, window)
	}
	return nil
}

// tooShort is the whole of the decision, away from the call that fetched the
// rules: which of them apply, and whether any expires sooner than the window.
//
// Three kinds are left alone. A disabled rule is not applying. A date-based
// expiry is a one-off rather than a window and has nothing to be compared
// against. And a rule whose prefix cannot reach any key [blob.Key] derives is
// about somebody else'"'"'s objects.
func tooShort(rules []types.LifecycleRule, window time.Duration) (int32, bool) {
	want := windowDays(window)
	for _, r := range rules {
		if r.Status != types.ExpirationStatusEnabled {
			continue
		}
		if r.Expiration == nil || r.Expiration.Days == nil {
			continue
		}
		if !ruleReaches(r) {
			continue
		}
		if int64(*r.Expiration.Days) < want {
			return *r.Expiration.Days, true
		}
	}
	return 0, false
}

// windowDays is the restore window in whole days, rounded up.
//
// Up rather than down, which is the opposite of
// [github.com/simonjanss/rig/internal/project.Files.RestoreWindowDays] and for
// the same reason: there the row must stop being restorable before the bytes
// go, and here the bytes must outlast the row.
func windowDays(window time.Duration) int64 {
	return int64(math.Ceil(window.Hours() / 24))
}

// ruleReaches reports whether a lifecycle rule could apply to an object rig
// wrote.
//
// Only the prefix is read, and everything else is assumed to match. A rule
// filtered by tag is the shape somebody uses to expire what rig marked deleted,
// which is precisely the rule that has to be checked; a rule filtered by size
// could reach any object at all. Guessing narrowly here would let the dangerous
// one through, and the cost of guessing widely is a startup error somebody
// answers by widening a rule they meant to widen anyway.
func ruleReaches(r types.LifecycleRule) bool {
	prefix := ""
	switch {
	case r.Filter != nil && r.Filter.Prefix != nil:
		prefix = *r.Filter.Prefix
	case r.Filter != nil && r.Filter.And != nil && r.Filter.And.Prefix != nil:
		prefix = *r.Filter.And.Prefix

	// The pre-Filter spelling. Deprecated by S3 for writing and still what it
	// answers with for every rule written before Filter existed, which is the
	// only reason this reads it: a check that ignored the old spelling would
	// pass on the oldest and most likely bucket to have one.
	case r.Prefix != nil: //nolint:staticcheck // reading what S3 still returns
		prefix = *r.Prefix //nolint:staticcheck // as above
	}
	return strings.HasPrefix(keyPrefix, prefix) || strings.HasPrefix(prefix, keyPrefix)
}
