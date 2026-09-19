package r2

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
)

type ObjectInfo = media.ObjectInfo

type Client struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
}

type R2 = Client

func New(ctx context.Context, endpoint, region, bucket, accessKey, secret string) (*Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secret, "")))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
	return &Client{bucket: bucket, client: client, presigner: s3.NewPresignClient(client)}, nil
}

func (c *Client) PresignPut(ctx context.Context, key, mime string, expiry time.Duration) (string, error) {
	out, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key), ContentType: aws.String(mime)}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *Client) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	out, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}, s3.WithPresignExpires(expiry))
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
