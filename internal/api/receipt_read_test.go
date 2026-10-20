package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A body read cut short for transport reasons (the server's ReadTimeout, a
// stalled client) is not the request's content at fault: it must not be a 400,
// which the backend takes as final and stops the receipt on. It is a 503 the
// backend retries with the same key.
func TestSendReceiptRoute_BodyReadCutShortIs503NotA400(t *testing.T) {
	srv, err := receiptServer(t, receiptConfig(t), &slowReceiptWriter{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Config.ReadTimeout = 300 * time.Millisecond
	ts.Start()
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := validReceiptBody
	fmt.Fprintf(conn, "POST /api/sendReceipt HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		mustIssuedToken(t), len(body), body[:len(body)/2])
	time.Sleep(600 * time.Millisecond) // past the ReadTimeout, the rest never comes

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s; want 503", resp.StatusCode, raw)
	}
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Code != "RECEIPT_REQUEST_READ_FAILED" {
		t.Errorf("code = %q (%s); want RECEIPT_REQUEST_READ_FAILED", e.Code, strings.TrimSpace(string(raw)))
	}
}
