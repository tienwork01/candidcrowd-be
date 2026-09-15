package r2

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type ObjectInfo struct {
	Size        int64
	ContentType string
}
type Storage interface {
	PresignPut(context.Context, string, string, time.Duration) (string, error)
	Head(context.Context, string) (ObjectInfo, error)
}
type R2 struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
}

func New(ctx context.Context, endpoint, region, bucket, accessKey, secret string) (*R2, error) {
	cfg, e := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secret, "")))
	if e != nil {
		return nil, e
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
	return &R2{bucket, client, s3.NewPresignClient(client)}, nil
}
func (r *R2) PresignPut(ctx context.Context, key, mime string, expiry time.Duration) (string, error) {
	out, e := r.presigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key), ContentType: aws.String(mime)}, s3.WithPresignExpires(expiry))
	if e != nil {
		return "", e
	}
	return out.URL, nil
}
func (r *R2) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, e := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if e != nil {
		return ObjectInfo{}, e
	}
	if out.ContentLength == nil {
		return ObjectInfo{}, fmt.Errorf("object has no content length")
	}
	return ObjectInfo{Size: *out.ContentLength, ContentType: aws.ToString(out.ContentType)}, nil
}
func ValidURL(value string) bool { _, e := url.ParseRequestURI(value); return e == nil }
