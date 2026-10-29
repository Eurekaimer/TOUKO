// Command r2-cleanup deletes every object under a prefix from the workspace's
// configured R2 bucket. It exists so benchmark runs leave nothing behind; it
// never touches the ledger and never reads object bytes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type config struct {
	Cloudflare struct {
		Endpoint string `json:"endpoint"`
		Bucket   string `json:"bucket"`
	} `json:"cloudflare"`
}

type credentialsFile struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

func main() {
	state := flag.String("state", "", "TOUKO state directory holding config.json and cloudflare/credentials.json")
	prefix := flag.String("prefix", "", "Only delete keys with this prefix")
	dryRun := flag.Bool("dry-run", false, "List matching keys without deleting")
	flag.Parse()
	if *state == "" || *prefix == "" {
		fmt.Fprintln(os.Stderr, "usage: r2-cleanup -state DIR -prefix PREFIX [-dry-run]")
		os.Exit(2)
	}
	var cfg config
	data, err := os.ReadFile(filepath.Join(*state, "config.json"))
	if err != nil {
		fail(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		fail(err)
	}
	var creds credentialsFile
	data, err = os.ReadFile(filepath.Join(*state, "cloudflare", "credentials.json"))
	if err != nil {
		fail(err)
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		fail(err)
	}
	if cfg.Cloudflare.Endpoint == "" || cfg.Cloudflare.Bucket == "" || creds.AccessKeyID == "" {
		fail(fmt.Errorf("cloudflare endpoint, bucket or credentials are missing in %s", *state))
	}
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, nil
	})
	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: provider,
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(cfg.Cloudflare.Endpoint)
		options.UsePathStyle = true
	})
	ctx := context.Background()
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(cfg.Cloudflare.Bucket),
		Prefix: aws.String(*prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			fail(err)
		}
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}
	fmt.Printf("matched %d objects under %s\n", len(keys), *prefix)
	if *dryRun || len(keys) == 0 {
		return
	}
	for start := 0; start < len(keys); start += 1000 {
		end := min(start+1000, len(keys))
		batch := make([]types.ObjectIdentifier, 0, end-start)
		for _, key := range keys[start:end] {
			batch = append(batch, types.ObjectIdentifier{Key: aws.String(key)})
		}
		_, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(cfg.Cloudflare.Bucket),
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			fail(err)
		}
	}
	fmt.Printf("deleted %d objects\n", len(keys))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "r2-cleanup:", err)
	os.Exit(1)
}
