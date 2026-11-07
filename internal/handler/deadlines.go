package handler

import (
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// transferReadIdleTimeout bounds how long a transfer's request body may go
// without delivering a byte before the read is abandoned.
//
// http.Server.ReadTimeout bounds the entire request, body included, and the
// shipped 30s value aborts an upload slower than about 1 MB/s part-way through.
// The handlers that move a file opt out of it (liftTransferDeadlines). Clearing
// it outright, though, left the body read unbounded, and that is worse than one
// slow request: a connection that simply goes silent — a dropped link, a
// half-open socket a tunnel or proxy never closes — blocked the handler forever.
// For a tus PATCH the handler holds the per-upload lock across the read, so
// every later HEAD, status poll, retry or cancel for that upload queues behind
// the stalled one and the upload freezes with no way forward. An idle deadline
// keeps a genuinely slow transfer alive — the clock restarts on every read —
// while turning silence into an ordinary failure the retry path already knows
// how to resume from. It sits below the client's own 90s no-progress watchdog
// on purpose, so the server abandons a dead body and answers (a retryable 5xx)
// before the client has to give up on its own.
const transferReadIdleTimeout = 60 * time.Second

// liftTransferDeadlines clears the server-wide read and write deadlines for the
// rest of this request, replacing the whole-request read deadline with an idle
// one so a stalled body fails instead of hanging.
//
// http.Server.ReadTimeout bounds the entire request, body included, and
// WriteTimeout starts counting as soon as the request headers are in. Both are
// sized for an API call and both are wrong for a file. The shipped 30s value
// aborts an upload slower than about 1 MB/s part-way through, and it kills the
// response of any transfer — including the "created" answer to an upload that
// already finished and whose bytes are already on disk.
//
// The configured values stay in place for ordinary routes, where they are the
// right guard. Only the handlers that move a file opt out, and only for their
// own request: net/http installs fresh deadlines when the connection is reused,
// so the next call on a keep-alive connection is bounded again.
//
// Errors are deliberately ignored. A ResponseWriter that cannot reach the
// underlying connection (HTTP/2 on older Go releases, a middleware that does
// not expose Unwrap) leaves the server deadline in force for the response; the
// idle body deadline is the part that has to hold, and it is applied through
// the body itself.
func liftTransferDeadlines(c *gin.Context) {
	applyTransferDeadlines(c, transferReadIdleTimeout)
}

// applyTransferDeadlines installs the deadlines with an explicit body idle
// timeout, so a test can drive the stall path without waiting minutes.
func applyTransferDeadlines(c *gin.Context, readIdle time.Duration) {
	rc := http.NewResponseController(c.Writer)

	// A transfer's response can be large and slow — a download, or the
	// finalise answer to a long body — so the write deadline is lifted.
	_ = rc.SetWriteDeadline(time.Time{})

	if readIdle <= 0 {
		_ = rc.SetReadDeadline(time.Time{})
		return
	}

	_ = rc.SetReadDeadline(time.Now().Add(readIdle))
	if c.Request.Body != nil {
		c.Request.Body = &idleBody{rc: rc, body: c.Request.Body, idle: readIdle}
	}
}

// idleBody refreshes the connection's read deadline before each read, turning
// the absolute per-request deadline into an idle one: it fires only when the
// body stops arriving, never because it is merely arriving slowly.
type idleBody struct {
	rc   *http.ResponseController
	body io.ReadCloser
	idle time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	return b.body.Read(p)
}

func (b *idleBody) Close() error {
	return b.body.Close()
}
