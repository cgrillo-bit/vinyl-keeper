package router

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	"github.com/ninesl/vinyl-keeper/app/router/assets/pages"
	"github.com/ninesl/vinyl-keeper/app/router/assets/ui"
	"github.com/ninesl/vinyl-keeper/app/router/assets/ui/parts"
	"github.com/ninesl/vinyl-keeper/app/router/values"
	"github.com/ninesl/vinyl-keeper/app/vinyl"
)

// Embedding is a float64 slice
type Embedding []float64

const ConfidenceThreshold = 0.80 // 80%

// parseFilterCriteria extracts filter criteria from query parameters
func parseFilterCriteria(r *http.Request) vinyl.FilterCriteria {
	query := r.URL.Query()
	return vinyl.FilterCriteria{
		Artist: query.Get(values.QueryArtist),
		Genres: nonEmptyValues(query[values.QueryGenre]), // supports multiple values
		Styles: nonEmptyValues(query[values.QueryStyle]), // supports multiple values
	}
}

func nonEmptyValues(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

type ScanHandlerParams struct {
	GetEmbedding           func([]byte) (Embedding, error)
	FindClosestVinylUnqiue func(Embedding) vinyl.VinylUnique
	GetVinyl               func(vinylID int64) *vinyl.VinylUnique
	PlayRecord             func(vinylID, userID int64) error
	GetUserID              func(*http.Request) int64
}

type ScanResult struct {
	Vinyl      vinyl.VinylUnique `json:"vinyl"`
	Found      bool              `json:"found"`
	Similarity float64           `json:"similarity"`
}

type SignedInUser struct {
	UserID   int64
	UserName string
}

func RenderHandler(component templ.Component) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)
		if err := component.Render(r.Context(), w); err != nil {
			http.Error(w, "failed to render html", http.StatusInternalServerError)
		}
	}
}

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func ScanButtonHandler(isSignedIn func(*http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)
		signedIn := isSignedIn(r)
		pages.ScannerButtonShell(signedIn, true).Render(r.Context(), w)
	}
}

func ScannerPageHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		signedIn := IsUserSignedIn(r)
		userName := GetUserName(r)
		pages.ScannerPage(values.TitleScanner, userName, signedIn).Render(r.Context(), w)
	}
}

func ModalMyCollectionHandler(
	getIndex func() *vinyl.VinylIndex,
) http.HandlerFunc {
	return RenderHandler(ui.FilterPanel(values.EndpointMyVinyl+values.EndpointFilter, getIndex(), "my-collection-scope", "my-collection-zone", "my-collection-filter-artist"))
}

