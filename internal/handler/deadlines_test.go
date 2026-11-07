package handler

import (
	"bytes"
	"io"
	"net"
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

// TestStalledTransferBodyIsCutOff is the regression guard for the other half of
// the transfer deadline story: a body that stops arriving must fail rather than
// hang.
//
// liftTransferDeadlines used to clear the read deadline outright, so a request
// whose client went silent mid-body — a dropped link, a half-open socket — sat
// in the handler forever. That alone is one stuck connection, but a tus PATCH
// holds the per-upload lock across the read, so the stuck request also blocked
// every later HEAD, status poll and retry for the same upload: the upload froze
// at whatever percentage it had reached. The body now carries an idle deadline,
// so silence becomes an error the retry path can resume from.
func TestStalledTransferBodyIsCutOff(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const idle = 300 * time.Millisecond
	returned := make(chan time.Duration, 1)

	router := gin.New()
	router.POST("/upload", func(c *gin.Context) {
		applyTransferDeadlines(c, idle)

		start := time.Now()
		// Read until the body ends or the idle deadline fires. A half-open
		// upload never ends, so only the deadline can release this.
		_, _ = io.Copy(io.Discard, c.Request.Body)
		returned <- time.Since(start)

		c.Status(http.StatusNoContent)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Declare far more body than is sent, then stop: the half-open upload.
	req := "POST /upload HTTP/1.1\r\nHost: test\r\nContent-Length: 1048576\r\n\r\nhello"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	select {
	case took := <-returned:
		if took < idle {
			t.Fatalf("handler returned after %v, before the %v idle deadline", took, idle)
		}
		if took > 5*time.Second {
			t.Fatalf("handler took %v to abandon a stalled body", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body pinned the handler: the read idle deadline is not enforced")
	}
}
