package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLabelPNGBuildsAuthenticatedQuery(t *testing.T) {
	scale := 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/devices/labels/42/png" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		q := r.URL.Query()
		for key, want := range map[string]string{
			"name": "0", "price": "1", "barcode": "0",
			"w": "58", "h": "40", "scale": "3",
		} {
			if q.Get(key) != want {
				t.Errorf("%s = %q, want %q", key, q.Get(key), want)
			}
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, Token: "secret"})
	got, err := client.FetchLabelPNG(context.Background(), 42, LabelPNGOptions{
		Name: false, Price: true, Barcode: false,
		WidthMM: 58, HeightMM: 40, Scale: &scale,
	})
	if err != nil {
		t.Fatalf("FetchLabelPNG() error = %v", err)
	}
	if string(got) != "png" {
		t.Fatalf("body = %q", got)
	}
}

func TestFetchLabelPNGOmitsAbsentScaleAndClassifies404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["scale"]; ok {
			t.Errorf("scale unexpectedly present in %q", r.URL.RawQuery)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL})
	_, err := client.FetchLabelPNG(context.Background(), 99, LabelPNGOptions{
		Name: true, Price: true, Barcode: true, WidthMM: 30, HeightMM: 20,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FetchLabelPNG() error = %v, want ErrNotFound", err)
	}
}
