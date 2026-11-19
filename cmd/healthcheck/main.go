// Command healthcheck probes an HTTP endpoint and exits 0 on a 2xx response, 1 otherwise.
// The runtime image is distroless (no shell, curl or wget), so docker-compose health checks
// run this instead: ["CMD", "/healthcheck", "http://localhost:8080/readyz"].
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <url>")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintln(os.Stderr, resp.Status)
		os.Exit(1)
	}
}
