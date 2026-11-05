package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/labstack/echo/v4"
)

// ローカルの偽サーバーへ取りに行くため SSRF ガードを外したクライアントに差し替える
func useUnguardedClient(t *testing.T) {
	orig := client
	client = &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(func() { client = orig })
}

func serveImageHandler(t *testing.T, remoteURL string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/image/64x64/"+remoteURL, nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	e.GET("/image/*", ImageHandler)
	e.ServeHTTP(rec, req)
	return rec
}

func originalCacheExists(remoteURL string) bool {
	sum := sha256.Sum256([]byte(remoteURL))
	_, err := os.Stat(filepath.Join(CachePath, hex.EncodeToString(sum[:])+".data"))
	return err == nil
}

func TestImageSizeLimit(t *testing.T) {
	useUnguardedClient(t)

	var small bytes.Buffer
	png.Encode(&small, image.NewRGBA(image.Rect(0, 0, 8, 8)))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		switch r.URL.Path {
		case "/small.png":
			w.Write(small.Bytes())
		case "/declared.png":
			w.Header().Set("Content-Length", fmt.Sprint(MaxImageBytes+1))
		case "/chunked.png":
			chunk := make([]byte, 1024*1024)
			for i := 0; i < MaxImageBytes/len(chunk)+2; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer srv.Close()

	nonce := fmt.Sprint(time.Now().UnixNano())
	cases := []struct {
		path string
		code int
	}{
		{"/small.png", 200},
		{"/declared.png", 413},
		{"/chunked.png", 413},
	}
	for _, tc := range cases {
		remoteURL := srv.URL + tc.path + "?n=" + nonce
		rec := serveImageHandler(t, remoteURL)
		if rec.Code != tc.code {
			t.Errorf("%s: status = %d, want %d (%s)", tc.path, rec.Code, tc.code, rec.Body.String())
		}
		if cached := originalCacheExists(remoteURL); cached != (tc.code == 200) {
			t.Errorf("%s: original cache exists = %v", tc.path, cached)
		}
	}
}

func TestSummaryReadsOnlyHead(t *testing.T) {
	useUnguardedClient(t)
	mc = memcache.New("127.0.0.1:1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>hello</title></head><body>`)
		filler := []byte(strings.Repeat("<p>a</p>", 1024))
		for {
			if _, err := w.Write(filler); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/summary?url="+url.QueryEscape(srv.URL+"/"), nil)
	rec := httptest.NewRecorder()
	if err := SummaryHandler(echo.New().NewContext(req, rec)); err != nil {
		t.Fatalf("SummaryHandler: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v, body was not limited", elapsed)
	}

	var summary Summary
	json.Unmarshal(rec.Body.Bytes(), &summary)
	if summary.Title != "hello" {
		t.Errorf("title = %q, want hello", summary.Title)
	}
}
