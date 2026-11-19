// server.go — the forge's HTTP server: routing, auth, request ids and body
// reading. Ported from the TypeScript forge (app.ts), keeping the wire
// behaviour identical.
//
// The surfaces: git smart HTTP at /{owner}/{repo}.git, REST v3 under /api/v3,
// GraphQL v4 at /api/graphql and MCP at /mcp. Every reply is JSON with a
// non-empty x-github-request-id; an error carries a JSON body with a string
// message; anything else is a JSON 404.
//
// Concurrency: handlers run in parallel (the clone workload is 50 concurrent
// clones). Read-only handlers take the store's read lock; mutating handlers
// take the write lock. The deterministic tick advances only under the write
// lock, so its order follows request order.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"

var requestSeq uint64

// requestID returns the next deterministic request id: req-00000001, ...
func requestID() string {
	n := atomic.AddUint64(&requestSeq, 1)
	return fmt.Sprintf("req-%08d", n)
}

func expectedToken() string {
	return os.Getenv("MAKEFUN_SHOP_TOKEN")
}

// isAuthenticated mirrors the TS forge: a non-empty Bearer token; when
// MAKEFUN_SHOP_TOKEN is set it must match.
func isAuthenticated(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		return false
	}
	if expected := expectedToken(); expected != "" && token != expected {
		return false
	}
	return true
}

// sendJSON writes a JSON body with a trailing newline (as the TS sendJson does).
func sendJSON(w http.ResponseWriter, status int, body any, extraHeaders map[string]string) {
	raw := marshalJSON(body)
	w.Header().Set("Content-Type", jsonType)
	w.Header().Set("X-Github-Request-Id", requestID())
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(raw)
}

// sendJSONBytes writes pre-encoded JSON bytes (already newline-terminated).
func sendJSONBytes(w http.ResponseWriter, status int, raw []byte, extraHeaders map[string]string) {
	w.Header().Set("Content-Type", jsonType)
	w.Header().Set("X-Github-Request-Id", requestID())
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(raw)
}

// sendBuffer writes a raw body (git smart HTTP results).
func sendBuffer(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Github-Request-Id", requestID())
	w.WriteHeader(status)
	w.Write(body)
}

// sendEmpty writes a header-only response (202/204).
func sendEmpty(w http.ResponseWriter, status int) {
	w.Header().Set("X-Github-Request-Id", requestID())
	w.WriteHeader(status)
}

func sendErr(w http.ResponseWriter, status int, message string) {
	sendJSON(w, status, map[string]any{"message": message}, nil)
}

// readBody reads the whole request body (bounded at 64 MiB, as in the TS forge).
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 64*1024*1024 {
		return nil, fmt.Errorf("request body too large")
	}
	return raw, nil
}

func decodeSegment(segment string) string {
	if !strings.Contains(segment, "%") {
		return segment
	}
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}

// parsePath splits the request path into decoded segments (empty parts dropped),
// mirroring the TS forge's parseUrl + segment handling.
func parsePath(r *http.Request) ([]string, url.Values) {
	escaped := r.URL.EscapedPath()
	rawParts := strings.Split(escaped, "/")
	segments := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		if part == "" {
			continue
		}
		segments = append(segments, decodeSegment(part))
	}
	return segments, r.URL.Query()
}

