// Command cyac-inject is a reverse proxy that adds one header to every
// request it forwards. It is meant to run as a cyservice: boxes on a
// network that attaches the service call it over plain HTTP, and it calls
// the upstream with a secret (an API key, say) that the boxes never see.
//
// The header's value comes from a file, normally the service's
// LoadCredential= secret at /run/secrets/<name>, read once at start.
// Restart the service to pick up a new one.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", ":8080", "address to serve on; match the service's Port=")
	upstream := flag.String("upstream", "", "URL to forward to; the request's path is appended to its path")
	header := flag.String("header", "Authorization", "header to set on every request")
	file := flag.String("value-file", "", "file holding the header's value, e.g. /run/secrets/<name>")
	prefix := flag.String("prefix", "", `put before the file's contents, e.g. "Bearer "`)
	flag.Parse()

	value, err := readValue(*file)
	if err != nil {
		log.Fatal(err)
	}
	h, err := handler(*upstream, *header, *prefix+value)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("forwarding %s to %s, setting %s", *listen, *upstream, http.CanonicalHeaderKey(*header))
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 30 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// readValue reads the header's value from path, without the trailing
// newline that files written by hand usually have.
func readValue(path string) (string, error) {
	if path == "" {
		return "", errors.New("-value-file is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s: want one non-empty line", path)
	}
	return v, nil
}

// handler forwards every request to upstream with header set to value,
// replacing any value the caller sent. It adds no X-Forwarded-* headers
// and drops the caller's, so the upstream learns nothing about the boxes.
func handler(upstream, header, value string) (http.Handler, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("-upstream %q: want an http or https URL", upstream)
	}
	if header == "" {
		return nil, errors.New("-header is required")
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u) // also sends the upstream's Host
			r.Out.Header.Set(header, value)
		},
		// Stream responses as they come, for server-sent events.
		FlushInterval: -1,
	}, nil
}
