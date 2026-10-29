package r2

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const (
	headTimeout = 15 * time.Second
	putTimeout  = 5 * time.Minute

	metadataRepositoryID = "touko-repository-id"
	metadataSourceID     = "touko-source-id"
	metadataSHA256       = "touko-sha256"
)

// Error is the only R2 failure type exposed by this package. It intentionally
// carries no request URL, response body, Authorization header or credential:
// Error() only renders the operation, the object key, the controlled code and
// the HTTP status. Cause is kept for errors.Is/As without being printed.
type Error struct {
	Op         string
	Key        string
	Code       string
	HTTPStatus int
	Cause      error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("r2")
	if e.Op != "" {
		b.WriteByte(' ')
		b.WriteString(e.Op)
	}
	if e.Key != "" {
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(e.Key))
	}
	if e.Code != "" {
		b.WriteString(": ")
		b.WriteString(e.Code)
	}
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.HTTPStatus)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// ErrorCode returns the controlled code of an R2 failure. Errors that did not
// originate from the adapter are treated as transport failures.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var r2err *Error
	if errors.As(err, &r2err) {
		if r2err.Code == "" {
			return "network"
		}
		return r2err.Code
	}
	return "network"
}

type ObjectInfo struct {
	ETag         string
	RepositoryID string
	SourceID     string
	SHA256       string
	Size         int64
}

type PutOptions struct {
	Object       domain.ContentObject
	RepositoryID domain.RepositoryID
	SourceID     string
	IfMatch      string
}

type Mirror interface {
	HeadObject(context.Context, string) (ObjectInfo, bool, error)
	PutObject(context.Context, string, io.ReadSeeker, PutOptions) (etag string, err error)
	DeleteObject(context.Context, string) error
}

// Client is a bucket-scoped S3 client for Cloudflare R2. AWS SDK types never
// leave this package.
type Client struct {
	api    *s3.Client
	bucket string
}

var _ Mirror = (*Client)(nil)

// New builds a client with the production HTTP transport. The endpoint must be
// an absolute HTTPS URL; there is no insecure option.
func New(cfg Config, accessKeyID, secretAccessKey string) (*Client, error) {
	return NewWithClient(cfg, accessKeyID, secretAccessKey, &http.Client{})
}

// NewWithClient builds a client around a copy of the supplied HTTP client. The
// copy never follows redirects, so credentials and object bytes cannot be
// forwarded to another origin.
func NewWithClient(cfg Config, accessKeyID, secretAccessKey string, client *http.Client) (*Client, error) {
	if client == nil {
		return nil, configFailure(errors.New("http client is required"))
	}
	if err := cfg.Validate(); err != nil {
		return nil, configFailure(err)
	}
	if !cfg.Enabled {
		return nil, configFailure(errors.New("cloudflare mirror is disabled"))
	}
	if !validCredential(accessKeyID) {
		return nil, &Error{Op: "config", Code: "credentials_missing", Cause: errors.New("missing or invalid R2 access key id")}
	}
	if !validCredential(secretAccessKey) {
		return nil, &Error{Op: "config", Code: "credentials_missing", Cause: errors.New("missing or invalid R2 secret access key")}
	}
	endpoint := cfg.NormalizedEndpoint()

	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	credentials := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}, nil
	})
	awsConfig := aws.Config{
		Region:                     "auto",
		Credentials:                credentials,
		HTTPClient:                 &copyClient,
		RetryMaxAttempts:           1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		BaseEndpoint:               aws.String(endpoint),
	}
	api := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.Region = "auto"
		options.Credentials = credentials
		options.HTTPClient = &copyClient
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
		options.RetryMaxAttempts = 1
		options.ContinueHeaderThresholdBytes = -1
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		options.DisableLogOutputChecksumValidationSkipped = true
		options.ClientLogMode = 0
	})
	return &Client{api: api, bucket: cfg.Bucket}, nil
}

func (c *Client) HeadObject(ctx context.Context, key string) (ObjectInfo, bool, error) {
	if err := validateKey("head", key); err != nil {
		return ObjectInfo{}, false, err
	}
	requestContext, cancel := context.WithTimeout(ctx, headTimeout)
	defer cancel()
	output, err := c.api.HeadObject(requestContext, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return objectInfo(output), true, nil
	}
	if httpStatus(err) != http.StatusNotFound {
		return ObjectInfo{}, false, classify("head", key, err)
	}
	if err := c.probeBucket(requestContext); err != nil {
		return ObjectInfo{}, false, err
	}
	return ObjectInfo{}, false, nil
}