// makeHandler builds the http.Handler for a store.
func makeHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			// A handler must never hang the connection: answer 500 JSON and keep serving.
			if rec := recover(); rec != nil {
				msg := "Internal Server Error"
				if err, ok := rec.(error); ok {
					msg = err.Error()
				} else if s, ok := rec.(string); ok {
					msg = s
				}
				func() {
					defer func() { _ = recover() }()
					sendErr(w, 500, msg)
				}()
			}
		}()

		segments, query := parsePath(r)
		method := r.Method
		path := r.URL.Path

		// The env's boot check calls this twice: 200 {"ok":true}. No auth.
		if method == http.MethodGet && path == "/healthz" {
			sendJSON(w, 200, map[string]any{"ok": true}, nil)
			return
		}

		// Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack.
		if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") {
			owner := segments[0]
			repoName := strings.TrimSuffix(segments[1], ".git")
			if !isAuthenticated(r) {
				sendErr(w, 401, "Requires authentication")
				return
			}
			if method == http.MethodGet && segments[2] == "info" && len(segments) > 3 && segments[3] == "refs" {
				store.RLock()
				result := InfoRefs(store, owner, repoName, query.Get("service"), r.Header.Get("Git-Protocol"))
				store.RUnlock()
				if result.Status == 200 {
					sendBuffer(w, 200, result.ContentType, result.Body)
				} else {
					sendJSONBytes(w, result.Status, result.Body, nil)
				}
				return
			}
			if method == http.MethodPost && segments[2] == "git-upload-pack" {
				body, err := readBody(r)
				if err != nil {
					sendErr(w, 400, "read error")
					return
				}
				store.RLock()
				result := UploadPack(store, owner, repoName, body, r.Header.Get("Git-Protocol"))
				store.RUnlock()
				if result.Status == 200 {
					sendBuffer(w, 200, result.ContentType, result.Body)
				} else {
					sendJSONBytes(w, result.Status, result.Body, nil)
				}
				return
			}
			if method == http.MethodPost && segments[2] == "git-receive-pack" {
				body, err := readBody(r)
				if err != nil {
					sendErr(w, 400, "read error")
					return
				}
				store.Lock()
				result := ReceivePack(store, owner, repoName, body)
				store.Unlock()
				if result.Status == 200 {
					sendBuffer(w, 200, result.ContentType, result.Body)
				} else {
					sendJSONBytes(w, result.Status, result.Body, nil)
				}
				return
			}
			sendErr(w, 404, "Not Found")
			return
		}

		// GraphQL v4.
		if method == http.MethodPost && path == "/api/graphql" {
			if !isAuthenticated(r) {
				sendErr(w, 401, "Requires authentication")
				return
			}
			raw, err := readBody(r)
			if err != nil {
				sendErr(w, 400, "read error")
				return
			}
			var body any
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &body); err != nil {
					sendErr(w, 400, "malformed JSON body")
					return
				}
			}
			store.Lock()
			status, result := HandleGraphQL(store, body)
			store.Unlock()
			sendJSON(w, status, result, nil)
			return
		}

		// MCP over Streamable HTTP.
		if method == http.MethodPost && path == "/mcp" {
			if !isAuthenticated(r) {
				sendErr(w, 401, "Requires authentication")
				return
			}
			raw, err := readBody(r)
			if err != nil {
				sendErr(w, 400, "read error")
				return
			}
			var body any
			if len(raw) == 0 {
				sendJSON(w, 400, map[string]any{
					"jsonrpc": "2.0", "id": nil,
					"error": map[string]any{"code": -32700, "message": "Parse error"},
				}, nil)
				return
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				sendJSON(w, 400, map[string]any{
					"jsonrpc": "2.0", "id": nil,
					"error": map[string]any{"code": -32700, "message": "Parse error"},
				}, nil)
				return
			}
			store.Lock()
			status, result := HandleMCP(store, body)
			store.Unlock()
			if status == 202 {
				sendEmpty(w, 202)
				return
			}
			sendJSON(w, status, result, nil)
			return
		}

		// REST v3.
		if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
			restSegments := []string{}
			if path != "/api/v3" {
				restSegments = segments[2:]
			}
			// The API root and /meta are public; everything else needs a token.
			needsAuth := !(len(restSegments) == 0 || (len(restSegments) == 1 && restSegments[0] == "meta"))
			if needsAuth && !isAuthenticated(r) {
				sendErr(w, 401, "Requires authentication")
				return
			}
			var body any
			if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete {
				raw, err := readBody(r)
				if err != nil {
					sendErr(w, 400, "read error")
					return
				}
				if len(raw) > 0 {
					if err := json.Unmarshal(raw, &body); err != nil {
						sendErr(w, 400, "malformed JSON body")
						return
					}
				}
			}
			host := r.Host
			if host == "" {
				host = "localhost"
			}
			if method == http.MethodGet {
				store.RLock()
				result := RouteRest(store, method, restSegments, query, body, host)
				store.RUnlock()
				if result.Status == 204 {
					sendEmpty(w, 204)
					return
				}
				sendJSON(w, result.Status, result.Body, result.Headers)
				return
			}
			store.Lock()
			result := RouteRest(store, method, restSegments, query, body, host)
			store.Unlock()
			if result.Status == 204 {
				sendEmpty(w, 204)
				return
			}
			sendJSON(w, result.Status, result.Body, result.Headers)
			return
		}

		// Everything else, every method: GitHub's 404 body.
		sendErr(w, 404, "Not Found")
	})
}
