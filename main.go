package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2/google"
)

const (
	envPrefix    = "GCSC_"
	gcsBase      = "https://storage.googleapis.com"
	storageScope = "https://www.googleapis.com/auth/devstorage.read_only"
)

// hopByHop headers must not be forwarded per RFC 7230.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// matchRule is a required-header guard: the named header must exist and match re.
type matchRule struct {
	header string
	re     *regexp.Regexp
}

type matchRules []matchRule

// parseMatchRules parses "[Header]:[Regex];[Header]:[Regex]" syntax.
// The first ':' in each ';'-delimited segment is the header/regex separator,
// so regex patterns may themselves contain ':'.
func parseMatchRules(s string) (matchRules, error) {
	var rules matchRules
	for part := range strings.SplitSeq(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, ":")
		if idx < 0 {
			return nil, fmt.Errorf("expected Header:Regex, got: %s", part)
		}
		header := strings.TrimSpace(part[:idx])
		pattern := part[idx+1:]
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex for %s: %v", header, err)
		}
		rules = append(rules, matchRule{header: header, re: re})
	}
	return rules, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(envPrefix + key); v != "" {
		return v
	}
	return def
}

func main() {
	portFlag := flag.String("port", envOr("PORT", "8080"), "listen port [$GCSC_PORT]")
	credFlag := flag.String("credential", envOr("CREDENTIAL", ""), "GCP service account JSON path [$GCSC_CREDENTIAL]")
	inspectFlag := flag.String("inspect", envOr("INSPECT", ""), "append req/res header dump (YAML) to this file [$GCSC_INSPECT]")
	reqmatchFlag := flag.String("reqmatch", envOr("REQMATCH", ""), `header guard rules "Header:Regex;Header:Regex" [$GCSC_REQMATCH]`)
	flag.Parse()

	rules, err := parseMatchRules(*reqmatchFlag)
	if err != nil {
		log.Fatalf("invalid -reqmatch: %v", err)
	}

	credPath := *credFlag

	p := &proxy{rules: rules}

	if *inspectFlag != "" {
		f, err := os.OpenFile(*inspectFlag, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("inspect: %v", err)
		}
		p.inspect = f
	}

	if credPath == "" {
		p.client.Store(&http.Client{})
	} else {
		go func() {
			waitForFile(credPath)
			log.Printf("credential ready: %s", credPath)
			client, err := buildClient(context.Background(), credPath)
			if err != nil {
				log.Fatalf("build http client: %v", err)
			}
			p.client.Store(client)
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/", p.handle)

	addr := ":" + *portFlag
	log.Printf("gcsconduit listening on %s (credential: %q, reqmatch rules: %d)", addr, credPath, len(rules))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func buildClient(ctx context.Context, credPath string) (*http.Client, error) {
	data, err := os.ReadFile(credPath)
	if err != nil {
		return nil, err
	}
	conf, err := google.JWTConfigFromJSON(data, storageScope)
	if err != nil {
		return nil, err
	}
	return conf.Client(ctx), nil
}

func waitForFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

func firstHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

type proxy struct {
	client    atomic.Pointer[http.Client]
	rules     matchRules
	inspect   *os.File
	inspectMu sync.Mutex
}

func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	client := p.client.Load()
	if client == nil {
		http.Error(w, "initializing", http.StatusServiceUnavailable)
		return
	}

	for _, rule := range p.rules {
		val := firstHeader(r.Header, rule.header, "X-"+rule.header)
		if val == "" || !rule.re.MatchString(val) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			log.Printf("403 reqmatch %s from %s", rule.header, r.RemoteAddr)
			if p.inspect != nil {
				p.writeInspect(r, http.StatusForbidden, nil)
			}
			return
		}
	}

	if r.URL.Path == "/" {
		http.Error(w, "path required: /bucket/path/to/file", http.StatusBadRequest)
		return
	}

	target := gcsBase + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	copyHeaders(r.Header, outReq.Header)
	outReq.Header.Set("Via", "1.1 gcsconduit")
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			outReq.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			outReq.Header.Set("X-Forwarded-For", ip)
		}
	}

	resp, err := client.Do(outReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		log.Printf("502 %s %s: %v", r.Method, target, err)
		return
	}
	defer resp.Body.Close()

	copyHeaders(resp.Header, w.Header())
	w.Header().Set("Via", "1.1 gcsconduit")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	log.Printf("%s %s → %d (%s)", r.Method, r.URL.Path, resp.StatusCode, r.RemoteAddr)

	if p.inspect != nil {
		p.writeInspect(r, resp.StatusCode, resp.Header)
	}
}

func (p *proxy) writeInspect(r *http.Request, status int, respHeader http.Header) {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "method: %s\n", r.Method)
	fmt.Fprintf(&b, "path: %s\n", r.URL.Path)
	if r.URL.RawQuery != "" {
		fmt.Fprintf(&b, "query: ?%s\n", r.URL.RawQuery)
	}
	b.WriteString("request:\n")
	for k, vv := range r.Header {
		fmt.Fprintf(&b, "  %s: %s\n", k, strings.Join(vv, ", "))
	}
	fmt.Fprintf(&b, "response:\n  status: %d\n", status)
	for k, vv := range respHeader {
		fmt.Fprintf(&b, "  %s: %s\n", k, strings.Join(vv, ", "))
	}

	p.inspectMu.Lock()
	defer p.inspectMu.Unlock()
	if _, err := p.inspect.WriteString(b.String()); err != nil {
		log.Printf("inspect write: %v", err)
	}
}

func copyHeaders(src, dst http.Header) {
	for k, vv := range src {
		if hopByHop[k] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok"}`)
}
