package statemigration

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Cloudflare SDK v7.13.1 Error.Error starts with METHOD "URL": STATUS TEXT.
// Match only that prefix; response bodies cannot select a public status.
var providerHTTPPrefix = regexp.MustCompile(`^(?:GET|HEAD|POST|PUT|PATCH|DELETE|CONNECT|OPTIONS|TRACE) "https?://[^"\r\n[:space:]]+": ([45][0-9]{2}) `)

// Go's url.Error quotes the URL and prefixes the transport error with the operation.
var providerTransportPrefix = regexp.MustCompile(`^(?:Get|Head|Post|Put|Patch|Delete|Connect|Options|Trace) "(?:[^"\\\r\n]|\\.)+": (.+)$`)

func providerHTTPDiagnostic(detail string) (int, string, []int64) {
	if prefix := providerHTTPPrefix.FindStringSubmatchIndex(detail); prefix != nil {
		status, err := strconv.Atoi(detail[prefix[2]:prefix[3]])
		if err != nil {
			return 0, "", nil
		}
		text := http.StatusText(status)
		if text != "" && strings.HasPrefix(detail[prefix[1]:], text+" ") {
			body := detail[prefix[1]+len(text)+1:]
			return status, "", providerAPIErrorCodes(body)
		}
		return 0, "", nil
	}
	transport := providerTransportPrefix.FindStringSubmatch(detail)
	if transport == nil {
		return 0, "", nil
	}
	message := transport[1]
	switch {
	case strings.HasPrefix(message, "dial tcp ") && strings.HasSuffix(message, ": connect: connection refused"):
		return 0, "connection_refused", nil
	case strings.HasPrefix(message, "dial tcp: lookup ") && strings.HasSuffix(message, ": no such host"):
		return 0, "dns_lookup_failed", nil
	case message == "context deadline exceeded", message == "context deadline exceeded (Client.Timeout exceeded while awaiting headers)", message == "net/http: TLS handshake timeout":
		return 0, "timeout", nil
	case strings.HasPrefix(message, "tls: failed to verify certificate: x509: "):
		return 0, "tls_certificate_invalid", nil
	default:
		return 0, "", nil
	}
}
