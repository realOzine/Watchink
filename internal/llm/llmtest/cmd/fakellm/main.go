// Command fakellm serves the scriptable fake OpenAI-compatible endpoint of
// package llmtest standalone, for manual and end-to-end runs without a real
// API key:
//
//	go run ./internal/llm/llmtest/cmd/fakellm -addr 127.0.0.1:18080 [-script file.json]
//
// Point llm.base_url at http://127.0.0.1:18080/v1 (any path prefix works).
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"time"

	"watchink/internal/llm/llmtest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address (loopback)")
	script := flag.String("script", "", "optional script JSON file (see llmtest.Script)")
	flag.Parse()

	srv := llmtest.New()
	if *script != "" {
		if err := srv.LoadScript(*script); err != nil {
			log.Fatalf("fakellm: %v", err)
		}
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("fakellm: %v", err)
	}
	log.Printf("fakellm: serving on http://%s/v1 (%d scripted calls queued)", ln.Addr(), srv.Pending())
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.Serve(ln))
}
