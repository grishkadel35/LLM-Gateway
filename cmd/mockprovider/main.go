// Command mockprovider serves fake OpenAI, Anthropic and Gemini APIs on one
// port, so the gateway can be exercised end to end without real API keys or
// spend. The handlers live in internal/mockprovider, where tests can also run
// them in-process.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/grishkadel/llm-gateway/internal/middleware"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "address to listen on")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           middleware.Logging(logger)(mockprovider.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("mock provider listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("mock provider stopped", "error", err)
		os.Exit(1)
	}
}
