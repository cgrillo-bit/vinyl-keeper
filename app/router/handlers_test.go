package router

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ninesl/vinyl-keeper/app/vinyl"
)

func TestScanCoverHTMLHandler_HighConfidence(t *testing.T) {
	// Load test image
	imagePath := filepath.Join("..", "..", "photos", "album_1.jpg")
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("Failed to read test image: %v", err)
	}

	// Mock embedding that will return high similarity
	testEmbedding := make(Embedding, 512)
	for i := range testEmbedding {
		testEmbedding[i] = 1.0 // Will give high cosine similarity
	}

	// Mock vinyl with matching embedding
	mockVinyl := vinyl.VinylRecord{
		VinylID:           1,
		VinylTitle:        "Test Album",
		VinylArtist:       "Test Artist",
		VinylPressingYear: 2020,
		ImageExtension:    "jpg",
		CoverEmbedding:    embeddingToBlob(testEmbedding),
		CoverRawBlob:      imageData,
	}

	params := ScanHandlerParams{
		GetEmbedding: func(img []byte) (Embedding, error) {
			return testEmbedding, nil
		},
		FindClosestVinylUnqiue: func(emb Embedding) vinyl.VinylRecord {
			return mockVinyl
		},
	}

	handler := ScanCoverHTMLHandler(params)

	req := httptest.NewRequest(http.MethodPost, "/search/htmx", bytes.NewReader(imageData))
	req.Header.Set("Content-Type", "image/jpeg")
	w := httptest.NewRecorder()

	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()

	if !strings.Contains(body, "Test Album") {
		t.Error("Response should contain vinyl title")
	}

	// Should show choice buttons (no auto-accept anymore)
	if !strings.Contains(body, "Yes, this is correct") {
		t.Error("Response should contain accept button")
	}

	if !strings.Contains(body, "No, register as new vinyl") {
		t.Error("Response should contain register button")
	}

	// Should contain confidence percentage badge
	if !strings.Contains(body, "%") {
		t.Error("Response should contain confidence percentage")
	}
}

func TestScanCoverHTMLHandler_LowConfidence(t *testing.T) {
	// Load test image
	imagePath := filepath.Join("..", "..", "photos", "album_1.jpg")
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("Failed to read test image: %v", err)
	}

	// Mock embedding for scanned image
	scannedEmbedding := make(Embedding, 512)
	for i := range scannedEmbedding {
		scannedEmbedding[i] = 1.0
	}

	// Mock vinyl with different embedding (low similarity)
	storedEmbedding := make(Embedding, 512)
	for i := range storedEmbedding {
		if i < 256 {
			storedEmbedding[i] = 1.0
		} else {
			storedEmbedding[i] = -1.0 // Opposite values for low similarity
		}
	}

	mockVinyl := vinyl.VinylRecord{
		VinylID:           1,
		VinylTitle:        "Different Album",
		VinylArtist:       "Different Artist",
		VinylPressingYear: 2020,
		ImageExtension:    "jpg",
		CoverEmbedding:    embeddingToBlob(storedEmbedding),
		CoverRawBlob:      imageData,
	}

	params := ScanHandlerParams{
		GetEmbedding: func(img []byte) (Embedding, error) {
			return scannedEmbedding, nil
		},
		FindClosestVinylUnqiue: func(emb Embedding) vinyl.VinylRecord {
			return mockVinyl
		},
	}

	handler := ScanCoverHTMLHandler(params)

	req := httptest.NewRequest(http.MethodPost, "/search/htmx", bytes.NewReader(imageData))
	req.Header.Set("Content-Type", "image/jpeg")
	w := httptest.NewRecorder()

	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()

	if !strings.Contains(body, "Different Album") {
		t.Error("Response should contain matched vinyl title")
	}

	// Should show both choice buttons
	if !strings.Contains(body, "Yes, this is correct") {
		t.Error("Response should contain accept button")
	}

	if !strings.Contains(body, "No, register as new vinyl") {
		t.Error("Response should contain register button")
	}

	if !strings.Contains(body, "confirm") || !strings.Contains(body, "vinyl_id") {
		t.Error("Response should post confirmation payload back to scan endpoint")
	}

	// Should contain confidence percentage badge
	if !strings.Contains(body, "%") {
		t.Error("Response should contain confidence percentage")
	}
}