func (c *Client) PutObject(ctx context.Context, key string, body io.ReadSeeker, opts PutOptions) (string, error) {
	if err := validateKey("put", key); err != nil {
		return "", err
	}
	if body == nil {
		return "", configFailure(errors.New("object body is required"))
	}
	if err := validateObject(opts); err != nil {
		return "", configFailure(err)
	}
	if opts.IfMatch != "" && !validETag(opts.IfMatch) {
		return "", configFailure(errors.New("invalid If-Match ETag"))
	}

	input := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(opts.Object.Size),
		ContentType:   aws.String(opts.Object.MIME),
		Metadata: map[string]string{
			metadataRepositoryID: opts.RepositoryID.String(),
			metadataSourceID:     opts.SourceID,
			metadataSHA256:       opts.Object.Hash.String(),
		},
	}
	if opts.IfMatch == "" {
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = aws.String(strconv.Quote(opts.IfMatch))
	}

	requestContext, cancel := context.WithTimeout(ctx, putTimeout)
	defer cancel()
	output, err := c.api.PutObject(requestContext, input)
	if err != nil {
		return "", classify("put", key, err)
	}
	etag := normalizedETag(aws.ToString(output.ETag))

	// A successful HTTP response is not sufficient evidence: read the object
	// back and require the bytes description and ownership metadata to match.
	info, exists, err := c.HeadObject(requestContext, key)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", &Error{Op: "put", Key: key, Code: "object_conflict", Cause: errors.New("uploaded object is missing on read-back")}
	}
	if info.Size != opts.Object.Size ||
		info.SHA256 != opts.Object.Hash.String() ||
		info.RepositoryID != opts.RepositoryID.String() ||
		info.SourceID != opts.SourceID {
		return "", &Error{Op: "put", Key: key, Code: "object_conflict", Cause: errors.New("uploaded object metadata does not match the expected description")}
	}
	if etag == "" {
		etag = info.ETag
	}
	return etag, nil
}

func (c *Client) DeleteObject(ctx context.Context, key string) error {
	if err := validateKey("delete", key); err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, headTimeout)
	defer cancel()
	_, err := c.api.DeleteObject(requestContext, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil
	}
	if httpStatus(err) == http.StatusNotFound {
		// S3 reports a missing key with 204. A 404 therefore indicates a
		// missing bucket or a permission problem, not an already deleted key.
		if err := c.probeBucket(requestContext); err != nil {
			return err
		}
		return nil
	}
	return classify("delete", key, err)
}

func (c *Client) probeBucket(ctx context.Context) error {
	requestContext, cancel := context.WithTimeout(ctx, headTimeout)
	defer cancel()
	_, err := c.api.HeadBucket(requestContext, &s3.HeadBucketInput{Bucket: aws.String(c.bucket)})
	if err == nil {
		return nil
	}
	return classify("head_bucket", c.bucket, err)
}

func objectInfo(output *s3.HeadObjectOutput) ObjectInfo {
	return ObjectInfo{
		ETag:         normalizedETag(aws.ToString(output.ETag)),
		RepositoryID: output.Metadata[metadataRepositoryID],
		SourceID:     output.Metadata[metadataSourceID],
		SHA256:       output.Metadata[metadataSHA256],
		Size:         aws.ToInt64(output.ContentLength),
	}
}

func validateKey(op, key string) error {
	if err := domain.ValidateRepositoryPath(key); err != nil {
		return &Error{Op: op, Key: key, Code: "configuration_invalid", Cause: err}
	}
	return nil
}

func validateObject(opts PutOptions) error {
	if err := opts.Object.Validate(); err != nil {
		return err
	}
	if !opts.Object.HasHash() {
		return errors.New("object hash is required")
	}
	if opts.RepositoryID == (domain.RepositoryID{}) {
		return errors.New("repository id is required")
	}
	if !validSourceID(opts.SourceID) {
		return errors.New("source id must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func validSourceID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validCredential(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validETag(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

func normalizedETag(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "W/")
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return value
}

func configFailure(cause error) *Error {
	return &Error{Op: "config", Code: "configuration_invalid", Cause: cause}
}

func httpStatus(err error) int {
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		return responseError.HTTPStatusCode()
	}
	var statusError interface{ HTTPStatusCode() int }
	if errors.As(err, &statusError) {
		return statusError.HTTPStatusCode()
	}
	return 0
}

func classify(op, key string, err error) *Error {
	classified := &Error{Op: op, Key: key, Cause: err}
	if errors.Is(err, context.Canceled) {
		classified.Code = "canceled"
		return classified
	}
	if errors.Is(err, context.DeadlineExceeded) {
		classified.Code = "timeout"
		return classified
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		classified.Code = "timeout"
		return classified
	}

	status := httpStatus(err)
	classified.HTTPStatus = status
	switch status {
	case http.StatusNotFound:
		switch op {
		case "head_bucket", "put", "delete":
			classified.Code = "bucket_unavailable"
		default:
			classified.Code = "not_found"
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		classified.Code = "access_denied"
	case http.StatusConflict:
		classified.Code = "object_conflict"
	case http.StatusPreconditionFailed:
		classified.Code = "precondition_failed"
	case http.StatusBadRequest:
		classified.Code = "configuration_invalid"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		classified.Code = "timeout"
	default:
		classified.Code = "network"
	}
	return classified
}
