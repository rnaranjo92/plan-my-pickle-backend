package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func newTwilio(rt rtFunc, from string) *TwilioSms {
	return &TwilioSms{
		accountSID: "AC123",
		authToken:  "tok",
		from:       from,
		http:       newClient(rt),
	}
}

func TestTwilioSendSuccess(t *testing.T) {
	var gotURL, gotBody, gotAuth string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return resp(201, `{"sid":"SM999","status":"queued"}`), nil
	})
	tw := newTwilio(rt, "+15125550000")
	res, err := tw.Send("5125551234", "court 3")
	if err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if !res.OK || res.ProviderRef != "SM999" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(gotURL, "/Accounts/AC123/Messages.json") {
		t.Errorf("wrong endpoint: %s", gotURL)
	}
	if gotAuth == "" {
		t.Error("expected basic auth header")
	}
	// E.164 normalization and From routing.
	if !strings.Contains(gotBody, "To=%2B15125551234") {
		t.Errorf("To not normalized in body: %s", gotBody)
	}
	if !strings.Contains(gotBody, "From=%2B15125550000") {
		t.Errorf("expected From for non-MG sender: %s", gotBody)
	}
}

func TestTwilioSendMessagingService(t *testing.T) {
	var gotBody string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return resp(201, `{"sid":"SM1"}`), nil
	})
	tw := newTwilio(rt, "MG0123")
	if _, err := tw.Send("+15125551234", "hi"); err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if !strings.Contains(gotBody, "MessagingServiceSid=MG0123") {
		t.Errorf("expected MessagingServiceSid for MG sender: %s", gotBody)
	}
	if strings.Contains(gotBody, "From=") {
		t.Errorf("should not send From for MG sender: %s", gotBody)
	}
}

func TestTwilioSendUnparseableNumber(t *testing.T) {
	called := false
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return resp(201, `{"sid":"x"}`), nil
	})
	tw := newTwilio(rt, "+1")
	res, err := tw.Send("", "hi")
	if err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if res.OK {
		t.Error("expected OK=false for empty number")
	}
	if called {
		t.Error("should not hit HTTP for an unparseable number")
	}
}

func TestTwilioSendHTTPError(t *testing.T) {
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	tw := newTwilio(rt, "+15125550000")
	res, err := tw.Send("+15125551234", "hi")
	if err != nil {
		t.Fatalf("Send should swallow errors, got %v", err)
	}
	if res.OK {
		t.Error("expected OK=false on transport error")
	}
}

func TestTwilioSendRejected(t *testing.T) {
	// Non-2xx with an error code body -> OK=false.
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(400, `{"code":21211,"message":"invalid 'To'"}`), nil
	})
	tw := newTwilio(rt, "+15125550000")
	res, _ := tw.Send("+15125551234", "hi")
	if res.OK {
		t.Errorf("expected rejection: %+v", res)
	}
}

func TestTwilioSend2xxButNoSID(t *testing.T) {
	// 2xx but missing sid is still treated as failure.
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"status":"queued"}`), nil
	})
	tw := newTwilio(rt, "+15125550000")
	res, _ := tw.Send("+15125551234", "hi")
	if res.OK {
		t.Errorf("expected OK=false with no sid: %+v", res)
	}
}
