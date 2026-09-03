// MinIO S3 state 后端实现。
//
// AgentTeams Manager / Worker 通过 stdio MCP proxy 调 opskeeper，
// opskeeper 自己持有 MinIO SDK 依赖（避免 Manager 也要 SDK）。
// 路径约定：<bucket>/shared/opskeeper/tasks/{task_id}/state.json
//
// 生产通过 env 注入：
//
//	OPSKEEPER_MINIO_ENDPOINT=minio.opskeeper-system.svc.cluster.local:9000
//	OPSKEEPER_MINIO_ACCESS_KEY=...
//	OPSKEEPER_MINIO_SECRET_KEY=...
//	OPSKEEPER_MINIO_BUCKET=agentteams-shared
//	OPSKEEPER_MINIO_SECURE=true
package agentteams

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MinIOBackend 是直接调 MinIO S3 API 的 backend（不依赖 minio SDK）。
type MinIOBackend struct {
	endpoint   string
	accessKey  string
	secretKey  string
	bucket     string
	useSSL     bool
	httpClient *http.Client
}

// NewMinIOBackend 构造。
func NewMinIOBackend(endpoint, accessKey, secretKey, bucket string, useSSL bool) *MinIOBackend {
	return &MinIOBackend{
		endpoint:   strings.TrimRight(endpoint, "/"),
		accessKey:  accessKey,
		secretKey:  secretKey,
		bucket:     bucket,
		useSSL:     useSSL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// keyFor 返回 S3 object key。
func (b *MinIOBackend) keyFor(taskID string) string {
	return path.Join("opskeeper", "tasks", taskID, "state.json")
}

func (b *MinIOBackend) scheme() string {
	if b.useSSL {
		return "https"
	}
	return "http"
}

// Get 读 state.json。
func (b *MinIOBackend) Get(ctx context.Context, taskID string) ([]byte, error) {
	key := b.keyFor(taskID)
	u := fmt.Sprintf("%s://%s/%s/%s", b.scheme(), b.endpoint, b.bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("minio Get: %w", err)
	}
	if err := b.signRequest(req, nil); err != nil {
		return nil, fmt.Errorf("minio sign: %w", err)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minio Get http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrStateNotFound
	}
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("minio Get: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	return io.ReadAll(resp.Body)
}

// Put 写 state.json（覆盖写，不带版本校验；CAS 由上层 CASLock 处理）。
func (b *MinIOBackend) Put(ctx context.Context, taskID string, body []byte) error {
	key := b.keyFor(taskID)
	u := fmt.Sprintf("%s://%s/%s/%s", b.scheme(), b.endpoint, b.bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("minio Put: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := b.signRequest(req, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("minio sign: %w", err)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("minio Put http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("minio Put: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// signRequest 给 MinIO/S3 请求加 AWS Signature V4。
//
// 实现简化版：使用标准 hash 库做 SHA-256，构造 canonical request → string to sign →
// signing key → signature。完整实现参考 AWS docs。
//
// 注意：本实现是「能跑通 MinIO 自部署版本」的最小可用版本，**不覆盖**：
//   - chunked upload
//   - SSE-KMS / SSE-S3
//   - presigned URL
//   - virtual-host style addressing
//
// 生产建议改用 minio-go SDK 或 aws-sdk-go-v2。
func (b *MinIOBackend) signRequest(req *http.Request, body io.Reader) error {
	const service = "s3"
	const region = "us-east-1"
	const algorithm = "AWS4-HMAC-SHA256"

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	host := req.URL.Host
	canonicalURI := req.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQueryString := canonicalQuery(req.URL.Query())

	// 必需头
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// 计算 body hash
	var bodyHash string
	if body == nil {
		bodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // empty SHA-256
	} else {
		h := sha256.New()
		_, _ = io.Copy(h, body)
		bodyHash = hex.EncodeToString(h.Sum(nil))
	}
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)

	// canonical headers（小写 key，trim value，按 key 排序 — AWS SigV4 要求）
	sortedKeys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	var canonicalHeaders strings.Builder
	signedHeaders := make([]string, 0, len(sortedKeys))
	for _, k := range sortedKeys {
		v := req.Header[k]
		canonicalHeaders.WriteString(strings.ToLower(k))
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(strings.Join(v, ",")))
		canonicalHeaders.WriteString("\n")
		signedHeaders = append(signedHeaders, strings.ToLower(k))
	}
	signedHeadersStr := strings.Join(signedHeaders, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeadersStr,
		bodyHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		credentialScope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	signingKey := deriveSigningKey(b.secretKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, b.accessKey, credentialScope, signedHeadersStr, signature,
	)
	req.Header.Set("Authorization", authHeader)
	return nil
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		for _, v := range q[k] {
			b.WriteString(url.QueryEscape(k))
			b.WriteString("=")
			b.WriteString(url.QueryEscape(v))
			b.WriteString("&")
		}
	}
	return strings.TrimRight(b.String(), "&")
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = io.WriteString(h, string(data))
	return h.Sum(nil)
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// 确保 ErrStateNotFound 可被 errors.Is 命中
var _ = errors.Is
var _ = hash.Hash(nil)
var _ = strconv.Itoa
