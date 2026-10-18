package handler

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/share"
)

/**
 * Link previews for share links.
 *
 * WhatsApp, Telegram, Instagram, Slack and the rest of them fetch the pasted URL
 * and read `<meta property="og:*">` out of the HTML. They do not run JavaScript.
 * This app is a single-page app, so every tag React renders lives in a document
 * the crawler has already finished reading and thrown away — which is why
 * pasting a share link produced a bare URL no matter what the page showed.
 *
 * The tags are injected here instead, into the same shell the browser gets. The
 * crawler gets an HTML document with real tags in it; the browser gets a normal
 * app that replaces the title once the share resolves. Neither can tell the
 * difference, and there is only one source of truth for the markup.
 */

// SiteName appears in every preview. It is the one string a recipient sees before
// they have signed in, so it says what the thing is.
const SiteName = "Yuarcloud"

// SharePreviewHandler renders the app shell with link-preview tags for a share.
type SharePreviewHandler struct {
	share    *share.ShareService
	baseURL  string
	indexLoc string
}

// NewSharePreviewHandler wires the handler. `indexLoc` is the SPA shell on disk;
// it is read per request rather than cached, matching the NoRoute handler beside
// it. A cached copy would keep describing a bundle that no longer exists after a
// rebuild, and a shell pinned to dead asset hashes is worse than none.
func NewSharePreviewHandler(svc *share.ShareService, baseURL, indexLoc string) *SharePreviewHandler {
	return &SharePreviewHandler{share: svc, baseURL: baseURL, indexLoc: indexLoc}
}

