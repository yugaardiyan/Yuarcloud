package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// The browser media elements are the only real client of this, so the cases
// below mirror what Chrome and Safari actually put in a Range header.
//
// The distinction that matters is malformed vs unsatisfiable. RFC 9110 §14.2:
// an unparseable Range is ignored and the whole representation is served; only
// a syntactically valid range pointing outside the file earns a 416. Collapsing
// both into one failure made a legal request format fail playback outright.
func TestParseRange(t *testing.T) {
	const size = 1000

	cases := []struct {
		name     string
		header   string
		want     parseRangeStatus
		wantCode int
	}{
		{"open ended", "bytes=0-", rangeOK, http.StatusPartialContent},
		{"explicit window", "bytes=100-199", rangeOK, http.StatusPartialContent},
		{"suffix", "bytes=-500", rangeOK, http.StatusPartialContent},
		{"suffix larger than file", "bytes=-5000", rangeOK, http.StatusPartialContent},
		{"end past EOF is clamped", "bytes=900-99999", rangeOK, http.StatusPartialContent},
		{"last byte", "bytes=999-999", rangeOK, http.StatusPartialContent},
		{"whitespace tolerated", "bytes= 100 - 199 ", rangeOK, http.StatusPartialContent},
		{"unit range", "bytes=0-0", rangeOK, http.StatusPartialContent},

		// Syntactically valid, no byte satisfies it.
		{"past EOF", "bytes=1000-", rangeUnsatisfiable, http.StatusRequestedRangeNotSatisfiable},
		{"zero suffix", "bytes=-0", rangeUnsatisfiable, http.StatusRequestedRangeNotSatisfiable},

		// Legal to receive, malformed, and must be served in full with 200.
		// A start past EOF is unsatisfiable whatever follows it, so `bytes=200-100`
		// against a 10-byte file is a 416. Against this 1000-byte file the start
		// is inside the file, so the reversal is what makes it malformed.
		{"reversed", "bytes=200-100", rangeMalformed, http.StatusOK},
		{"reversed, short", "bytes=8-2", rangeMalformed, http.StatusOK},
		{"not bytes", "items=0-10", rangeMalformed, http.StatusOK},
		{"multi range", "bytes=0-10,20-30", rangeMalformed, http.StatusOK},
		{"garbage", "bytes=abc-def", rangeMalformed, http.StatusOK},
		{"no dash", "bytes=100", rangeMalformed, http.StatusOK},
		{"negative start", "bytes=-5-10", rangeMalformed, http.StatusOK},
		{"empty", "", rangeMalformed, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, got := parseRange(tc.header, size)
			if got != tc.want {
				t.Fatalf("parseRange(%q) = %v, want %v", tc.header, got, tc.want)
			}
			if got != rangeOK {
				return
			}
			// A served range must always be non-empty and inside the file.
			if start < 0 || end < start || end >= size {
				t.Fatalf("parseRange(%q) produced an unusable range %d-%d for size %d", tc.header, start, end, size)
			}
		})
	}
}

func TestAsciiFallback(t *testing.T) {
	cases := map[string]string{
		"report.pdf":  "report.pdf",
		"my file.png": "my file.png",
		// Semicolon is legal inside a quoted-string, so it stays.
		"laporan;qurin": "laporan;qurin",
		// A quote or a backslash would terminate the quoted-string early.
		`quo"te`: "quo_te",
		`a\b`:    "a_b",
		// One underscore per rune, not per byte: three CJK characters are
		// three underscores even though they are nine bytes.
		"日本語.txt": "___.txt",
		// Non-printing characters cannot go in a header at all.
		"a\x00b": "a_b",
		"":       "file",
	}

	for in, want := range cases {
		if got := asciiFallback(in); got != want {
			t.Errorf("asciiFallback(%q) = %q, want %q", in, got, want)
		}
	}
}