func TestScanCoverHTMLHandler_ConfirmFlow_PassesUserIDToPlayRecord(t *testing.T) {
	mockVinyl := vinyl.VinylRecord{
		VinylID:           1,
		VinylTitle:        "Confirmed Album",
		VinylArtist:       "Confirmed Artist",
		VinylPressingYear: 2020,
		ImageExtension:    "jpg",
	}

	var gotVinylID int64
	var gotUserID int64

	params := ScanHandlerParams{
		GetVinyl: func(vinylID int64) *vinyl.VinylRecord {
			if vinylID != 1 {
				t.Fatalf("expected vinylID 1 in confirm flow, got %d", vinylID)
			}
			return &mockVinyl
		},
		PlayRecord: func(vinylID, userID int64) error {
			gotVinylID = vinylID
			gotUserID = userID
			return nil
		},
		GetUserID: func(*http.Request) int64 {
			return 99
		},
	}

	handler := ScanCoverHTMLHandler(params)
	req := httptest.NewRequest(http.MethodPost, "/search/htmx?confirm=1&vinyl_id=1&similarity=88.8", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Result().StatusCode)
	}
	if gotVinylID != 1 {
		t.Fatalf("Expected PlayRecord vinylID=1, got %d", gotVinylID)
	}
	if gotUserID != 99 {
		t.Fatalf("Expected PlayRecord userID=99, got %d", gotUserID)
	}
}

func TestMyVinylFilterHandler_PassesUserID(t *testing.T) {
	var gotUserID int64

	getMyVinyl := func(userID int64) []vinyl.VinylWithPlayData {
		gotUserID = userID
		return []vinyl.VinylWithPlayData{}
	}

	index := vinyl.BuildVinylIndex([]vinyl.VinylRecord{})
	handler := MyVinylFilterHandler(
		getMyVinyl,
		func() *vinyl.VinylIndex { return index },
		func(*http.Request) int64 { return 7 },
	)

	req := httptest.NewRequest(http.MethodGet, "/myvinyl/filter", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Result().StatusCode)
	}
	if gotUserID != 7 {
		t.Fatalf("Expected getMyVinyl userID=7, got %d", gotUserID)
	}
}

func TestScanCoverHTMLHandler_InvalidImage(t *testing.T) {
	params := ScanHandlerParams{
		GetEmbedding: func(img []byte) (Embedding, error) {
			return nil, nil
		},
		FindClosestVinylUnqiue: func(emb Embedding) vinyl.VinylRecord {
			return vinyl.VinylRecord{}
		},
	}

	handler := ScanCoverHTMLHandler(params)

	// Send invalid image data (not a JPEG)
	invalidData := []byte("not a jpeg")
	req := httptest.NewRequest(http.MethodPost, "/search/htmx", bytes.NewReader(invalidData))
	req.Header.Set("Content-Type", "image/jpeg")
	w := httptest.NewRecorder()

	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	body := w.Body.String()

	if !strings.Contains(body, "Invalid image format") {
		t.Error("Should return invalid image format error")
	}
}

func TestScanCoverHTMLHandler_NoVinylFound(t *testing.T) {
	imagePath := filepath.Join("..", "..", "photos", "album_1.jpg")
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("Failed to read test image: %v", err)
	}

	testEmbedding := make(Embedding, 512)

	params := ScanHandlerParams{
		GetEmbedding: func(img []byte) (Embedding, error) {
			return testEmbedding, nil
		},
		FindClosestVinylUnqiue: func(emb Embedding) vinyl.VinylRecord {
			// Return zero-value vinyl (VinylID = 0)
			return vinyl.VinylRecord{}
		},
	}

	handler := ScanCoverHTMLHandler(params)

	req := httptest.NewRequest(http.MethodPost, "/search/htmx", bytes.NewReader(imageData))
	req.Header.Set("Content-Type", "image/jpeg")
	w := httptest.NewRecorder()

	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	body := w.Body.String()

	if !strings.Contains(body, "No matching vinyl found") {
		t.Error("Should return no matching vinyl error")
	}
}

// Helper function to convert Embedding to blob for tests
func embeddingToBlob(emb Embedding) []byte {
	blob := make([]byte, len(emb)*8)
	for i, val := range emb {
		bits := floatToBits(val)
		blob[i*8] = byte(bits)
		blob[i*8+1] = byte(bits >> 8)
		blob[i*8+2] = byte(bits >> 16)
		blob[i*8+3] = byte(bits >> 24)
		blob[i*8+4] = byte(bits >> 32)
		blob[i*8+5] = byte(bits >> 40)
		blob[i*8+6] = byte(bits >> 48)
		blob[i*8+7] = byte(bits >> 56)
	}
	return blob
}

func floatToBits(f float64) uint64 {
	// Use math package to properly convert float64 to bits
	return math.Float64bits(f)
}
