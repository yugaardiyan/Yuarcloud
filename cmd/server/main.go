package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/yuarcloud/yuarcloud/internal/admin"
	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/database"
	"github.com/yuarcloud/yuarcloud/internal/handler"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/share"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

func main() {
	// Load .env file if exists
	godotenv.Load()

	// Load configuration
	cfgPath := ""
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// A blank signing key does not make the server fail — it makes it hand out
	// tokens anyone can forge, because the same empty string signs and verifies.
	// The shipped config leaves this empty on purpose, so this check is what
	// stops a fresh `docker run` from coming up publicly forgeable.
	if len(cfg.Auth.JWTSecret) < 32 {
		log.Fatalf(
			"auth.jwt_secret is missing or shorter than 32 characters.\n" +
				"Set it in the config file, or export YUAR_AUTH_JWT_SECRET with a random value, e.g.\n" +
				"  openssl rand -base64 48")
	}

	// Initialize database
	db, err := database.Init(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Drop audit rows past the retention window, now and once a day. Started
	// here rather than inside Init so the goroutine belongs to main's lifetime
	// alongside the chunk sweeper, rather than to a constructor whose caller
	// may never have wanted a background job.
	database.StartAuditRetention(db, database.AuditRetention)

	// Initialize storage backend
	store, err := storage.NewLocalStorage(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// Pre-create the trash root so the very first "move to trash" never
	// races against directory creation on a slow disk and never fails with
	// a confusing "failed to move item to trash" if Mkdir races with a
	// stray file at the trash root.
	if err := store.Mkdir(context.Background(), ".yuarcloud-trash"); err != nil {
		log.Fatalf("Failed to create trash root: %v", err)
	}

	// Initialize auth service
	authService := auth.NewAuthService(
		cfg.Auth.JWTSecret,
		cfg.Auth.TokenTTL,
		cfg.Auth.RefreshTTL,
		cfg.Auth.BcryptCost,
		cfg.Auth.Issuer,
	)

	// Seed admin user. An account created from the shipped default gets a flag
	// that pins it to the password-change endpoint until the credential is
	// replaced. Nothing else is required to trigger it: "yuarcloud"/"yuarcloud"
	// is what the repository, the image and the example config all hand an
	// operator who then exposes the port.
	isDefaultPass := cfg.Admin.DefaultPass == defaultShippedAdminPass
	seeded, err := database.SeedAdmin(db, cfg.Admin.DefaultUser, hashPassword(cfg.Admin.DefaultPass, authService), isDefaultPass)
	if err != nil {
		log.Printf("Warning: failed to seed admin user: %v", err)
	} else if seeded && isDefaultPass {
		log.Printf("WARNING: seeded administrator %q with the default password. "+
			"It may only change its own password until you set a new one.", cfg.Admin.DefaultUser)
	}

	// Seeding only ever covers a database that has never had an admin, which is
	// the least exposed case. An installation that has been running for a year
	// is the one most likely to still hold the published credential and the one
	// nobody was ever prompted about, so check the accounts that already exist.
	if n, err := database.FlagAccountsOnDefaultPassword(db, defaultShippedAdminPass); err != nil {
		log.Printf("Warning: could not check administrator passwords: %v", err)
	} else if n > 0 {
		log.Printf("WARNING: %d administrator account(s) still use the default password. "+
			"They may only change their own password until it is replaced.", n)
	}

	// Initialize services
	shareService := share.NewShareService(
		db,
		cfg.Share.BaseURL,
		cfg.Share.DefaultExpiryDays,
		cfg.Share.MaxExpiryDays,
		cfg.Share.RequirePassword,
		cfg.Share.MaxViews,
	)

	adminService := admin.NewAdminService(db)

	// Initialize handlers
	authHandler := handler.NewAuthHandler(authService, db)
	fileHandler := handler.NewFileHandler(db, store, authService)
	shareHandler := handler.NewShareHandler(db, shareService, store)
	adminHandler := handler.NewAdminHandler(db, adminService, authService, shareService, store)

	// Re-apply whatever an admin last saved, so the panel's settings survive a
	// restart. The YAML values seeded the limits above; these win where set,
	// because the panel is the newer intent.
	if err := adminHandler.ApplyChunkedConfig(context.Background()); err != nil {
		log.Printf("Warning: failed to apply chunked upload settings: %v", err)
	}

	// Setup Gin router
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Slack on top of the file ceiling for the multipart framing — boundaries,
	// the part header, the filename — that is not part of the file. Small and
	// fixed: it exists so a legal upload is not rejected by a limit that was
	// meant to bound file content, not protocol.
	const multipartOverhead = 1 << 20 // 1 MiB

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(gin.Logger())

	// Trust no proxies unless the operator names one.
	//
	// gin defaults to trusting every source, which makes ClientIP() return
	// whatever the caller put in X-Forwarded-For. That is attacker-controlled
	// on any directly-reachable deployment, and it feeds the rate limiter's key
	// and audit_logs.ip_address. nil means "nobody is a proxy", so the real
	// peer address is used. An operator behind nginx or Cloudflare sets
	// server.trusted_proxies to their CIDR and gets per-client attribution
	// back.
	if err := router.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		log.Fatalf("invalid server.trusted_proxies: %v", err)
	}

	// CORS
	corsConfig := cors.Config{
		AllowOrigins:     cfg.CORS.AllowedOrigins,
		AllowMethods:     cfg.CORS.AllowedMethods,
		AllowHeaders:     cfg.CORS.AllowedHeaders,
		AllowCredentials: cfg.CORS.AllowCredentials,
		MaxAge:           cfg.CORS.MaxAge,
	}
	router.Use(cors.New(corsConfig))

	// Rate limiting
	if cfg.RateLimit.Enabled {
		router.Use(middleware.RateLimitMiddleware(cfg.RateLimit.RequestsPerMinute))
	}

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "timestamp": time.Now().Unix()})
	})

	// API routes
	api := router.Group("/api")
	{
		// Public auth routes
		authGroup := api.Group("/auth")
		{
			authGroup.POST("/login", authHandler.Login)
			authGroup.POST("/refresh", authHandler.Refresh)
		}

		// Preview lives outside the protected group on purpose.
		//
		// <img>, <video> and <audio> cannot set an Authorization header, so a
		// media player has to be handed a plain URL. What rides in that URL is
		// NOT the session token — it is a five-minute, single-file preview
		// token minted by IssuePreviewToken. Confining that to one read-only
		// route keeps long-lived credentials out of URLs everywhere else.
		preview := api.Group("/files", middleware.AuthPreviewMiddleware(authService))
		preview.Use(middleware.ScopeMiddleware(db))
		{
			preview.GET("/:id/preview", fileHandler.PreviewFile)
		}

		// Protected routes
		protected := api.Group("")
		protected.Use(middleware.AuthMiddleware(authService))
		protected.Use(middleware.ScopeMiddleware(db))
		// An account still on the shipped default password may do nothing but
		// replace it. Sits inside the protected group so it can trust the
		// resolved user, and after ScopeMiddleware so the exempt set matches
		// real route patterns.
		protected.Use(middleware.PasswordRotationRequiredMiddleware(db, map[string]bool{
			"POST /api/auth/change-password": true,
			"POST /api/auth/logout":          true,
			// Read-only, and the panel needs it to render the screen that
			// explains the situation.
			"GET /api/auth/me": true,
		}))
		{
			// User profile
			protected.GET("/auth/me", authHandler.Me)
			protected.POST("/auth/change-password", authHandler.ChangePassword)
			protected.POST("/auth/logout", authHandler.Logout)

			// Files
			files := protected.Group("/files")
			{
				files.GET("", fileHandler.ListFiles)
				files.GET("/search", fileHandler.Search)
				files.GET("/folders/tree", fileHandler.FolderTree)
				files.GET("/folders/flat", fileHandler.FlatFolderList)
				files.GET("/shared", fileHandler.ListShared)
				files.GET("/stats", fileHandler.StorageStats)
				files.GET("/:id", fileHandler.GetFile)
				files.POST("/folder", fileHandler.CreateFolder)
				// Capped at the same ceiling the handler enforces, plus a
				// little slack for the multipart framing around the part
				// itself. Enforcing only inside the handler was too late:
				// the body had already been streamed to a temp file.
				files.POST("/upload",
					middleware.BodyLimitMiddleware(store.GetMaxFileSize()+multipartOverhead),
					fileHandler.UploadFile)
				files.GET("/:id/download", fileHandler.DownloadFile)
				files.GET("/upload-limits", fileHandler.UploadLimits)
				files.POST("/:id/preview-token", fileHandler.IssuePreviewToken)
				files.GET("/:id/download-zip", fileHandler.DownloadFolder)
				files.DELETE("/:id", fileHandler.DeleteFile)
				files.PATCH("/:id/rename", fileHandler.RenameFile)
				files.POST("/:id/move", fileHandler.MoveFile)
				files.POST("/:id/copy", fileHandler.CopyFile)

				// Trash
				files.POST("/:id/trash", fileHandler.MoveToTrash)
				files.POST("/:id/restore", fileHandler.RestoreFromTrash)
				files.DELETE("/:id/permanent", fileHandler.DeletePermanently)

				// Chunked upload
				files.POST("/upload/chunked/init", fileHandler.InitChunkedUpload)
				files.POST("/upload/chunked/:uploadId/chunk/:chunkIndex",
					middleware.BodyLimitMiddleware(store.GetMaxFileSize()+multipartOverhead),
					fileHandler.UploadChunk)
				files.POST("/upload/chunked/:uploadId/complete", fileHandler.CompleteChunkedUpload)
				files.GET("/upload/chunked/:uploadId/status", fileHandler.GetUploadStatus)
				files.DELETE("/upload/chunked/:uploadId", fileHandler.CancelUpload)
			}

			// Trash (collection-level)
			protected.GET("/trash", fileHandler.ListTrash)
			protected.POST("/trash/empty", fileHandler.EmptyTrash)

			// Shares
			shares := protected.Group("/shares")
			{
				shares.POST("", shareHandler.CreateShare)
				shares.GET("", shareHandler.GetShares)
				shares.DELETE("/:id", shareHandler.DeleteShare)
			}
		}

		// Public share access (no auth)
		publicShares := api.Group("/shares")
		{
			publicShares.GET("/:token", shareHandler.AccessShare)
			publicShares.GET("/:token/download", shareHandler.DownloadSharedFile)
			publicShares.GET("/:token/preview", shareHandler.PreviewSharedFile)
			// Unauthenticated: whoever holds a write-share link can post here,
			// so this one is the difference between a disk-filling request and
			// a rejected one.
			publicShares.POST("/:token/upload",
				middleware.BodyLimitMiddleware(store.GetMaxFileSize()+multipartOverhead),
				shareHandler.ShareUpload)
		}

		// Admin routes
		adminGroup := api.Group("/admin")
		adminGroup.Use(middleware.AuthMiddleware(authService))
		adminGroup.Use(middleware.AdminOnlyMiddleware(db))
		// The panel is reachable without passing through the `protected` group,
		// so the rotation confinement has to be applied again here rather than
		// assumed. It was not: an administrator still on the default credential
		// got 403 on every file route and a fully working /admin panel. That is
		// the worst version of the bug — the confinement appeared to hold, and
		// the one thing a flagged admin could still do was create another
		// account with a known password and sign in as that instead.
		adminGroup.Use(middleware.PasswordRotationRequiredMiddleware(db, nil))
		{
			adminGroup.GET("/stats", adminHandler.GetStats)
			adminGroup.GET("/users", adminHandler.GetUsers)
			adminGroup.GET("/users/:id", adminHandler.GetUser)
			adminGroup.POST("/users", adminHandler.CreateUser)
			adminGroup.PATCH("/users/:id", adminHandler.UpdateUser)
			adminGroup.DELETE("/users/:id", adminHandler.DeleteUser)
			adminGroup.PATCH("/users/:id/quota", adminHandler.UpdateUserQuota)
			adminGroup.PATCH("/users/:id/role", adminHandler.UpdateUserRole)
			adminGroup.GET("/audit-logs", adminHandler.GetAuditLogs)
			adminGroup.GET("/shares", adminHandler.GetShares)
			adminGroup.DELETE("/shares/:id", adminHandler.DeleteShare)
			adminGroup.GET("/chunked-config", adminHandler.GetChunkedUploadConfig)
			adminGroup.POST("/chunked-config", adminHandler.UpdateChunkedUploadConfig)
		}
	}

	// Serve the built frontend. Enabled when web/dist exists, regardless of mode,
	// so `go run ./cmd/server` after `npm run build` works without extra flags.
	if distDir, err := resolveWebDist(); err == nil {
		assetsDir := filepath.Join(distDir, "assets")
		indexFile := filepath.Join(distDir, "index.html")

		// Registered before the static route below: gin copies the handler chain
		// into a route when it is created, so a middleware added afterwards
		// never reaches routes already on the engine.
		//
		// Asset filenames carry a content hash, so a cached copy can never be
		// the wrong one — let them sit in the browser for a year.
		router.Use(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			}
			c.Next()
		})

		if _, err := os.Stat(assetsDir); err == nil {
			router.Static("/assets", assetsDir)
			log.Printf("Serving frontend assets from %s", assetsDir)
		}

		// Link previews for share links.
		//
		// Registered here, before NoRoute, because NoRoute would otherwise hand
		// these paths the untouched shell and the preview tags would never be
		// written. Chat clients read the HTML and never run the app, so a tag
		// that only exists in the DOM after React boots is a tag nobody reading
		// the link ever sees.
		if sharePreviewHandler := handler.NewSharePreviewHandler(shareService, cfg.Share.BaseURL, indexFile); sharePreviewHandler != nil {
			router.GET("/s/:token", sharePreviewHandler.Share)
			router.GET("/yc/:token", sharePreviewHandler.Share)

			if err := ensurePreviewAssets(resolveWebDist); err != nil {
				log.Printf("Warning: could not prepare link-preview assets: %v", err)
			}
			log.Printf("Share links will carry a link preview (og:image at /og.png)")
		}

		if cfg.Share.BaseURL == "" {
			log.Printf("Note: share.base_url is empty, so preview links use the request's Host. " +
				"Previews only appear for a server reachable from the internet.")
		} else {
			log.Printf("Share links will use base_url %s for preview URLs", cfg.Share.BaseURL)
		}

		router.NoRoute(func(c *gin.Context) {
			// Unknown API routes must stay JSON 404s, not the SPA shell.
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.JSON(http.StatusNotFound, gin.H{"error": "endpoint not found"})
				return
			}

			// Serve real files (favicon, robots.txt, ...) when they exist.
			candidate := filepath.Join(distDir, filepath.Clean("/"+c.Request.URL.Path))
			if strings.HasPrefix(candidate, distDir) {
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
					c.File(candidate)
					return
				}
			}

			// The shell must never be cached. It names the hashed bundles, so
			// a stale copy pins the browser to an old index-*.js — and because
			// those files stay on disk after a rebuild, the old app keeps
			// running with no error anywhere to explain why. Served without a
			// Cache-Control header the browser heuristic-caches it, which is
			// exactly how a fix like this one can look like it did nothing.
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.File(indexFile)
		})
		log.Printf("Serving frontend from %s", distDir)
	} else {
		log.Printf("Frontend not built (looked for web/dist). API-only mode — run `npm run build` in web/ or use the Vite dev server.")
	}

	// Start server
	srv := &http.Server{
		Addr:           fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:        router,
		ReadTimeout:    cfg.Server.ReadTimeout,
		WriteTimeout:   cfg.Server.WriteTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
	}

	// Graceful shutdown
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	log.Printf("Yuarcloud server started on %s:%d", cfg.Server.Host, cfg.Server.Port)

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	// After the server stops answering, so nothing can still be mid-transaction.
	// Under WAL this folds the write-ahead log back into the database file: a
	// stopped server then leaves one self-contained y.db, which is what makes
	// "back up /config" true rather than almost true.
	database.Shutdown(db)

	log.Println("Server exited")
}

// defaultShippedAdminPass is the credential the example config, the repository
// docs and the container image all publish. Matching it is what marks a seeded
// account as needing rotation.
//
// It has to stay in step with admin.default_pass in internal/config. When the two
// disagree, the rotation check silently matches nothing: SeedAdmin still sets the
// flag for a fresh install, because that branch compares against the configured
// value, while the sweep for already-existing accounts compares against this
// constant and would wave a year-old default-password admin straight through.
const defaultShippedAdminPass = "yuarcloud"

func hashPassword(password string, authService *auth.AuthService) string {
	hash, _ := authService.HashPassword(password)
	return hash
}

// resolveWebDist locates the built frontend directory, checking the working
// directory first and then walking up a few levels so the binary works from
// bin/, from the repo root, and from a container WORKDIR alike.
func resolveWebDist() (string, error) {
	var candidates []string

	if env := os.Getenv("YUAR_WEB_DIST"); env != "" {
		candidates = append(candidates, env)
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := wd
	for i := 0; i < 4; i++ {
		candidates = append(candidates, filepath.Join(dir, "web", "dist"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("web/dist/index.html not found in any of %d candidate paths", len(candidates))
}
