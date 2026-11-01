package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestTransferSurvivesServerTimeouts is the regression guard for the shipped
// 30s request deadlines.
//
// Before the handlers called liftTransferDeadlines, a request that outlived
// ReadTimeout failed before its body was read and one that outlived
// WriteTimeout lost its response — which is the shape of a large upload dying
// part-way through, or an upload that finished but was reported as a network
// error. The handler below sleeps past both deadlines on purpose.
func TestTransferSurvivesServerTimeouts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const serverDeadline = 100 * time.Millisecond
	const handlerWork = 250 * time.Millisecond

	router := gin.New()
	router.POST("/upload", func(c *gin.Context) {
		liftTransferDeadlines(c)

		// Stands in for the body of a slow transfer (outlives ReadTimeout) and
		// for the response written afterwards (outlives WriteTimeout).
		time.Sleep(handlerWork)

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("reading body after the server deadline: %v", err)
		}
		c.String(http.StatusOK, "read %d", len(body))
	})

	srv := httptest.NewUnstartedServer(router)
	srv.Config.ReadTimeout = serverDeadline
	srv.Config.WriteTimeout = serverDeadline
	srv.Start()
	defer srv.Close()

	payload := bytes.Repeat([]byte("x"), 64)
	resp, err := http.Post(srv.URL+"/upload", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if want := "read 64"; string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}
