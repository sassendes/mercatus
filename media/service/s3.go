package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PublicURL string
	PathStyle bool
}

type S3 struct {
	cfg    S3Config
	host   string
	scheme string
	client *http.Client
}

func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid S3_ENDPOINT %q", cfg.Endpoint)
	}
	host := u.Host
	if !cfg.PathStyle {
		host = cfg.Bucket + "." + u.Host
	}
	return &S3{cfg: cfg, host: host, scheme: u.Scheme, client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func awsEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *S3) objectPath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = awsEscape(p)
	}
	path := "/" + strings.Join(parts, "/")
	if s.cfg.PathStyle {
		path = "/" + awsEscape(s.cfg.Bucket) + path
	}
	return path
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func (s *S3) Presign(method, key string, expires time.Duration, now time.Time) string {
	return s.PresignWithHeaders(method, key, expires, now, nil)
}

func (s *S3) PresignWithHeaders(method, key string, expires time.Duration, now time.Time, headers map[string]string) string {
	signed := map[string]string{"host": s.host}
	for k, v := range headers {
		signed[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	names := make([]string, 0, len(signed))
	for k := range signed {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + signed[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + s.cfg.Region + "/s3/aws4_request"
	params := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    s.cfg.AccessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(expires.Seconds())),
		"X-Amz-SignedHeaders": signedHeaders,
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := make([]string, 0, len(keys))
	for _, k := range keys {
		q = append(q, awsEscape(k)+"="+awsEscape(params[k]))
	}
	query := strings.Join(q, "&")
	path := s.objectPath(key)
	canonical := strings.Join([]string{method, path, query, canonHeaders.String(), signedHeaders, "UNSIGNED-PAYLOAD"}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	k := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), date)
	k = hmacSHA256(k, s.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return s.scheme + "://" + s.host + path + "?" + query + "&X-Amz-Signature=" + sig
}

func (s *S3) PublicURL(key string) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL + "/" + key
	}
	return s.scheme + "://" + s.host + s.objectPath(key)
}

func (s *S3) Head(ctx context.Context, key string) (size int64, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.Presign(http.MethodHead, key, time.Minute, time.Now()), nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("object not found (status %d)", resp.StatusCode)
	}
	return resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.Presign(http.MethodDelete, key, time.Minute, time.Now()), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete failed (status %d)", resp.StatusCode)
	}
	return nil
}
