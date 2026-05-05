package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	// Embed Mozilla's root CA certificates directly into the binary.
	//
	// WHY: This app makes HTTPS calls to api.discogs.com (see keeper.go:143-156).
	// TLS certificate validation requires trusted root CA certificates to verify
	// that the remote server's certificate is legitimate.
	//
	// Normally, Go reads these from the OS filesystem (e.g., /etc/ssl/certs/).
	// But minimal containers (Alpine, scratch, distroless) often don't include them.
	//
	// WHAT THIS DOES: The x509roots/fallback package embeds ~200KB of Mozilla's
	// root certificates into the compiled binary. When Go can't find system certs,
	// it automatically falls back to these embedded ones.
	//
	// BENEFIT: The binary becomes completely self-contained for HTTPS - no need
	// to install ca-certificates packages or mount cert volumes in containers.
	//
	// Official documentation: https://pkg.go.dev/golang.org/x/crypto/x509roots/fallback
	// Background reading: https://go.dev/blog/certpool
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/ninesl/vinyl-keeper/app/router"
	"github.com/ninesl/vinyl-keeper/app/router/assets/ui"
	"github.com/ninesl/vinyl-keeper/app/router/values"
	"github.com/ninesl/vinyl-keeper/app/vinyl"
)

//go:embed router/assets
var assetsFS embed.FS

func setEmbeddingRoutes(r *router.Router, keeper *keeper) {
	// Search endpoint (JSON response)
	r.Route(http.MethodPost,
		values.EndpointSearch,
		router.ScanCoverHandler(router.ScanHandlerParams{
			GetEmbedding: func(imgData []byte) (router.Embedding, error) {
				emb, err := RequestEmbedding(imgData)
				if err != nil {
					return nil, err
				}
				// Convert Embedding type to router.Embedding
				result := make(router.Embedding, len(emb))
				copy(result, emb)
				return result, nil
			},
			FindClosestVinylUnqiue: func(emb router.Embedding) vinyl.VinylRecord {
				// Convert router.Embedding to main.Embedding
				mainEmb := make(Embedding, len(emb))
				copy(mainEmb, emb)
				return keeper.FindClosestVinyl(mainEmb)
			},
			PlayRecord: keeper.PlayRecord,
			GetUserID:  router.GetUserID,
		}))

	// Search endpoint (HTML response for scanner) — triggers vinyl-registered on play
	r.Route(http.MethodPost,
		values.EndpointSearch+values.EndpointHTMX,
		router.ScanCoverHTMLHandler(router.ScanHandlerParams{
			GetEmbedding: func(imgData []byte) (router.Embedding, error) {
				emb, err := RequestEmbedding(imgData)
				if err != nil {
					return nil, err
				}
				result := make(router.Embedding, len(emb))
				copy(result, emb)
				return result, nil
			},
			FindClosestVinylUnqiue: func(emb router.Embedding) vinyl.VinylRecord {
				mainEmb := make(Embedding, len(emb))
				copy(mainEmb, emb)
				return keeper.FindClosestVinyl(mainEmb)
			},
			FindClosestVinyls: func(emb router.Embedding, n int) []vinyl.VinylRecord {
				mainEmb := make(Embedding, len(emb))
				copy(mainEmb, emb)
				return keeper.FindClosestVinyls(mainEmb, n)
			},
			FindClosestReleaseCandidates: func(emb router.Embedding, n int, userID int64) []vinyl.ReleaseCandidate {
				mainEmb := make(Embedding, len(emb))
				copy(mainEmb, emb)
				return keeper.FindClosestReleaseCandidates(mainEmb, n, userID)
			},
			GetVinyl:            keeper.GetVinyl,
			GetReleaseCandidate: keeper.GetReleaseCandidate,
			PlayRecord:          keeper.PlayRecord,
			PlayRecordRelease:   keeper.PlayRecordRelease,
			GetUserID:           router.GetUserID,
		}))

}

func routeAssets(r *router.Router) {
	assetsSub, err := fs.Sub(assetsFS, "router/assets")
	if err != nil {
		log.Fatalf("failed to create assets fs: %v", err)
	}
	r.Route(http.MethodGet,
		values.EndpointAssets+"/{path...}",
		http.StripPrefix(values.EndpointAssets,
			http.FileServer(http.FS(assetsSub))).ServeHTTP)

}

func TestHandlerInit(r *router.Router) {
	r.Use(router.RecoveryMiddleware)
	r.Use(router.LoggingMiddleware)
	r.Use(router.AuthMiddleware)
}

