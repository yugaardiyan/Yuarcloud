package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// liftTransferDeadlines clears the server-wide read and write deadlines for the
// rest of this request.
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
// not expose Unwrap) leaves the server deadline in force; that is the old
// behaviour, not a reason to fail the transfer before it starts.
func liftTransferDeadlines(c *gin.Context) {
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}
