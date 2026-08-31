package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRPCSignature(t *testing.T) {
	var received url.Values
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.Query()
		_, _ = w.Write([]byte(`{"RequestId":"r-1"}`))
	}))
	defer httpServer.Close()
	client := &RPCClient{Endpoint: httpServer.URL, Version: "2014-05-26", AccessKey: "AKID", Secret: "SECRET", HTTPClient: httpServer.Client()}
	if _, err := client.Call(context.Background(), "DescribeRegions", map[string]string{"RegionId": "cn-hongkong"}); err != nil {
		t.Fatal(err)
	}
	if received.Get("Signature") == "" || received.Get("SignatureMethod") != "HMAC-SHA1" {
		t.Fatalf("missing signature fields: %v", received)
	}
	canonical := canonicalQuery(received)
	stringToSign := "GET&%2F&" + percentEncode(canonical)
	h := hmac.New(sha1.New, []byte("SECRET&"))
	_, _ = h.Write([]byte(stringToSign))
	expected := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if received.Get("Signature") != expected {
		t.Fatalf("signature mismatch: got %s want %s", received.Get("Signature"), expected)
	}
}

func TestRPCTreatsCode200AsSuccess(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"RequestId":"r-1","Code":"200","Datapoints":"[]"}`))
	}))
	defer httpServer.Close()
	client := &RPCClient{Endpoint: httpServer.URL, Version: "2019-01-01", AccessKey: "AKID", Secret: "SECRET", HTTPClient: httpServer.Client()}
	if _, err := client.Call(context.Background(), "DescribeMetricList", nil); err != nil {
		t.Fatalf("successful CMS response was treated as an error: %v", err)
	}
}

func TestRPCTreatsSuccessCodeAsSuccess(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"RequestId":"r-1","Code":"Success","Message":"Successful!"}`))
	}))
	defer httpServer.Close()
	client := &RPCClient{Endpoint: httpServer.URL, Version: "2017-12-14", Product: "BssOpenApi", AccessKey: "AKID", Secret: "SECRET", HTTPClient: httpServer.Client()}
	if _, err := client.Call(context.Background(), "QueryAccountBalance", nil); err != nil {
		t.Fatalf("successful BSS response was treated as an error: %v", err)
	}
}

func TestRPCDoesNotRetryMutatingActions(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"Code":"InternalError","Message":"temporary"}`))
	}))
	defer server.Close()
	client := &RPCClient{Endpoint: server.URL, Version: "2014-05-26", AccessKey: "AKID", Secret: "SECRET", HTTPClient: server.Client()}
	if _, err := client.Call(context.Background(), "RunInstances", nil); err == nil {
		t.Fatal("mutating RPC unexpectedly succeeded")
	}
	if calls != 1 {
		t.Fatalf("mutating RPC was retried %d times", calls)
	}
}
