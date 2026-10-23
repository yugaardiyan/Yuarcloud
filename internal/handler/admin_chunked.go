package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
)

// Chunked-upload ceilings: reading them, writing them, and pushing the stored
// values into the live limits the upload path reads on every request.
//
// The defaults here mirror configs/config.yaml.example and exist until an admin
// overrides them. formatDuration answers the way the config spells a duration,
// because the value round-trips through a form that offers "15m", "1h", "24h"
// and cannot match Go's own "1h0m0s".

// maxAllowedChunkSize keeps a single upload request under the 100 MB ceiling
// imposed by Cloudflare Tunnel's free tier.
const maxAllowedChunkSize int64 = 100 * 1024 * 1024

func (h *AdminHandler) GetChunkedUploadConfig(c *gin.Context) {
	// Report the values the server is actually enforcing, not just what is in
	// the database. They normally agree — ApplyChunkedConfig writes through on
	// save and again at boot — but the live numbers are what an upload will be
	// judged against, so showing anything else risks the page disagreeing with
	// the behaviour the user is trying to configure.
	limits := h.storage.GetLimits()
	concurrency := h.settingInt64(c.Request.Context(), settingKeyMaxConcurrentUploads, defaultMaxConcurrentUploads)

	c.JSON(http.StatusOK, gin.H{
		"max_chunk_size":         limits.MaxChunkSize(),
		"max_file_size":          limits.MaxFileSize(),
		"max_concurrent_uploads": normalizeConcurrentUploads(concurrency),
		"temp_dir":               defaultTempDir,
		"cleanup_interval":       formatDuration(limits.CleanupInterval()),
	})
}

// formatDuration writes a duration the way the config files and the admin form
// both spell it: "15m", "1h", "24h".
//
// time.Duration.String() writes the whole breakdown instead — six hours comes
// back as "6h0m0s" — so the value an admin saved as "6h" was reported as
// "6h0m0s" and matched no option on the form that had just sent it. Every load
// of that page showed the first entry in the dropdown instead, and saving from
// it overwrote whatever was really configured. Anything that does not land on a
// whole unit falls through to Go's own spelling, which is at least honest.
//
// The alternative was returning the string from the settings table verbatim.
// That would round-trip too, but it reports what was stored rather than what is
// being enforced, which is the opposite of what this endpoint is for: the live
// limit is the one an upload is actually judged against.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	// Seconds only while there are none of the larger units to keep. Past a
	// minute, "1m30s" reads better than the "90s" the same duration is, and
	// Go's own spelling is the more legible of the two.
	case d < time.Minute && d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	default:
		return d.String()
	}
}

