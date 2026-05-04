package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ninesl/vinyl-keeper/app/router"
)

// Embedding is a dense float64 vector (e.g. 512-d from the ONNX model).
type Embedding []float64

// Vinyl is the domain type used for in-memory search.
type Vinyl struct {
	VinylID int64 // Associated with sqlc vinyl.VinylUnique
	// Title     string
	// Artist    string
	Embedding Embedding
}

// func (v Vinyl) VinylUnique(queries vinyl.Queries) vinyl.VinylUnique {
// }

// EmbeddingFromBlob decodes a little-endian float64 BLOB (as stored in SQLite)
// into an Embedding slice.
func EmbeddingFromBlob(b []byte) (Embedding, error) {
	if len(b)%8 != 0 {
		return nil, fmt.Errorf("embedding blob not aligned to float64: %d bytes", len(b))
	}
	emb := make(Embedding, len(b)/8)
	for i := range emb {
		emb[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return emb, nil
}

// EmbeddingToBlob serializes an Embedding to raw little-endian float64 bytes
// for SQLite BLOB storage.
func EmbeddingToBlob(emb Embedding) []byte {
	buf := make([]byte, len(emb)*8)
	for i, v := range emb {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

// RequestEmbedding sends image bytes to the Python ONNX service and returns
// the parsed float64 embedding vector.
func RequestEmbedding(imgData []byte) (Embedding, error) {
	params, err := loadImageEmbedParams(imgData)
	if err != nil {
		return nil, fmt.Errorf("env config: %w", err)
	}
	log.Printf("[Embed] Sending image to python service endpoint=http://%s:%d%s", params.Host, params.Port, params.Endpoint)

	resp, err := router.SendImageBytes(params)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("[Embed] Python service response status=%d", resp.StatusCode)

	// Binary format: raw float64 LE bytes (embedding)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty response from image service")
	}

	emb, err := EmbeddingFromBlob(body)
	if err != nil {
		return nil, fmt.Errorf("parse embedding: %w", err)
	}
	return emb, nil
}

func loadImageEmbedParams(imgData []byte) (router.ImageEmbedParams, error) {
	host := os.Getenv("IMAGE_SERVICE_HOST")
	if host == "" {
		return router.ImageEmbedParams{}, fmt.Errorf("IMAGE_SERVICE_HOST is not set")
	}

	portRaw := os.Getenv("IMAGE_SERVICE_PORT")
	if portRaw == "" {
		return router.ImageEmbedParams{}, fmt.Errorf("IMAGE_SERVICE_PORT is not set")
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		return router.ImageEmbedParams{}, fmt.Errorf("invalid IMAGE_SERVICE_PORT: %w", err)
	}

	endpoint := os.Getenv("IMAGE_SERVICE_ENDPOINT")
	if endpoint == "" {
		return router.ImageEmbedParams{}, fmt.Errorf("IMAGE_SERVICE_ENDPOINT is not set")
	}

	return router.ImageEmbedParams{
		ImgData:  imgData,
		Host:     host,
		Port:     port,
		Endpoint: endpoint,
	}, nil
}

func waitForImageServiceHealth() error {
	host := os.Getenv("IMAGE_SERVICE_HOST")
	if host == "" {
		return fmt.Errorf("IMAGE_SERVICE_HOST is not set")
	}

	portRaw := os.Getenv("IMAGE_SERVICE_PORT")
	if portRaw == "" {
		return fmt.Errorf("IMAGE_SERVICE_PORT is not set")
	}

	port, err := strconv.Atoi(portRaw)
	if err != nil {
		return fmt.Errorf("invalid IMAGE_SERVICE_PORT: %w", err)
	}

	healthEndpoint := os.Getenv("IMAGE_SERVICE_HEALTH_ENDPOINT")
	if healthEndpoint == "" {
		healthEndpoint = "/health"
	}

	retries := 60
	if raw := os.Getenv("IMAGE_SERVICE_WAIT_RETRIES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return fmt.Errorf("invalid IMAGE_SERVICE_WAIT_RETRIES: %q", raw)
		}
		retries = parsed
	}

	waitSeconds := 1
	if raw := os.Getenv("IMAGE_SERVICE_WAIT_SECONDS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return fmt.Errorf("invalid IMAGE_SERVICE_WAIT_SECONDS: %q", raw)
		}
		waitSeconds = parsed
	}

	url := fmt.Sprintf("http://%s:%d%s", host, port, healthEndpoint)
	client := &http.Client{Timeout: 2 * time.Second}

	for i := 0; i < retries; i++ {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(time.Duration(waitSeconds) * time.Second)
	}

	return fmt.Errorf("image service did not become healthy at %s after %d attempts", url, retries)
}