// Share renders the shell for /s/:token and /yc/:token with preview tags.
//
// A missing, expired or disabled share still renders, with generic tags. The page
// itself decides whether the file can be opened; all this route promises is that
// the link says something sensible. Answering 404 here would make a revoked link
// look like a broken website to anyone who had already pasted it.
func (h *SharePreviewHandler) Share(c *gin.Context) {
	shell, err := os.ReadFile(h.indexLoc)
	if err != nil {
		c.String(http.StatusServiceUnavailable, "frontend not built")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", h.render(c, c.Param("token"), shell))
}

func (h *SharePreviewHandler) render(c *gin.Context, token string, shell []byte) []byte {
	title := SiteName
	description := "Self-hosted cloud storage."
	image := h.absoluteURL(c, "/og.png")

	rec, err := h.share.GetShareByToken(c.Request.Context(), token)
	switch {
	case err != nil || !shareStillOpen(rec, time.Now()):
		// Unknown, revoked or expired: say nothing about the file. The name is
		// the thing the share was withholding.
	case rec.PasswordHash != "":
		// A password-protected share does not get its name in the preview
		// either. The link is the capability; the filename is what the
		// password protects, and a preview card is rendered before anybody is
		// asked for anything.
		description = "A password-protected file was shared with you on " + SiteName + "."
	case rec.Permission == "write":
		title = "Upload to a shared folder"
		description = "A folder was shared with you. Add your files to it."
	default:
		title, description = describeShare(rec)
	}

	return injectPreviewTags(shell, title, description, image, h.absoluteURL(c, c.Request.URL.Path))
}

// shareStillOpen mirrors the access rules the share page itself enforces, so the
// preview agrees with what opening the link would actually do.
func shareStillOpen(s *model.ShareLink, now time.Time) bool {
	if s == nil || !s.IsActive {
		return false
	}
	if s.ExpiresAt != nil && now.After(*s.ExpiresAt) {
		return false
	}
	if s.MaxViews > 0 && s.ViewCount >= s.MaxViews {
		return false
	}
	return true
}

// describeShare turns a share into the two lines a preview card shows.
//
// Only the base name goes in the title. The full path would put the owner's whole
// folder tree into every chat the link is forwarded to, which is more than
// "here is a file" should cost.
func describeShare(s *model.ShareLink) (title, description string) {
	name := s.File.Name
	if name == "" {
		return SiteName, "A file was shared with you on " + SiteName + "."
	}
	kind := "File"
	if s.File.IsDir {
		kind = "Folder"
	}
	return name, fmt.Sprintf("%s · %s", kind, humanSize(s.File.Size))
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// injectPreviewTags inserts the preview block just before </head> and replaces
// the title. String surgery on the shell is deliberate: the alternative is a
// second templated copy of index.html kept in step with Vite's output by hand,
// and a stale copy of a shell is worse than no copy at all.
func injectPreviewTags(shell []byte, title, description, image, pageURL string) []byte {
	doc := stripPreviewTags(string(shell))

	// The shell already carries a <title> for the app. On a share route the
	// preview's title has to win, so the original goes rather than being left
	// to be read first.
	doc = replaceTitle(doc, title)

	block := "\n    <!-- Link preview. Injected server-side, and only on share routes: " +
		"chat clients read this out of the HTML and never run the app, so tags " +
		"rendered by React would never reach them. -->\n" +
		previewTag("og:type", "website") +
		previewTag("og:site_name", SiteName) +
		previewTag("og:title", title) +
		previewTag("og:description", description) +
		previewTag("og:url", pageURL) +
		previewTag("og:image", image) +
		previewTag("og:image:width", "1200") +
		previewTag("og:image:height", "630") +
		previewTag("og:image:alt", title) +
		previewTag("twitter:card", "summary_large_image") +
		previewTag("twitter:title", title) +
		previewTag("twitter:description", description) +
		previewTag("twitter:image", image) +
		"    <link rel=\"icon\" href=\"/favicon.ico\" sizes=\"any\" />\n"

	const headClose = "</head>"
	if idx := strings.Index(doc, headClose); idx >= 0 {
		return []byte(doc[:idx] + block + doc[idx:])
	}
	return []byte(doc)
}

/*
 * stripPreviewTags removes the preview tags already in the shell before this
 * route adds its own.
 *
 * This is not tidiness. index.html carries a static set so that the app's own
 * URL previews as the app, and appending the share set behind it produced a
 * document with two of every tag — and every one of these crawlers reads the
 * *first* match. The file name lost to the generic "Yuarcloud" that shipped in
 * the shell, which is exactly the value the preview exists to show. Order is
 * load-bearing in Open Graph, so the shell's copy has to go rather than merely
 * be overridden.
 */
func stripPreviewTags(doc string) string {
	patterns := []*regexp.Regexp{
		// <meta property="og:..." ...> and <meta name="twitter:..." ...>,
		// either attribute order, self-closing or not.
		regexp.MustCompile(`(?is)<meta[^>]*\bproperty\s*=\s*["']og:[^>]*>`),
		regexp.MustCompile(`(?is)<meta[^>]*\bname\s*=\s*["']twitter:[^>]*>`),
		// The .ico link is what this route adds, so drop the shell's copy and
		// let the one below stand. apple-touch-icon is left alone: the shell's
		// is correct for every route and this one adds nothing to it.
		regexp.MustCompile(`(?is)<link[^>]*\brel\s*=\s*["']icon["'][^>]*\bfavicon\.ico[^>]*>`),
	}
	for _, re := range patterns {
		doc = re.ReplaceAllString(doc, "")
	}
	return doc
}

/*
 * previewTag escapes both halves for an HTML attribute, not merely for a Go
 * string. A file can be called `<script>` or `x" onload="y`, and this text is
 * read by parsers that never execute it but still tokenise it: an unescaped
 * quote would end the attribute early and drop the rest of the name into the
 * document as markup. `%q` alone is not enough — it escapes the quote and the
 * backslash but leaves `<`, `>` and `&` alone.
 */
func previewTag(property, content string) string {
	return fmt.Sprintf("    <meta property=\"%s\" content=\"%s\" />\n",
		html.EscapeString(property), html.EscapeString(content))
}

func replaceTitle(doc, title string) string {
	const open, close = "<title>", "</title>"
	start := strings.Index(doc, open)
	if start < 0 {
		return doc
	}
	end := strings.Index(doc[start:], close)
	if end < 0 {
		return doc
	}
	return doc[:start] + open + html.EscapeString(title) + close + doc[start+end+len(close):]
}

/*
 * absoluteURL builds a link a crawler outside this network can actually fetch.
 *
 * The Host header is the honest answer when the request came through a reverse
 * proxy, which is the only deployment where previews can work at all — the
 * crawler had to reach the server in order to ask. base_url covers the case where
 * a proxy rewrites Host to something internal. Localhost and private addresses are
 * deliberately left as they are rather than "fixed": there is nothing on the far
 * side to fix them to, and inventing a hostname yields a card whose image fails
 * to load, which looks worse than a card with no image at all.
 */
func (h *SharePreviewHandler) absoluteURL(c *gin.Context, path string) string {
	if h.baseURL != "" {
		return strings.TrimRight(h.baseURL, "/") + path
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	u := url.URL{Scheme: scheme, Host: host, Path: path}
	return u.String()
}