func setUserCookie(w http.ResponseWriter, userID int64) {
	http.SetCookie(w, &http.Cookie{
		Name:     values.CookieUserID,
		Value:    strconv.FormatInt(userID, 10),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func AlbumsFilterHandler(
	getAllVinyls func() []vinyl.VinylUnique,
	getIndex func() *vinyl.VinylIndex,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		criteria := parseFilterCriteria(r)
		vinyls := getAllVinyls()
		index := getIndex()

		filtered := vinyl.FilterVinylUnique(vinyls, criteria, index)
		ui.AlbumsGrid(filtered).Render(r.Context(), w)
	}
}

func MyVinylFilterHandler(
	getMyVinyl func(userID int64) []vinyl.VinylWithPlayData,
	getIndex func() *vinyl.VinylIndex,
	getUserID func(*http.Request) int64,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		criteria := parseFilterCriteria(r)
		userID := getUserID(r)
		vinyls := getMyVinyl(userID)
		index := getIndex()

		filtered := vinyl.FilterVinylWithPlayData(vinyls, criteria, index)
		ui.MyVinylGrid(filtered).Render(r.Context(), w)
	}
}

// ScanCoverHandler takes an jpeg blob as a POST and returns
// a json encoded ScanResult
func ScanCoverHandler(params ScanHandlerParams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		//TODO: more idiomatic way to verify blob is an type/image and throw a StatusBadRequest if invalid
		// validate it's actually a JPEG (starts with FF D8)
		if len(body) < 3 || body[0] != 0xFF || body[1] != 0xD8 {
			http.Error(w, "invalid JPEG image", http.StatusBadRequest)
			return
		}

		// ensure jpeg is correct size??
		embedding, err := params.GetEmbedding(body)
		if err != nil {
			http.Error(w, "failed to get embedding: "+err.Error(), http.StatusInternalServerError)
			return
		}
		vinylResult := params.FindClosestVinylUnqiue(embedding)
		vinylEmbedding, err := embeddingFromBlob(vinylResult.CoverEmbedding)
		if err != nil {
			http.Error(w, "failed to decode vinyl embedding: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Calculate similarity
		sim := cosineSimilarity(embedding, vinylEmbedding)

		w.Header().Set("Content-Type", values.ContentTypeJSON)
		json.NewEncoder(w).Encode(ScanResult{
			Vinyl:      vinylResult,
			Found:      vinylResult.VinylID != 0,
			Similarity: sim,
		})
	}
}

func ScanCoverHTMLHandler(params ScanHandlerParams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if r.FormValue(values.QueryConfirm) == "1" {
			vinylIDStr := r.FormValue(values.ParamVinylID)
			if vinylIDStr == "" {
				w.WriteHeader(http.StatusBadRequest)
				parts.ErrorMessage("Missing vinyl ID").Render(r.Context(), w)
				return
			}
			vinylID, err := strconv.ParseInt(vinylIDStr, 10, 64)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				parts.ErrorMessage("Invalid vinyl ID").Render(r.Context(), w)
				return
			}
			if params.GetVinyl == nil {
				w.WriteHeader(http.StatusInternalServerError)
				parts.ErrorMessage("Scanner confirmation is not configured").Render(r.Context(), w)
				return
			}
			v := params.GetVinyl(vinylID)
			if v == nil {
				w.WriteHeader(http.StatusNotFound)
				parts.ErrorMessage("Vinyl not found").Render(r.Context(), w)
				return
			}
			similarityPercent := 100.0
			if simStr := r.FormValue(values.QuerySimilarity); simStr != "" {
				if parsed, parseErr := strconv.ParseFloat(simStr, 64); parseErr == nil {
					similarityPercent = parsed
				}
			}
			renderAcceptedScanResult(w, r, params, *v, similarityPercent)
			return
		}

		// Read image bytes
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		// Optional: validate it's actually a JPEG (starts with FF D8)
		if len(body) < 3 || body[0] != 0xFF || body[1] != 0xD8 {
			parts.ErrorMessage("Invalid image format").Render(r.Context(), w)
			return
		}

		// Get embedding from ONNX service
		embedding, err := params.GetEmbedding(body)
		if err != nil {
			parts.ErrorMessage("Failed to process image: "+err.Error()).Render(r.Context(), w)
			return
		}

		// Find closest vinyl
		vinylResult := params.FindClosestVinylUnqiue(embedding)

		if vinylResult.VinylID == 0 {
			parts.ErrorMessage("No matching vinyl found").Render(r.Context(), w)
			return
		}

		// Decode the embedding from blob to calculate similarity
		vinylEmbedding, err := embeddingFromBlob(vinylResult.CoverEmbedding)
		if err != nil {
			parts.ErrorMessage("Failed to decode vinyl embedding").Render(r.Context(), w)
			return
		}

		// Calculate similarity
		sim := cosineSimilarity(embedding, vinylEmbedding)

		// If confidence is below threshold, show choice card
		if sim < ConfidenceThreshold {
			pages.LowConfidenceChoiceCard(vinylResult, sim*100).Render(r.Context(), w)
			return
		}

		renderAcceptedScanResult(w, r, params, vinylResult, sim*100)
	}
}

func renderAcceptedScanResult(w http.ResponseWriter, r *http.Request, params ScanHandlerParams, vinylResult vinyl.VinylUnique, similarityPercent float64) {
	if params.PlayRecord != nil {
		userID := params.GetUserID(r)
		if userID > 0 {
			if err := params.PlayRecord(vinylResult.VinylID, userID); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				parts.ErrorMessage("Failed to record play: "+err.Error()).Render(r.Context(), w)
				return
			}
		}
	}
	SetHXTrigger(w, values.EventVinylRegistered)
	pages.ScanResultCard(vinylResult, similarityPercent).Render(r.Context(), w)
}

