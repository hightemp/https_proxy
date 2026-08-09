package proxy

import (
	"fmt"
	"net/http"
	"strings"
)

// viaReceivedBy is a stable pseudonym, as permitted for the Via received-by value.
const viaReceivedBy = "https_proxy"

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