func main() {
	log.Println("[Init] Creating keeper")
	k, err := NewKeeper()
	if err != nil {
		log.Fatalf("failed to create keeper: %v", err)
	}

	keeper := k.(*keeper)
	log.Println("[Init] Keeper created successfully")

	log.Println("[Init] Waiting for image service health check")
	if err := waitForImageServiceHealth(); err != nil {
		log.Fatalf("image service health check failed: %v", err)
	}
	log.Println("[Init] Image service health check passed")

	if runMainReleaseMigration() {
		log.Println("[Migration] Starting release + plays migration")
		if err := keeper.MigrateMainReleaseEmbeddings(); err != nil {
			log.Fatalf("main-release migration failed: %v", err)
		}
		if err := keeper.MigrateLegacyUserVinylPlays(); err != nil {
			log.Fatalf("legacy plays migration failed: %v", err)
		}
		log.Println("[Migration] Release + plays migration complete")
		return
	}

	log.Println("[Init] Creating router")
	r := router.NewRouter()
	TestHandlerInit(r)
	log.Println("[Init] Middleware registration complete")

	r.Route(http.MethodGet, values.EndpointHealth, router.HealthHandler)
	routeAssets(r)

	// Main page - scanner interface
	r.Route(http.MethodGet,
		"/",
		router.ScannerPageHandler(),
	)

	// Modal content routes
	r.Route(http.MethodGet,
		values.EndpointModal+"/my-collection",
		router.ModalMyCollectionHandler(keeper.GetVinylIndex))

	r.Route(http.MethodGet,
		values.EndpointModal+"/my-collection/"+values.PageParam(values.ParamVinylID)+values.EndpointPressings,
		router.PressingChoiceModalHandler(router.PressingModalHandlerParams{
			GetUserID:         router.GetUserID,
			GetVinyl:          keeper.GetVinyl,
			ListPressingItems: keeper.ListPressingOptions,
		}))

	r.Route(http.MethodGet,
		values.EndpointModal+"/register",
		router.RenderHandler(ui.VinylRegisterForm()),
	)

	// Sign-in modal - shows current sign-in status in the panel
	r.Route(http.MethodGet,
		values.EndpointModal+"/sign-in",
		router.SignInPanelHandler(),
	)

	// Scanner button (OOB swap on sign-in/sign-out events)
	r.Route(http.MethodGet,
		values.EndpointSearch+values.EndpointButton,
		router.ScanButtonHandler(router.IsUserSignedIn))

	// Sign-in related routes
	r.Route(http.MethodGet,
		values.EndpointSignIn+values.EndpointButton,
		router.SignInButtonHandler(keeper))

	r.Route(http.MethodGet,
		values.EndpointSignIn+values.EndpointBootstrap,
		router.SignInBootstrapHandler())

	r.Route(http.MethodGet,
		values.EndpointSignIn+values.EndpointUsers,
		router.SignInUsersListHandler(keeper))

	r.Route(http.MethodPost,
		values.EndpointSignIn+values.EndpointSubmit,
		router.SignInSubmitHandler(keeper))

	r.Route(http.MethodPost,
		values.EndpointSignIn+values.EndpointNew,
		router.SignInCreateUserHandler(keeper))

	r.Route(http.MethodPost,
		values.EndpointSignIn+values.EndpointUserDelete,
		router.SignInDeleteUserHandler(keeper))

	// Sign-out route
	r.Route(http.MethodPost,
		values.EndpointSignIn+values.EndpointSignOut,
		router.SignOutHandler())

	// Nav auth buttons (refreshes on sign-in/sign-out)
	r.Route(http.MethodGet,
		values.EndpointNavAuthButtons,
		router.NavAuthButtonsHandler())

	r.Route(http.MethodGet,
		values.EndpointMyVinyl+values.EndpointFilter,
		router.MyVinylFilterHandler(keeper.MyVinyl, keeper.GetVinylIndex, router.GetUserID))

	r.Route(http.MethodPost,
		values.EndpointMyVinyl+values.EndpointRelease+values.EndpointChange,
		router.ChangePressingHandler(router.ChangePressingHandlerParams{
			GetUserID:          router.GetUserID,
			ChangeUserPressing: keeper.ChangeUserPressing,
			GetIndex:           keeper.GetVinylIndex,
		}))

	log.Println("[Init] Registering embedding routes")
	setEmbeddingRoutes(r, keeper)

	// Register submit (HTMX endpoint) — triggers vinyl-registered on success
	r.Route(http.MethodPost,
		values.EndpointRegister+values.EndpointSubmit,
		router.RegisterSubmitHandler(router.RegisterHandlerParams{
			RegisterVinyl: func(artist, album string) (vinyl.VinylRecord, error) {
				if masterID, resolveErr := FindDiscogsMasterID(album, artist); resolveErr == nil && masterID > 0 {
					mid := int64(masterID)
					if existing := keeper.FindExistingVinyl("", "", &mid); existing != nil {
						return *existing, nil
					}
				}
				params, err := RegisterUniqueVinylAlbumArtist(album, artist)
				if err != nil {
					return vinyl.VinylRecord{}, err
				}
				return keeper.RegisterVinylUnique(params)
			},
			RegisterVinylID: func(masterID int) (vinyl.VinylRecord, error) {
				mid := int64(masterID)
				if existing := keeper.FindExistingVinyl("", "", &mid); existing != nil {
					return *existing, nil
				}
				params, err := RegisterUniqueVinylMasterID(masterID)
				if err != nil {
					return vinyl.VinylRecord{}, err
				}
				return keeper.RegisterVinylUnique(params)
			},
			FindExistingVinyl:   keeper.FindExistingVinyl,
			GetPrimaryReleaseID: keeper.GetPrimaryReleaseID,
		}))

	// Delete vinyl (HTMX endpoint) — triggers vinyl-registered on success
	r.Route(http.MethodDelete,
		values.EndpointDelete+"/"+values.PageParam(values.ParamVinylID),
		router.DeleteVinylHandler(router.DeleteHandlerParams{
			DeleteVinyl: func(vinylID int64) error {
				return keeper.DeleteVinyl(vinylID)
			},
		}))

	// Cover image serving
	r.Route(http.MethodGet,
		values.EndpointCover+"/"+values.PageParam(values.ParamVinylID),
		router.HandleServeAlbumImage(router.ServeAlbumImageHandlerParams{
			GetVinyl:        keeper.GetVinyl,
			GetReleaseCover: keeper.GetReleaseCover,
		}))

	r.Route(http.MethodGet,
		values.EndpointCover+"/"+values.PageParam(values.ParamVinylID)+"/"+values.PageParam(values.ParamReleaseID),
		router.HandleServeAlbumImage(router.ServeAlbumImageHandlerParams{
			GetVinyl:        keeper.GetVinyl,
			GetReleaseCover: keeper.GetReleaseCover,
		}))

	// Get the router handler and wrap with auth middleware
	log.Println("[Init] Building HTTP handler")
	baseHandler, err := r.ServeHTTP()
	if err != nil {
		log.Fatalf("failed to setup router: %v", err)
	}

	handler := baseHandler
	log.Println("[Init] Router setup complete")

	addr := ":8080"
	enableTLS := true
	log.Println("[Init] Configuring TLS settings")
	if raw := os.Getenv("ENABLE_TLS"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			log.Fatalf("invalid ENABLE_TLS value %q: %v", raw, err)
		}
		enableTLS = parsed
	}

	if enableTLS {
		log.Println("[Init] TLS enabled")
		certFile := os.Getenv("TLS_CERT")
		keyFile := os.Getenv("TLS_KEY")

		if certFile == "" || keyFile == "" {
			log.Fatal("TLS is enabled but TLS_CERT and TLS_KEY are not set")
		}

		fmt.Printf("Server listening on https://0.0.0.0%s\n", addr)
		fmt.Printf("Using TLS cert: %s\n", certFile)

		log.Println("[Init] Starting HTTPS server")
		if err := http.ListenAndServeTLS(addr, certFile, keyFile, handler); err != nil {
			log.Fatalf("server error: %v", err)
		}
		log.Println("[Shutdown] Server shutting down")
	} else {
		log.Println("[Init] TLS disabled")
		fmt.Printf("Server listening on http://0.0.0.0%s (tls disabled)\n", addr)
		log.Println("[Init] Starting HTTP server")

		if err := http.ListenAndServe(addr, handler); err != nil {
			log.Fatalf("server error: %v", err)
		}
		log.Println("[Shutdown] Server shutting down")
	}
}

func runMainReleaseMigration() bool {
	v := strings.TrimSpace(os.Getenv("MIGRATE_MAIN_RELEASES"))
	if v == "" {
		return false
	}

	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
