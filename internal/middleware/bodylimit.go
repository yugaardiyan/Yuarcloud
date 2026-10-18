package middleware

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimitMiddleware caps the bytes a single request body may carry.
//
// gin's MaxMultipartMemory only decides where a part is *spilled to*, not how
// much may be sent. Setting it does not bound anything: c.FormFile parses the
// entire body, streams every oversized part into a temp file, and only then
// does the handler get to look at fileHeader.Size — by which point the bytes
// are already on disk. One request could write as much as the uploader cared
// to send, and the write-share upload endpoint is reachable with no
// authentication at all, so "anyone with a folder link" was also "anyone who
// can fill the server's disk".
//
// MaxBytesReader fails the read instead, so the transfer stops at the limit
// and nothing past it is ever written. The handler still has to notice: it
// gets an ordinary-looking error from the multipart parser. BodyTooLarge
// recognises it so the route can answer 413 rather than a generic 400.
func BodyLimitMiddleware(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit <= 0 {
			// Zero means "no ceiling", which is a supported configuration for
			// a self-hosted owner who knows what their disk looks like.
			c.Next()
			return
		}

		// Keep the unwrapped reader. Draining has to come from this one: the
		// MaxBytesReader that replaced Request.Body refuses to produce another
		// byte after the limit trips, so reading through it to "finish" the
		// request yields nothing and the connection still resets.
		underlying := c.Request.Body
		c.Request.Body = http.MaxBytesReader(c.Writer, underlying, limit)
		c.Next()

		// Let the client actually read the refusal.
		//
		// When a handler stops reading mid-body, the connection closes with
		// unread data still in the socket's receive buffer, and the kernel
		// sends RST rather than FIN — discarding the response with it. The
		// uploader sees a connection reset instead of the 413 that says why,
		// and the one honest message is replaced by the one failure they can
		// do nothing about.
		//
		// Bounded on purpose: a client announcing a gigabyte must not be able
		// to make the server read a gigabyte on the way to saying no.
		if c.Writer.Status() == http.StatusRequestEntityTooLarge && underlying != nil {
			_, _ = io.CopyN(io.Discard, underlying, drainAfterLimit)
		}
	}
}

// drainAfterLimit is how much of an over-limit body is read and discarded so
// the response survives. 512 KiB covers a client that overshot the ceiling by
// framing and a little content, without turning "no" into "read it anyway".
const drainAfterLimit = 512 << 10

// BodyTooLarge reports whether err came from hitting the body limit, so the
// caller can distinguish it from a malformed request.
func BodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