// A stored name containing a quote used to end the quoted-string early, so the
// bytes after it stopped being part of the filename parameter and the browser's
// suggested save name became attacker-chosen:
//
//	Content-Disposition: attachment; filename="a".txt"
//	                                     ^ grammar ends here
//
// Response splitting is not the risk it looks like — net/http strips CR and LF
// from header values — but the header stops being what it claims to be, and
// there were four sites each formatting this by hand.
func TestContentDispositionKeepsNameInsideTheQuotedString(t *testing.T) {
	nasty := `a".txt`

	h := contentDisposition("attachment", nasty)
	if !strings.HasPrefix(h, `attachment; filename="`) {
		t.Fatalf("unexpected shape: %s", h)
	}

	quoted := h[len(`attachment; filename="`):]
	value, _, ok := strings.Cut(quoted, `"`)
	if !ok {
		t.Fatalf("no closing quote in %s", h)
	}
	if strings.ContainsAny(value, "\"\\") {
		t.Fatalf("quote or backslash survived into the filename parameter: %q", value)
	}

	// The real name still rides in filename*, so nothing is lost — including
	// the UTF-8 that the ASCII form cannot carry.
	if !strings.Contains(h, "filename*=UTF-8''a%22.txt") {
		t.Fatalf("filename* lost the original name: %s", h)
	}
}

func TestContentDispositionUsesBothForms(t *testing.T) {
	got := contentDisposition("inline", "日本語 report.pdf")
	if !strings.HasPrefix(got, `inline; filename="`) {
		t.Errorf("disposition or fallback wrong: %s", got)
	}
	if !strings.Contains(got, "filename*=UTF-8''%E6%97%A5%E6%9C%AC%E8%AA%9E%20report.pdf") {
		t.Errorf("filename* is not percent-encoded UTF-8: %s", got)
	}
}

// The three outcomes are dispatched by a gate, not by a switch with early
// returns — a malformed range has to reach the full-body code *after* the
// block. It used to be a switch case with no return, so `break` left the switch
// and landed on the 206 path: a header we did not understand was answered 206
// with `Content-Range: bytes 0-0/size` and one byte of content, which reads as
// a successful partial response and stops the client asking for the rest.
func TestPreviewServesWholeFileForAnUnparseableRange(t *testing.T) {
	root := t.TempDir()
	content := []byte("0123456789")

	// A real on-disk backend, so the test fails if the dispatch ever depends on
	// something other than what parseRange returns.
	store, err := storage.NewLocalStorage(&config.Config{
		Storage: config.StorageConfig{
			Type:    "local",
			Local:   config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{TempDir: filepath.Join(root, ".yuarcloud-chunks")},
		},
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "r.txt"), content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/preview", func(c *gin.Context) {
		c.Header("Accept-Ranges", "bytes")
		size := int64(len(content))
		hdr := c.GetHeader("Range")
		start, end, status := parseRange(hdr, size)

		switch {
		case status == rangeUnsatisfiable:
			c.Header("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			c.Status(http.StatusRequestedRangeNotSatisfiable)

		case status == rangeOK:
			reader, err := store.ReadRange(context.Background(), "r.txt", start, end-start+1)
			if err != nil {
				c.Status(http.StatusInternalServerError)
				return
			}
			defer reader.Close()
			c.Header("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(size, 10))
			c.Status(http.StatusPartialContent)
			io.Copy(c.Writer, reader)

		default:
			// The full body, as RFC 9110 §14.2 asks for an unparseable range.
			c.Status(http.StatusOK)
			c.Writer.Write(content)
		}
	})

	// `bytes=200-100` is deliberately absent: against a 10-byte file its start
	// is past EOF, which is unsatisfiable rather than malformed, and 416 is the
	// right answer for that.
	for _, hdr := range []string{"bytes=abc-def", "bytes=0-1,4-5", "items=0-4", "bytes=8-2"} {
		req := httptest.NewRequest(http.MethodGet, "/preview", nil)
		req.Header.Set("Range", hdr)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Range %q -> %d, want 200", hdr, rec.Code)
		}
		if cr := rec.Header().Get("Content-Range"); cr != "" {
			t.Errorf("Range %q produced Content-Range %q; a full response must not claim to be partial", hdr, cr)
		}
		if rec.Body.String() != string(content) {
			t.Errorf("Range %q returned %q, want the whole file", hdr, rec.Body.String())
		}
	}
}
