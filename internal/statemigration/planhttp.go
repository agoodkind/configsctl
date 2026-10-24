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

func providerHTTPDiagnostic(detail string) (int, string) {
	if prefix := providerHTTPPrefix.FindStringSubmatchIndex(detail); prefix != nil {
		status, err := strconv.Atoi(detail[prefix[2]:prefix[3]])
		if err != nil {
			return 0, ""
		}
		text := http.StatusText(status)
		if text != "" && strings.HasPrefix(detail[prefix[1]:], text+" ") {
			return status, ""
		}
		return 0, ""
	}
	transport := providerTransportPrefix.FindStringSubmatch(detail)
	if transport == nil {
		return 0, ""
	}
	message := transport[1]
	switch {
	case strings.HasPrefix(message, "dial tcp ") && strings.HasSuffix(message, ": connect: connection refused"):
		return 0, "connection_refused"
	case strings.HasPrefix(message, "dial tcp: lookup ") && strings.HasSuffix(message, ": no such host"):
		return 0, "dns_lookup_failed"
	case message == "context deadline exceeded", message == "context deadline exceeded (Client.Timeout exceeded while awaiting headers)", message == "net/http: TLS handshake timeout":
		return 0, "timeout"
	case strings.HasPrefix(message, "tls: failed to verify certificate: x509: "):
		return 0, "tls_certificate_invalid"
	default:
		return 0, ""
	}
}
