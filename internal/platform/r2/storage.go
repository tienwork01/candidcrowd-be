package r2

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
)

type ObjectInfo = media.ObjectInfo

type Client struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
	uploader  *manager.Uploader
}

type R2 = Client

// Media keys are immutable: a completed upload is never overwritten. Browser
// caches may therefore retain an object safely, but must not share it through
// intermediary caches because access is granted by a signed URL.
const privateMediaCacheControl = "private, max-age=1800"

func New(ctx context.Context, endpoint, region, bucket, accessKey, secret string) (*Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secret, "")))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &Client{
		bucket:    bucket,
		client:    client,
		presigner: s3.NewPresignClient(client),
		uploader:  manager.NewUploader(client),
	}, nil
}

func (c *Client) PresignPut(ctx context.Context, key, mime string, expiry time.Duration) (string, error) {
	out, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key), ContentType: aws.String(mime), CacheControl: aws.String(privateMediaCacheControl)}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *Client) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	out, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key), ResponseCacheControl: aws.String(privateMediaCacheControl)}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *Client) PresignDownload(ctx context.Context, key, filename string, expiry time.Duration) (string, error) {
	input := &s3.GetObjectInput{
		Bucket:               aws.String(c.bucket),
		Key:                  aws.String(key),
		ResponseCacheControl: aws.String(privateMediaCacheControl),
	}
	if filename != "" {
		input.ResponseContentDisposition = aws.String(fmt.Sprintf("attachment; filename=%q", filename))
	} else {
		input.ResponseContentDisposition = aws.String("attachment")
	}
	out, err := c.presigner.PresignGetObject(ctx, input, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *Client) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		return ObjectInfo{}, err
	}
	if out.ContentLength == nil {
		return ObjectInfo{}, fmt.Errorf("object has no content length")
	}
	return ObjectInfo{Size: *out.ContentLength, ContentType: aws.ToString(out.ContentType)}, nil
}

func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	return err
}

// maxDeleteBatch is the ceiling the S3 DeleteObjects API imposes per request.
const maxDeleteBatch = 1000

// DeleteMany removes objects in batches and returns the keys that survived.
//
// A whole batch failing is reported by returning every key in it: the caller
// decides what to do, and its only correct move either way is to leave those
// database rows alone so the next run tries again.
func (c *Client) DeleteMany(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	var failed []string
	var firstErr error
	for start := 0; start < len(keys); start += maxDeleteBatch {
		end := start + maxDeleteBatch
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]

		objects := make([]types.ObjectIdentifier, 0, len(chunk))
		for _, key := range chunk {
			objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
		}
		out, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			// Quiet asks the provider to report only the failures.
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			failed = append(failed, chunk...)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, deleteErr := range out.Errors {
			failed = append(failed, aws.ToString(deleteErr.Key))
			if firstErr == nil {
				firstErr = fmt.Errorf("delete %s: %s", aws.ToString(deleteErr.Key), aws.ToString(deleteErr.Message))
			}
		}
	}
	return failed, firstErr
}

// DeletePrefix removes every object whose key starts with prefix and reports
// how many were removed. Keys are listed and deleted a page at a time, so an
// event with tens of thousands of objects never holds them all in memory.
//
// prefix must name a directory-like scope (ending in "/"); an empty or
// root-level prefix is refused so a bug can never empty the bucket.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if !strings.HasSuffix(prefix, "/") || strings.Count(prefix, "/") < 2 {
		return 0, fmt.Errorf("r2: refusing to delete unscoped prefix %q", prefix)
	}
	deleted := 0
	paginator := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, err
		}
		keys := make([]string, 0, len(page.Contents))
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		failed, err := c.DeleteMany(ctx, keys)
		deleted += len(keys) - len(failed)
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

func (c *Client) Put(ctx context.Context, key, mime string, body io.Reader) error {
	_, err := c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(c.bucket),
		Key:          aws.String(key),
		Body:         body,
		ContentType:  aws.String(mime),
		CacheControl: aws.String(privateMediaCacheControl),
	})
	return err
}

func (c *Client) OpenRead(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	if out.ContentLength == nil {
		_ = out.Body.Close()
		return nil, ObjectInfo{}, fmt.Errorf("object has no content length")
	}
	return out.Body, ObjectInfo{Size: *out.ContentLength, ContentType: aws.ToString(out.ContentType)}, nil
}
