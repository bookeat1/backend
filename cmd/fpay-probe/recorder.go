package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// recorder is a payment.Doer (see internal/infrastructure/payment/httpclient.go)
// that sits between the adapter's retry/backoff logic and the real network,
// exactly the seam the adapter's own tests use with httptest. It changes
// nothing about the request or the response — it only reads and re-wraps the
// bodies so it can print the exact bytes that crossed the wire, since the
// adapter deliberately hides most provider-internal wording once it has
// translated a response into a domain type, and that internal wording is the
// whole point of this probe.
//
// It never sees the secret key: FreedomPay's signature (pg_sig) is a keyed
// digest computed by the adapter before the request reaches here, never the
// key itself, so nothing printed by this recorder can leak FREEDOMPAY_SECRET_KEY.
type recorder struct {
	client *http.Client
	out    io.Writer
	n      int
}

func newRecorder(out io.Writer) *recorder {
	return &recorder{client: &http.Client{}, out: out}
}

// Do implements payment.Doer.
func (r *recorder) Do(req *http.Request) (*http.Response, error) {
	r.n++
	attempt := r.n

	var reqBody []byte
	if req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
		req.ContentLength = int64(len(reqBody))
	}

	fmt.Fprintf(r.out, "\n>>> attempt #%d: %s %s\n", attempt, req.Method, req.URL.String())
	printParams(r.out, reqBody)

	resp, err := r.client.Do(req)
	if err != nil {
		fmt.Fprintf(r.out, "<<< attempt #%d: transport error: %v\n", attempt, err)
		return nil, err
	}

	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	fmt.Fprintf(r.out, "<<< attempt #%d: HTTP %d, raw XML body:\n%s\n", attempt, resp.StatusCode, string(respBody))
	return resp, nil
}

// printParams renders a form-encoded request body as sorted key=value lines,
// the same signature-relevant view the adapter computed pg_sig over
// (signature.go: alphabetical by field name). It never contains the secret
// key — only pg_sig, which is a one-way digest of it.
func printParams(out io.Writer, body []byte) {
	if len(body) == 0 {
		fmt.Fprintln(out, "(empty body)")
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		fmt.Fprintf(out, "(could not parse body as form data: %v)\nraw: %s\n", err, string(body))
		return
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		for _, v := range values[k] {
			fmt.Fprintf(&b, "  %s = %s\n", k, v)
		}
	}
	fmt.Fprint(out, b.String())
}
