// main.go — the forge's entrypoint.
//
// When MAKEFUN_ENV_URL is set (the competition harness), the binary owns the
// whole lifecycle exactly like the TypeScript main.ts: listen on an ephemeral
// port, register it with the env (retrying with backoff), wait for /done, then
// shut down and exit 0.
//
// Without MAKEFUN_ENV_URL (local runs), it listens on FORGE_PORT (or an
// ephemeral port when unset), prints PORT=<n> to stdout, and serves until
// killed.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func sleep(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// register posts the port to the env's /register endpoint, retrying with
// backoff. 200/409 mean registered; 400/405 are fatal.
func register(client *http.Client, base string, port int) {
	delay := 500
	for {
		body, _ := json.Marshal(map[string]int{"port": port})
		resp, err := client.Post(base+"/register", "application/json", bytes.NewReader(body))
		if err != nil {
			sleep(delay)
			delay = minInt(delay*2, 8000)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 409 {
			return
		}
		if resp.StatusCode == 400 || resp.StatusCode == 405 {
			fmt.Fprintf(os.Stderr, "register failed: HTTP %d\n", resp.StatusCode)
			os.Exit(1)
		}
		sleep(delay)
		delay = minInt(delay*2, 8000)
	}
}

// waitDone polls the env's /done endpoint until it reports done.
func waitDone(client *http.Client, base string) {
	delay := 500
	for {
		resp, err := client.Get(base + "/done")
		if err != nil {
			sleep(delay)
			delay = minInt(delay*2, 8000)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var parsed struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(body, &parsed) == nil && parsed.Done {
				return
			}
			delay = 500
			sleep(1000)
			continue
		}
		sleep(delay)
		delay = minInt(delay*2, 8000)
	}
}

func main() {
	store := NewStore()
	handler := makeHandler(store)

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		// Standalone mode (local runs): FORGE_PORT or an ephemeral port.
		port := 0
		if v := os.Getenv("FORGE_PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				port = n
			}
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		actualPort := ln.Addr().(*net.TCPAddr).Port
		fmt.Printf("PORT=%d\n", actualPort)
		server := &http.Server{Handler: handler}
		go func() {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
			<-ch
			server.Close()
		}()
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	base := strings.TrimRight(envURL, "/")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("PORT=%d\n", port)
	server := &http.Server{Handler: handler}
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	register(client, base, port)
	waitDone(client, base)
	server.Close()
	os.Exit(0)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
