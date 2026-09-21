package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// AWS4-HMAC-SHA256 request signing. Garage accepts nothing else, and it is
// unforgiving about the canonical form: the URI has to carry AWS's own
// percent-encoding, not Go's, so uriEncodePath below is the authority on both
// the string that gets signed and the string that goes on the wire.

const algorithm = "AWS4-HMAC-SHA256"

// sign adds X-Amz-Date, X-Amz-Content-Sha256 and Authorization to req.
func (c *Client) sign(req *http.Request, payload []byte) {
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])

	now := c.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Only these three are signed. Everything else a proxy might add stays out of
	// the signature, which is what keeps this working over the tailnet.
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	uri := req.URL.RawPath
	if uri == "" {
		uri = uriEncodePath(req.URL.Path)
	}
	// req.URL.RawQuery is already canonical: url() built it with canonicalQuery.
	sig, scope := signature(c.secret, c.region, "s3", dateStamp, amzDate, req.Method, uri,
		req.URL.RawQuery, canonicalHeaders, signedHeaders, payloadHash)

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, c.access, scope, signedHeaders, sig))
}

// signature is the algorithm itself, kept separate from the request so that a
// test can drive it with AWS's published example (which signs a header set this
// client never sends) and check the result against the documented signature.
func signature(secret, region, service, dateStamp, amzDate, method, uri, query,
	canonicalHeaders, signedHeaders, payloadHash string) (sig, scope string) {
	canonicalRequest := strings.Join([]string{
		method, uri, query, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	crSum := sha256.Sum256([]byte(canonicalRequest))
	scope = dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		algorithm, amzDate, scope, hex.EncodeToString(crSum[:]),
	}, "\n")
	key := signingKey(secret, dateStamp, region, service)
	return hex.EncodeToString(hmacSHA256(key, stringToSign)), scope
}

// signingKey is the four-step HMAC chain from the secret to a key scoped to one
// day, region and service.
func signingKey(secret, dateStamp, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// uriEncode is AWS's encoding: every byte outside the unreserved set becomes
// %XX with upper-case hex. In a path the separators stay bare; in a query value
// they do not.
func uriEncode(s string, keepSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && keepSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[ch>>4])
			b.WriteByte(upperhex[ch&0xf])
		}
	}
	return b.String()
}

func uriEncodePath(p string) string { return uriEncode(p, true) }

// canonicalQuery is the query string sorted by name then value, with both
// encoded. Only ListObjectsV2 uses one, but its continuation token is base64 and
// does contain characters that have to be escaped.
func canonicalQuery(q url.Values) string {
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}