func (h *AdminHandler) UpdateChunkedUploadConfig(c *gin.Context) {
	var req struct {
		MaxChunkSize         int64  `json:"max_chunk_size"`
		MaxFileSize          int64  `json:"max_file_size"`
		MaxConcurrentUploads int    `json:"max_concurrent_uploads"`
		CleanupInterval      string `json:"cleanup_interval"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// Reject values the upload path cannot honour.
	if req.MaxChunkSize > maxAllowedChunkSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("max_chunk_size must be at most %d bytes", maxAllowedChunkSize)})
		return
	}
	if req.MaxFileSize > 0 && req.MaxChunkSize > 0 && req.MaxFileSize < req.MaxChunkSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_file_size must be larger than max_chunk_size"})
		return
	}
	// Zero means the field was absent from an older client, so leave the stored
	// value alone. A present value has to be inside the range the uploader
	// enforces rather than silently clamped.
	if req.MaxConcurrentUploads != 0 && (req.MaxConcurrentUploads < 1 || req.MaxConcurrentUploads > maxAllowedConcurrentUploads) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("max_concurrent_uploads must be between 1 and %d", maxAllowedConcurrentUploads)})
		return
	}

	updates := map[string]string{}
	if req.MaxChunkSize > 0 {
		updates[settingKeyMaxChunkSize] = strconv.FormatInt(req.MaxChunkSize, 10)
	}
	if req.MaxFileSize > 0 {
		updates[settingKeyMaxFileSize] = strconv.FormatInt(req.MaxFileSize, 10)
	}
	if req.MaxConcurrentUploads > 0 {
		updates[settingKeyMaxConcurrentUploads] = strconv.Itoa(req.MaxConcurrentUploads)
	}
	if req.CleanupInterval != "" {
		if _, err := time.ParseDuration(req.CleanupInterval); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cleanup_interval must be a duration such as 30m or 1h"})
			return
		}
		updates[settingKeyCleanupInterval] = req.CleanupInterval
	}

	for key, value := range updates {
		if err := h.adminService.UpdateSetting(ctx, key, value, "Chunked upload configuration", true); err != nil {
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save configuration"})
			return
		}
	}

	// Persisting is not the end of it — push the new values into the running
	// limits so they take effect without a restart.
	if err := h.ApplyChunkedConfig(ctx); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply configuration"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_chunked_config", "setting", nil, "Updated chunked upload configuration")

	c.JSON(http.StatusOK, gin.H{"message": "chunked upload config updated"})
}

// Upload defaults. The chunked ceilings mirror configs/config.yaml.example and
// are used until an admin overrides them; parallel uploads is a browser-side
// setting with no YAML equivalent, so three is the floor it starts from.
const (
	settingKeyMaxChunkSize         = "storage.chunked.max_chunk_size"
	settingKeyMaxFileSize          = "storage.chunked.max_file_size"
	settingKeyCleanupInterval      = "storage.chunked.cleanup_interval"
	settingKeyMaxConcurrentUploads = "upload.max_concurrent"

	defaultMaxChunkSize         int64 = 10 * 1024 * 1024
	defaultCleanupInterval            = "1h"
	defaultTempDir                    = "./data/tmp/chunks"
	defaultMaxConcurrentUploads       = 3
	maxAllowedConcurrentUploads       = 10
)

// normalizeConcurrentUploads keeps a hand-edited setting inside the range the
// uploader and the admin form both promise. Missing or broken values fall back
// to the default rather than turning a typo into zero parallel uploads.
func normalizeConcurrentUploads(v int64) int {
	if v < 1 {
		return defaultMaxConcurrentUploads
	}
	if v > maxAllowedConcurrentUploads {
		return maxAllowedConcurrentUploads
	}
	return int(v)
}

// ApplyChunkedConfig pushes the persisted ceilings into the live limits the
// upload path reads on every request.
//
// This is the step that was missing. The handler used to save these to the
// database and stop there; nothing consumed them, so the numbers on the settings
// page were decorative until someone edited the YAML and restarted. Saving now
// has to reach into the running process or the page is lying about what it does.
func (h *AdminHandler) ApplyChunkedConfig(ctx context.Context) error {
	limits := h.storage.GetLimits()

	if v := h.settingInt64(ctx, settingKeyMaxChunkSize, defaultMaxChunkSize); v > 0 {
		limits.SetMaxChunkSize(v)
	}
	// Zero means "no ceiling" rather than "reject everything", so it is left to
	// whatever is already in place rather than forced to the default.
	if v := h.settingInt64(ctx, settingKeyMaxFileSize, 0); v > 0 {
		limits.SetMaxFileSize(v)
	}
	if raw := h.settingString(ctx, settingKeyCleanupInterval, defaultCleanupInterval); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			limits.SetCleanupInterval(d)
		}
	}
	return nil
}

// settingInt64 reads a numeric setting, falling back on parse failure.
func (h *AdminHandler) settingInt64(ctx context.Context, key string, fallback int64) int64 {
	raw, err := h.adminService.GetSetting(ctx, key)
	if err != nil || raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func (h *AdminHandler) settingString(ctx context.Context, key, fallback string) string {
	raw, err := h.adminService.GetSetting(ctx, key)
	if err != nil || raw == "" {
		return fallback
	}
	return raw
}