func cosineSimilarity(a, b Embedding) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

func embeddingFromBlob(b []byte) (Embedding, error) {
	if len(b)%8 != 0 {
		return nil, fmt.Errorf("embedding blob not aligned to float64: %d bytes", len(b))
	}
	emb := make(Embedding, len(b)/8)
	for i := range emb {
		bits := uint64(b[i*8]) | uint64(b[i*8+1])<<8 | uint64(b[i*8+2])<<16 | uint64(b[i*8+3])<<24 |
			uint64(b[i*8+4])<<32 | uint64(b[i*8+5])<<40 | uint64(b[i*8+6])<<48 | uint64(b[i*8+7])<<56
		emb[i] = math.Float64frombits(bits)
	}
	return emb, nil
}

type RegisterHandlerParams struct {
	RegisterVinyl   func(artist, album string) (vinyl.VinylUnique, error)
	RegisterVinylID func(masterID int) (vinyl.VinylUnique, error)
}

type RegisterResult struct {
	Success bool              `json:"success"`
	Vinyl   vinyl.VinylUnique `json:"vinyl,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func RegisterSubmitHandler(params RegisterHandlerParams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		var (
			artist         = r.FormValue(values.QueryArtist)
			album          = r.FormValue(values.QueryAlbum)
			masterID       = r.FormValue(values.QueryMasterID)
			nameSearch     = artist != "" && album != ""
			masterIDSearch = masterID != ""
			vinylUnique    vinyl.VinylUnique
			err            error
		)

		if nameSearch && masterIDSearch {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("need to have either artist/album OR master ID, not both").Render(r.Context(), w)
			return
		} else if nameSearch {
			vinylUnique, err = params.RegisterVinyl(artist, album)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				parts.ErrorMessage(err.Error()).Render(r.Context(), w)
				return
			}

		} else if masterIDSearch {
			id, err := strconv.Atoi(masterID)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				parts.ErrorMessage(err.Error()).Render(r.Context(), w)
			}

			vinylUnique, err = params.RegisterVinylID(id)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				parts.ErrorMessage(err.Error()).Render(r.Context(), w)
				return
			}
		} else {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("must provide album/artist or master ID").Render(r.Context(), w)
			return
		}

		SetHXTrigger(w, values.EventVinylRegistered)
		pages.AlbumCard(vinylUnique).Render(r.Context(), w)
	}
}

type DeleteHandlerParams struct {
	DeleteVinyl func(vinylID int64) error
}

func DeleteVinylHandler(params DeleteHandlerParams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		vinylIDStr := r.PathValue(values.ParamVinylID)
		if vinylIDStr == "" {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Missing vinyl ID").Render(r.Context(), w)
			return
		}

		vinylID, err := strconv.ParseInt(vinylIDStr, 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Invalid vinyl ID").Render(r.Context(), w)
			return
		}

		if err := params.DeleteVinyl(vinylID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to delete: "+err.Error()).Render(r.Context(), w)
			return
		}

		//SetHXTrigger(w, values.EventVinylRegistered)
		w.WriteHeader(http.StatusOK)
		//w.Write([]byte(``)) // Empty response - HTMX will remove element
	}
}

type ServeAlbumImageHandlerParams struct {
	GetVinyl func(vinylID int64) *vinyl.VinylUnique
}

func HandleServeAlbumImage(params ServeAlbumImageHandlerParams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vinylIDStr := r.PathValue(values.ParamVinylID)
		if vinylIDStr == "" {
			http.NotFound(w, r)
			return
		}

		vinylID, err := strconv.ParseInt(vinylIDStr, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		vinyl := params.GetVinyl(vinylID)
		if vinyl == nil {
			http.NotFound(w, r)
			return
		}

		// Set proper content type based on extension
		contentType := "image/" + vinyl.ImageExtension
		w.Header().Set("Content-Type", contentType)

		// Set caching headers - covers don't change once stored
		w.Header().Set("Cache-Control", "public, max-age=86400") // 24 hours FIXME: can be permanent/only on change??
		w.Header().Set("ETag", fmt.Sprintf("\"%d\"", vinyl.VinylID))

		w.Write(vinyl.CoverRawBlob)
	}
}
