package httpapi

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
)

func TestEndpointLifecycleValidationAndAttribution(t *testing.T) {
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	const path = "/v1/endpoints/00000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		method, suffix, body string
		status               int
	}{
		{"PUT", "", `{"url":"https://example.test/hooks","retry_profile":"outage","expected_version":1,"reason":"maintenance"}`, 200},
		{"PUT", "", `{"url":"https://example.test/hooks","retry_profile":"unknown","expected_version":1,"reason":"maintenance"}`, 400},
		{"PUT", "", `{"url":"http://169.254.169.254/","expected_version":1,"reason":"maintenance"}`, 400},
		{"PUT", "", `{"url":"https://example.test/hooks","retry_base_seconds":61,"retry_cap_seconds":60,"expected_version":1,"reason":"maintenance"}`, 400},
		{"PUT", "", `{"url":"https://example.test/hooks","event_ttl_seconds":604801,"expected_version":1,"reason":"maintenance"}`, 400},
		{"POST", "/pause", `{"expected_version":1,"reason":"maintenance"}`, 200},
		{"POST", "/resume", `{"expected_version":0,"reason":"maintenance"}`, 400},
		{"POST", "/pause", `{"expected_version":1,"reason":" "}`, 400},
		{"POST", "/pause", `{"expected_version":1,"reason":"maintenance","actor":"spoofed"}`, 400},
		{"POST", "/secret-rotations", `{"expected_version":1,"reason":"maintenance","secret":"synthetic-rotation-secret","overlap_seconds":300}`, 200},
		{"POST", "/secret-rotations", `{"expected_version":1,"reason":"maintenance","secret":"short","overlap_seconds":300}`, 400},
		{"POST", "/secret-rotations", `{"expected_version":1,"reason":"maintenance","secret":"synthetic-rotation-secret","overlap_seconds":299}`, 400},
		{"GET", "/audit?after=-1", "", 400},
		{"GET", "/audit?after=9223372036854775808", "", 400},
	} {
		s := &fakeStore{}
		api := New(s, box, apiClock{now: time.Unix(100, 0)}, discardLogger(), nil, testSecurity(t))
		r := httptest.NewRequest(tc.method, path+tc.suffix, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.suffix, w.Code, w.Body.String())
		}
		if tc.status == 400 && s.endpointChange.Action != "" {
			t.Fatal("invalid update reached persistence")
		}
		if tc.status == 200 && s.endpointChange.PrincipalID != "00000000-0000-4000-8000-000000000001" {
			t.Fatal("missing authenticated actor")
		}
		if tc.status == 200 && tc.method == "PUT" {
			e := s.endpointChange.Settings
			if e.MaxAttempts != 20 || e.RetryBaseSeconds != 300 || e.RetryCapSeconds != 7200 || e.EventTTLSeconds != 86400 {
				t.Fatal("incorrect outage profile")
			}
		}
		if tc.status == 200 && tc.suffix == "/secret-rotations" {
			key, err := box.Decrypt(s.endpointChange.Ciphertext)
			if err != nil || string(key) != "synthetic-rotation-secret" || bytes.Contains(s.endpointChange.Ciphertext, key) || bytes.Contains(w.Body.Bytes(), key) {
				t.Fatal("rotation did not protect secret")
			}
		}
	}
}
