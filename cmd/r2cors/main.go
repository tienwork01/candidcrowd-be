package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	endpoint := os.Getenv("R2_ENDPOINT")
	region := os.Getenv("R2_REGION")
	bucket := os.Getenv("R2_BUCKET")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secret := os.Getenv("R2_SECRET_ACCESS_KEY")

	if endpoint == "" || bucket == "" || accessKey == "" || secret == "" {
		log.Fatal("Missing required R2 configuration (R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY)")
	}

	ctx := context.Background()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secret, "")),
	)
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	corsRules := []types.CORSRule{
		{
			AllowedOrigins: []string{
				"http://localhost:3000",
				"https://localhost:3000",
				"http://127.0.0.1:3000",
				"https://127.0.0.1:3000",
				"*",
			},
			AllowedMethods: []string{
				"GET",
				"PUT",
				"POST",
				"HEAD",
			},
			AllowedHeaders: []string{
				"*",
			},
			ExposeHeaders: []string{
				"ETag",
				"Content-Type",
				"Content-Length",
			},
			MaxAgeSeconds: aws.Int32(3600),
		},
	}

	fmt.Printf("Setting CORS configuration on bucket '%s'...\n", bucket)
	_, err = client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(bucket),
		CORSConfiguration: &types.CORSConfiguration{
			CORSRules: corsRules,
		},
	})
	if err != nil {
		log.Fatalf("Failed to put bucket CORS: %v", err)
	}
	fmt.Println("Successfully configured bucket CORS!")

	// Verify CORS configuration
	out, err := client.GetBucketCors(ctx, &s3.GetBucketCorsInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		log.Fatalf("Failed to get bucket CORS: %v", err)
	}

	fmt.Printf("Current CORS Rules on '%s':\n", bucket)
	for i, rule := range out.CORSRules {
		fmt.Printf("Rule %d:\n", i+1)
		fmt.Printf("  AllowedOrigins: %v\n", rule.AllowedOrigins)
		fmt.Printf("  AllowedMethods: %v\n", rule.AllowedMethods)
		fmt.Printf("  AllowedHeaders: %v\n", rule.AllowedHeaders)
	}
}
