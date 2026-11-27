package provision

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// AliyunSignature computes the Alibaba Cloud RPC signature (HMAC-SHA1,
// base64) for the canonicalized query parameters.
func AliyunSignature(params url.Values, secretKey, method string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(awsPercentEncode(k))
		canonical.WriteByte('=')
		canonical.WriteString(awsPercentEncode(params.Get(k)))
	}
	stringToSign := method + "&" + awsPercentEncode("/") + "&" + awsPercentEncode(canonical.String())
	mac := hmac.New(sha1.New, []byte(secretKey+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
