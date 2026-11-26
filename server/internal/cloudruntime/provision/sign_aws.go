package provision

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// awsSigningKey derives the Signature V4 key for the given scope.
func awsSigningKey(secret, dateStamp, region, service string) []byte {
	key := []byte("AWS4" + secret)
	for _, step := range []string{dateStamp, region, service, "aws4_request"} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(step))
		key = mac.Sum(nil)
	}
	return key
}

func awsPercentEncode(value string) string {
	// url.PathEscape leaves ~ unescaped, matching the RFC 3986 unreserved set.
	return strings.ReplaceAll(url.PathEscape(value), "+", "%20")
}

// awsCanonicalRequest builds the canonical request and returns it together
// with the signed (lower-cased) header names.
func awsCanonicalRequest(method string, u *url.URL, query url.Values, headers map[string]string, body []byte) (string, []string) {
	signed := make(map[string]string, len(headers)+1)
	for k, v := range headers {
		signed[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	signed["host"] = u.Host
	names := make([]string, 0, len(signed))
	for name := range signed {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(signed[name])
		canonicalHeaders.WriteByte('\n')
	}

	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonicalQuery strings.Builder
	for i, k := range keys {
		if i > 0 {
			canonicalQuery.WriteByte('&')
		}
		canonicalQuery.WriteString(awsPercentEncode(k))
		canonicalQuery.WriteByte('=')
		canonicalQuery.WriteString(awsPercentEncode(query.Get(k)))
	}

	payloadHash := sha256.Sum256(body)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		method,
		path,
		canonicalQuery.String(),
		canonicalHeaders.String(),
		strings.Join(names, ";"),
		hex.EncodeToString(payloadHash[:]),
	}, "\n")
	return canonical, names
}

// AWSSignRequest returns the full header set (including Authorization) for a
// Signature V4 request. sessionToken may be empty.
func AWSSignRequest(method, rawURL string, query url.Values, headers map[string]string, body []byte,
	accessKey, secretKey, sessionToken, region, service, amzDate string) (map[string]string, error) {

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(headers)+3)
	for k, v := range headers {
		result[k] = v
	}
	result["X-Amz-Date"] = amzDate
	if sessionToken != "" {
		result["X-Amz-Security-Token"] = sessionToken
	}

	canonical, names := awsCanonicalRequest(method, parsed, query, result, body)
	scope := strings.Join([]string{amzDate[:8], region, service, "aws4_request"}, "/")
	canonicalHash := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(canonicalHash[:]),
	}, "\n")
	signature := hmac.New(sha256.New, awsSigningKey(secretKey, amzDate[:8], region, service))
	signature.Write([]byte(stringToSign))

	result["Authorization"] = "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope +
		", SignedHeaders=" + strings.Join(names, ";") +
		", Signature=" + hex.EncodeToString(signature.Sum(nil))
	return result, nil
}
