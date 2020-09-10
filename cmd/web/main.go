package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dimfeld/httptreemux/v5"
	"github.com/newrelic/go-agent/v3/integrations/logcontext"
	"github.com/newrelic/go-agent/v3/newrelic"
)

var (
	httpClient = &http.Client{
		Transport:     newrelic.NewRoundTripper(http.DefaultTransport),
		CheckRedirect: http.DefaultClient.CheckRedirect,
		Jar:           http.DefaultClient.Jar,
		Timeout:       http.DefaultClient.Timeout,
	}
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	nrLicense := os.Getenv("NEWRELIC_LICENSE_KEY")
	if nrLicense == "" {
		return fmt.Errorf("NEWRELIC_LICENSE_KEY required")
	}
	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName("aereal-test"),
		newrelic.ConfigLicense(nrLicense),
		newrelic.ConfigLogger(newrelic.NewLogger(os.Stderr)),
		newrelic.ConfigDistributedTracerEnabled(true),
	)
	if err != nil {
		return fmt.Errorf("cannot build new relic app: %w", err)
	}

	cl := &contextLogger{apiKey: nrLicense}

	mux := httptreemux.New()
	mux.UseHandler(withNewRelic(app))
	mux.GET("/", func(w http.ResponseWriter, r *http.Request, params map[string]string) {
		fmt.Fprintln(w, "OK")
	})
	mux.GET("/fetch", func(w http.ResponseWriter, r *http.Request, params map[string]string) {
		ctx := r.Context()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://aereal.org/", nil)
		if err != nil {
			http.Error(w, fmt.Sprintf("cannot build request: %s", err), http.StatusInternalServerError)
			return
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to send request: %s", err), http.StatusInternalServerError)
			return
		}
		cl.log(ctx, fmt.Sprintf("done fetch: status=%d", resp.StatusCode))
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(struct{ Status int }{Status: resp.StatusCode})
	})

	log.Printf("start server")
	err = http.ListenAndServe(":8000", mux)
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func withNewRelic(app *newrelic.Application) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			txn := app.StartTransaction(fmt.Sprintf("%s %s", r.Method, r.URL.Path))
			defer txn.End()

			r = newrelic.RequestWithTransactionContext(r, txn)
			txn.SetWebRequestHTTP(r)
			txn.SetWebResponse(w)

			next.ServeHTTP(w, r)
		})
	}
}

type logBody map[string]interface{}

type detailedLog struct {
	Logs []logBody `json:"logs"`
}

type contextLogger struct {
	apiKey string
}

func (l *contextLogger) log(ctx context.Context, msg string) {
	txn := newrelic.FromContext(ctx)
	if txn == nil {
		return
	}
	data := logBody{}
	logcontext.AddLinkingMetadata(data, txn.GetLinkingMetadata())
	now := time.Now()
	data[logcontext.KeyTimestamp] = uint64(now.UnixNano()) / uint64(1000*1000)
	data[logcontext.KeyMessage] = msg
	if err := l.send(ctx, []logBody{data}); err != nil {
		log.Printf("! error=%s", err)
	}
}

func (l *contextLogger) send(ctx context.Context, datas []logBody) error {
	j, err := json.Marshal([]detailedLog{{Logs: datas}})
	if err != nil {
		return fmt.Errorf("json.Marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://log-api.newrelic.com/log/v1", strings.NewReader(string(j)))
	if err != nil {
		return fmt.Errorf("NewRequest: %w", err)
	}
	req.Header.Set("x-license-key", l.apiKey)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Do: %w", err)
	}
	log.Printf("post log status=%d", resp.StatusCode)
	return nil
}
