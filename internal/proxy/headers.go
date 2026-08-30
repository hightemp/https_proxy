package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// viaReceivedBy is a stable pseudonym, as permitted for the Via received-by value.
const viaReceivedBy = "https_proxy"

var privacyHeaderNames = map[string]struct{}{
	"cf-connecting-ip":         {},
	"client-ip":                {},
	"do-connecting-ip":         {},
	"fastly-client-ip":         {},
	"fly-client-ip":            {},
	"forwarded":                {},
	"remote-addr":              {},
	"true-client-ip":           {},
	"via":                      {},
	"x-appengine-user-ip":      {},
	"x-azure-clientip":         {},
	"x-client-ip":              {},
	"x-cluster-client-ip":      {},
	"x-envoy-external-address": {},
	"x-forwarded":              {},
	"x-originating-ip":         {},
	"x-original-forwarded-for": {},
	"x-proxyuser-ip":           {},
	"x-real-ip":                {},
	"x-remote-addr":            {},
	"x-remote-ip":              {},
}

func removePrivacyHeaders(header http.Header) {
	for name := range header {
		normalized := strings.ToLower(name)
		_, exactMatch := privacyHeaderNames[normalized]
		if exactMatch || strings.HasPrefix(normalized, "x-forwarded-") {
			delete(header, name)
		}
	}
}

type privacyTrailerBody struct {
	io.ReadCloser
	trailer http.Header
}

func (b *privacyTrailerBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	if err == io.EOF {
		removePrivacyHeaders(b.trailer)
	}
	return read, err
}

func appendVia(header http.Header, protoMajor, protoMinor int) {
	header.Add("Via", fmt.Sprintf("%d.%d %s", protoMajor, protoMinor, viaReceivedBy))
}

func forwardedVia(header http.Header, protoMajor, protoMinor int) string {
	forwardedHeader := header.Clone()
	if forwardedHeader == nil {
		forwardedHeader = make(http.Header)
	}
	removeHopByHopHeaders(forwardedHeader)
	appendVia(forwardedHeader, protoMajor, protoMinor)
	return strings.Join(forwardedHeader.Values("Via"), ", ")
}

func headerValuesContainToken(values []string, token string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func announceTrailers(header, trailers http.Header) int {
	announced := len(trailers)
	if announced == 0 {
		return 0
	}

	keys := make([]string, 0, announced)
	for key := range trailers {
		keys = append(keys, key)
	}
	header.Add("Trailer", strings.Join(keys, ", "))
	return announced
}

func copyTrailers(header, trailers http.Header, announced int) {
	if len(trailers) == announced {
		copyHeaders(header, trailers)
		return
	}

	for key, values := range trailers {
		for _, value := range values {
			header.Add(http.TrailerPrefix+key, value)
		}
	}
}
