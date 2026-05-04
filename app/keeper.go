package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ninesl/vinyl-keeper/app/vinyl"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	discogsMinRequestInterval     = 5 * time.Second
	discogsFailureRequestInterval = 15 * time.Second
)

var (
	discogsRequestGateMu sync.Mutex
	discogsNextAllowedAt time.Time
	discogsRequestDelay  = discogsMinRequestInterval
)

func waitForDiscogsRequestSlot() {
	discogsRequestGateMu.Lock()
	defer discogsRequestGateMu.Unlock()

	now := time.Now()
	if now.Before(discogsNextAllowedAt) {
		wait := time.Until(discogsNextAllowedAt)
		log.Printf("[Discogs] rate-limit wait=%s", wait.Round(time.Millisecond))
		time.Sleep(wait)
	}

	discogsNextAllowedAt = time.Now().Add(discogsRequestDelay)
}

func recordDiscogsRequestResult(success bool) {
	discogsRequestGateMu.Lock()
	defer discogsRequestGateMu.Unlock()

	if success {
		discogsRequestDelay = discogsMinRequestInterval
		next := time.Now().Add(discogsRequestDelay)
		if discogsNextAllowedAt.After(next) {
			discogsNextAllowedAt = next
		}
		return
	}

	discogsRequestDelay = discogsFailureRequestInterval
	discogsNextAllowedAt = time.Now().Add(discogsRequestDelay)
}

type Keeper interface {
	RegisterVinylUnique(args RegisterVinylParams) (vinyl.VinylRecord, error)

	KeepRecord(vinylID, userID int64) error // makes an entry for the record, returns an error if exists already
	PlayRecord(vinylID, userID int64) error // ++ to the numPlays of the vinylID, saves the record if not already logged
	NumPlays(vinylID, userID int64) int     // Number of plays this vinylID has had for this user
	AllVinyl() []vinyl.VinylRecord
	MyVinyl(userID int64) []vinyl.VinylWithPlayData // returns all vinyl user has played, ordered by last_played DESC
	DeleteVinyl(vinylID int64) error                // removes vinyl from DB and in-memory caches
}

type RegisterVinylParams struct {
	VinylTitle     string
	VinylArtist    string
	MasterID       *int64
	Styles         *string
	Genres         *string
	ReleaseID      int64
	RecentPrice    *float64
	Country        *string
	Notes          *string
	Released       string
	MasterRelease  int64
	ResourceURI    string
	ImageExtension string
	CoverRawBlob   []byte
	CoverEmbedding []byte
}

type keeper struct {
	ctx             context.Context
	db              *sql.DB
	queries         *vinyl.Queries
	vinylLookup     map[int64]vinyl.VinylRecord
	embeddingLookup map[int64]Embedding
	// number of plays per user per vinyl: userID -> (vinylID -> playCount)
	userNumPlays map[int64]map[int64]int

	// Filter index and cached sorted slices
	vinylIndex   *vinyl.VinylIndex
	needsRebuild bool

	mu sync.RWMutex
}

func NewKeeper() (Keeper, error) {
	k := &keeper{}
	err := k.initKeeper(context.Background())
	return k, err
}

func (k *keeper) AllVinyl() []vinyl.VinylRecord {
	v, err := k.queries.GetAllVinylRecords(k.ctx)
	if err != nil {
		log.Printf("[Keeper] failed to fetch all vinyl: %v", err)
		return []vinyl.VinylRecord{}
	}

	return mapVinylRecords(v)
}

func (k *keeper) MyVinyl(userID int64) []vinyl.VinylWithPlayData {
	userPlays, err := k.queries.GetUserVinylPlays(k.ctx, userID)
	if err != nil {
		log.Printf("[Keeper] failed to fetch user vinyl plays for user %d: %v", userID, err)
		return []vinyl.VinylWithPlayData{}
	}

	k.mu.RLock()
	defer k.mu.RUnlock()

	result := make([]vinyl.VinylWithPlayData, 0, len(userPlays))
	for _, play := range userPlays {
		vinylUnique, ok := k.vinylLookup[play.VinylID]
		if !ok {
			log.Printf("[Keeper] data consistency warning: vinyl_id %d found in vinyl_plays but not in vinylLookup", play.VinylID)
			continue
		}
		firstPlayed := stringPtrIfNonEmpty(play.FirstPlayed)
		lastPlayed := stringPtrIfNonEmpty(play.LastPlayed)
		result = append(result, vinyl.VinylWithPlayData{
			VinylRecord: vinylUnique,
			Plays:       play.Plays,
			FirstPlayed: firstPlayed,
			LastPlayed:  lastPlayed,
		})
	}

	return result
}

func (k *keeper) ListUsers() ([]vinyl.User, error) {
	users, err := k.queries.ListUsers(k.ctx)
	if err != nil {
		return nil, err
	}
	return users, nil
}

func (k *keeper) CreateUser(name string) (vinyl.User, error) {
	return k.queries.CreateUser(k.ctx, strings.TrimSpace(name))
}

func (k *keeper) GetUserByID(userID int64) (*vinyl.User, error) {
	user, err := k.queries.GetUserByID(k.ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

func (k *keeper) RegisterVinylUnique(args RegisterVinylParams) (vinyl.VinylRecord, error) {
	unique, err := k.queries.RegisterVinylUnique(k.ctx, vinyl.RegisterVinylUniqueParams{
		VinylTitle:  args.VinylTitle,
		VinylArtist: args.VinylArtist,
		MasterID:    args.MasterID,
		Styles:      args.Styles,
		Genres:      args.Genres,
	})
	if err != nil {
		return vinyl.VinylRecord{}, err
	}

	_, err = k.queries.UpsertVinylRelease(k.ctx, vinyl.UpsertVinylReleaseParams{
		VinylID:          unique.VinylID,
		ReleaseID:        args.ReleaseID,
		LowestPrice:      args.RecentPrice,
		PriceLastUpdated: ptrDateToday(),
		Country:          args.Country,
		Notes:            args.Notes,
		Released:         args.Released,
		MasterRelease:    args.MasterRelease,
		ResourceUri:      args.ResourceURI,
		ImageExtension:   args.ImageExtension,
		CoverRawBlob:     args.CoverRawBlob,
		CoverEmbedding:   args.CoverEmbedding,
	})
	if err != nil {
		return vinyl.VinylRecord{}, err
	}

	row, err := k.queries.GetVinylRecordByID(k.ctx, unique.VinylID)
	if err != nil {
		return vinyl.VinylRecord{}, err
	}
	vinylRecord := mapVinylRecord(row)

	// Decode embedding from the returned vinyl record
	emb, err := EmbeddingFromBlob(vinylRecord.CoverEmbedding)
	if err != nil {
		return vinyl.VinylRecord{}, fmt.Errorf("failed to decode embedding for vinyl %d: %w", vinylRecord.VinylID, err)
	}

	// Update in-memory caches with proper locking
	k.mu.Lock()
	k.vinylLookup[vinylRecord.VinylID] = vinylRecord
	k.embeddingLookup[vinylRecord.VinylID] = emb
	k.needsRebuild = true // Mark index for rebuild
	k.mu.Unlock()

	return vinylRecord, nil
}

// needs to impl finding the extension. this abstraction helps with making the consumer API have optional ways to get images
type keeperRegisterVinylParams interface {
	GenerateEmbedding() Embedding
}

// This will not be the final fields for this bc we will need to have this work for discogs eventually
//type KeeperRegisterUniqueVinylParams struct {
//	AlbumTitle, Artist string
//}

// TODO: a smart way to have input for an image.
// in the final app this is simply done from scanning the phone input, so we will just want []byte most likely
// we also want to have the Actual ALBUM cover. this comes from discogs most likely in the final version?
// so we want an image url/image source. I think that pasting a image url from anywhere works. Then it saves the raw image into
// sqlite under a Blob and we can note the image extension pretty easily by parsing the url/source file}

type discogsResp struct {
	masterID      int
	releaseID     int
	title, artist string
	rawCoverData  []byte
	extension     string
	releaseYear   int
	genres        string
	styles        string
}

type discogsSearchResp struct {
	Results []struct {
		Type     string `json:"type"`
		MasterID int    `json:"master_id"`
	} `json:"results"`
}

type discogsMasterResp struct {
	Title       string   `json:"title"`
	Year        int      `json:"year"`
	MainRelease int      `json:"main_release"`
	Genres      []string `json:"genres"`
	Styles      []string `json:"styles"`
	Artists     []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Images []struct {
		Type string `json:"type"`
		URI  string `json:"uri"`
	} `json:"images"`
}

// this gets the first master release from the input string
// will only really work for an album and not 45s, live albums (without saying very specific terms)
// eventually we will need multiple []discogsResp of the releases
// we can then maybe determine pressing, etc off that? Not there yet regardless
func requestDiscogs(albumTitle, artist string) (discogsResp, error) {
	albumTitle = strings.TrimSpace(albumTitle)
	artist = strings.TrimSpace(artist)
	if albumTitle == "" || artist == "" {
		return discogsResp{}, fmt.Errorf("must provide strings for both albumTitle and artist")
	}

	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	format := "&format=album"
	// Step 1: Search for the release
	searchURL := fmt.Sprintf("https://api.discogs.com/database/search?release_title=%s&artist=%s%s&per_page=1&page=1",
		strings.ReplaceAll(albumTitle, " ", "%20"),
		strings.ReplaceAll(artist, " ", "%20"),
		format)

	log.Printf("[Discogs] GET %s", searchURL)
	searchResp, err := httpClient.Get(searchURL)
	if err != nil {
		return discogsResp{}, fmt.Errorf("search request failed: %w", err)
	}
	defer searchResp.Body.Close()

	if searchResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(searchResp.Body)
		return discogsResp{}, fmt.Errorf("search returned status %d: %s", searchResp.StatusCode, string(body))
	}

	var searchResult discogsSearchResp
	if err := json.NewDecoder(searchResp.Body).Decode(&searchResult); err != nil {
		return discogsResp{}, fmt.Errorf("failed to decode search response: %w", err)
	}

	if len(searchResult.Results) == 0 {
		return discogsResp{}, fmt.Errorf("no results found for album '%s' by artist '%s'", albumTitle, artist)
	}

	masterIDs := make([]int, 0, len(searchResult.Results))
	seenMasterID := make(map[int]struct{}, len(searchResult.Results))
	appendMasterID := func(masterID int) {
		if masterID == 0 {
			return
		}
		if _, exists := seenMasterID[masterID]; exists {
			return
		}
		seenMasterID[masterID] = struct{}{}
		masterIDs = append(masterIDs, masterID)
	}

	// Prefer explicit master results, then any result carrying a master_id.
	for _, result := range searchResult.Results {
		if result.Type == "master" {
			appendMasterID(result.MasterID)
		}
	}
	for _, result := range searchResult.Results {
		appendMasterID(result.MasterID)
	}

	if len(masterIDs) == 0 {
		return discogsResp{}, fmt.Errorf("no master_id found in search results for album '%s' by artist '%s'", albumTitle, artist)
	}

	var lastErr error
	for _, masterID := range masterIDs {
		resp, err := requestMasterDiscogs(masterID)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return discogsResp{}, fmt.Errorf("failed to resolve usable discogs master from search results: %w", lastErr)
	}

	return discogsResp{}, fmt.Errorf("failed to resolve usable discogs master from search results")
}

func requestMasterDiscogs(masterID int) (discogsResp, error) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}
	// Step 2: Get master release details
	masterURL := fmt.Sprintf("https://api.discogs.com/masters/%d", masterID)
	log.Printf("[Discogs] GET %s", masterURL)
	masterResp, err := httpClient.Get(masterURL)
	if err != nil {
		return discogsResp{}, fmt.Errorf("master request failed: %w", err)
	}
	defer masterResp.Body.Close()

	if masterResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(masterResp.Body)
		return discogsResp{}, fmt.Errorf("master request returned status %d: %s", masterResp.StatusCode, string(body))
	}

	var masterResult discogsMasterResp
	if err := json.NewDecoder(masterResp.Body).Decode(&masterResult); err != nil {
		return discogsResp{}, fmt.Errorf("failed to decode master response: %w", err)
	}

	// Build comma-separated artist string (no spaces)
	var artistNames []string
	for _, a := range masterResult.Artists {
		artistNames = append(artistNames, a.Name)
	}
	artistStr := strings.Join(artistNames, ",")

	// Build comma-separated genres and styles strings (no spaces)
	genresStr := strings.Join(masterResult.Genres, ",")
	stylesStr := strings.Join(masterResult.Styles, ",")

	// Find primary image URI, then fall back to the first available image.
	var imageURI string
	for i := 0; i < len(masterResult.Images); i++ {
		if masterResult.Images[i].Type == "primary" {
			imageURI = masterResult.Images[i].URI
			break
		}
	}
	if imageURI == "" && len(masterResult.Images) > 0 {
		imageURI = masterResult.Images[0].URI
	}

	if imageURI == "" {
		return discogsResp{}, fmt.Errorf("no image found for master %d", masterID)
	}

	// Step 3: Download the image
	log.Printf("[Discogs] GET %s", imageURI)
	waitForDiscogsRequestSlot()
	imageResp, err := httpClient.Get(imageURI)
	if err != nil {
		recordDiscogsRequestResult(false)
		return discogsResp{}, fmt.Errorf("image download failed: %w", err)
	}
	defer imageResp.Body.Close()

	if imageResp.StatusCode != http.StatusOK {
		recordDiscogsRequestResult(false)
		return discogsResp{}, fmt.Errorf("image download returned status %d", imageResp.StatusCode)
	}
	recordDiscogsRequestResult(true)

	rawImageData, err := io.ReadAll(imageResp.Body)
	if err != nil {
		return discogsResp{}, fmt.Errorf("failed to read image data: %w", err)
	}

	// Extract extension from URI
	lastDot := strings.LastIndex(imageURI, ".")
	extension := ""
	if lastDot != -1 {
		extension = imageURI[lastDot+1:]
	}

	return discogsResp{
		masterID:     masterID,
		releaseID:    masterResult.MainRelease,
		title:        masterResult.Title,
		artist:       artistStr,
		rawCoverData: rawImageData,
		extension:    extension,
		releaseYear:  masterResult.Year,
		genres:       genresStr,
		styles:       stylesStr,
	}, nil
}

func RegisterUniqueVinylMasterID(masterID int) (RegisterVinylParams, error) {
	resp, err := requestMasterDiscogs(masterID)
	if err != nil {
		return RegisterVinylParams{}, err
	}
	return registerParams(resp)
}

func RegisterUniqueVinylAlbumArtist(albumTitle, artist string) (RegisterVinylParams, error) {
	// get raw image data []byte and string image extension .png, .jpg etc
	resp, err := requestDiscogs(albumTitle, artist)
	if err != nil {
		return RegisterVinylParams{}, err
	}
	return registerParams(resp)
}

func registerParams(resp discogsResp) (RegisterVinylParams, error) {
	emb, err := RequestEmbedding(resp.rawCoverData)
	if err != nil {
		return RegisterVinylParams{}, fmt.Errorf("failed to generate embedding: %w", err)
	}

	// Convert to pointers for nullable fields
	masterID := int64(resp.masterID)
	var stylesPtr, genresPtr *string
	if resp.styles != "" {
		stylesPtr = &resp.styles
	}
	if resp.genres != "" {
		genresPtr = &resp.genres
	}

	releaseID := int64(resp.releaseID)
	if releaseID == 0 {
		releaseID = int64(resp.masterID)
	}

	return RegisterVinylParams{
		VinylTitle:     resp.title,
		VinylArtist:    resp.artist,
		MasterID:       &masterID,
		Styles:         stylesPtr,
		Genres:         genresPtr,
		ReleaseID:      releaseID,
		Released:       strconv.Itoa(resp.releaseYear),
		MasterRelease:  1,
		ResourceURI:    fmt.Sprintf("https://api.discogs.com/masters/%d", masterID),
		ImageExtension: resp.extension,
		CoverRawBlob:   resp.rawCoverData,
		CoverEmbedding: EmbeddingToBlob(emb),
	}, nil
}

func (k *keeper) KeepRecord(vinylID, userID int64) error {
	if !k.checkIfExists(vinylID) {
		return fmt.Errorf("%d does not exist in keeper as a UniqueVinyl", vinylID)
	}
	releaseID, err := k.queries.GetPrimaryReleaseID(k.ctx, vinylID)
	if err != nil {
		return fmt.Errorf("failed to resolve release for vinyl %d: %w", vinylID, err)
	}
	err = k.queries.EnsureOwnershipPlay(k.ctx, vinyl.EnsureOwnershipPlayParams{
		UserID:     userID,
		VinylID:    vinylID,
		ReleaseID:  releaseID,
		PlayedDate: time.Now().Format("2006-01-02"),
	})
	if err != nil {
		return fmt.Errorf("failed to record ownership for vinyl %d user %d: %w", vinylID, userID, err)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.userNumPlays[userID] == nil {
		k.userNumPlays[userID] = make(map[int64]int)
	}
	if _, ok := k.userNumPlays[userID][vinylID]; !ok {
		k.userNumPlays[userID][vinylID] = 0
	}
	return nil
}

// PlayRecord makes an entry given the VinylID to the user's collection
func (k *keeper) PlayRecord(vinylID, userID int64) error {
	if !k.checkIfExists(vinylID) {
		return fmt.Errorf("%d does not exist in keeper as a UniqueVinyl", vinylID)
	}
	releaseID, err := k.queries.GetPrimaryReleaseID(k.ctx, vinylID)
	if err != nil {
		return fmt.Errorf("failed to resolve release for vinyl %d: %w", vinylID, err)
	}

	if err := k.queries.EnsureOwnershipPlay(k.ctx, vinyl.EnsureOwnershipPlayParams{
		UserID:     userID,
		VinylID:    vinylID,
		ReleaseID:  releaseID,
		PlayedDate: time.Now().Format("2006-01-02"),
	}); err != nil {
		return fmt.Errorf("failed to ensure ownership play for vinyl %d: %w", vinylID, err)
	}

	nextPlay, err := k.queries.NextPlayNumber(k.ctx, vinyl.NextPlayNumberParams{
		UserID:    userID,
		VinylID:   vinylID,
		ReleaseID: releaseID,
	})
	if err != nil {
		return fmt.Errorf("failed to resolve next play number for vinyl %d: %w", vinylID, err)
	}

	if err := k.queries.InsertVinylPlay(k.ctx, vinyl.InsertVinylPlayParams{
		UserID:     userID,
		VinylID:    vinylID,
		ReleaseID:  releaseID,
		Play:       nextPlay,
		PlayedDate: time.Now().Format("2006-01-02"),
	}); err != nil {
		return fmt.Errorf("failed to record play for vinyl %d: %w", vinylID, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.userNumPlays[userID] == nil {
		k.userNumPlays[userID] = make(map[int64]int)
	}
	k.userNumPlays[userID][vinylID] = int(nextPlay)
	return nil
}

func (k *keeper) NumPlays(vinylID, userID int64) int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.userNumPlays[userID] == nil {
		return 0
	}
	return k.userNumPlays[userID][vinylID]
}

func (k *keeper) DeleteVinyl(vinylID int64) error {
	// Delete from database
	if err := k.queries.DeleteVinyl(k.ctx, vinylID); err != nil {
		return fmt.Errorf("failed to delete vinyl %d from database: %w", vinylID, err)
	}

	// Remove from in-memory caches with proper locking
	k.mu.Lock()
	delete(k.vinylLookup, vinylID)
	delete(k.embeddingLookup, vinylID)
	// Remove from all users' play counts
	for userID := range k.userNumPlays {
		delete(k.userNumPlays[userID], vinylID)
	}
	k.needsRebuild = true // Mark index for rebuild
	k.mu.Unlock()

	return nil
}

func (k *keeper) DeleteUser(userID int64) error {
	if err := k.queries.DeleteUser(k.ctx, userID); err != nil {
		return fmt.Errorf("failed to delete user %d from database: %w", userID, err)
	}

	k.mu.Lock()
	delete(k.userNumPlays, userID)
	k.mu.Unlock()

	return nil
}

func (k *keeper) initKeeper(ctx context.Context) error {
	k.ctx = ctx
	// Initialize DB and queries
	if err := k.initializeQueries(ctx); err != nil {
		return err
	}

	// Load all vinyls
	vinyls, err := k.queries.GetAllVinylRecords(k.ctx)
	if err != nil {
		return err
	}
	k.vinylLookup = make(map[int64]vinyl.VinylRecord)
	k.embeddingLookup = make(map[int64]Embedding)
	for _, row := range vinyls {
		v := mapVinylRecord(row)
		k.vinylLookup[v.VinylID] = v
		// Decode embedding from blob
		emb, err := EmbeddingFromBlob(v.CoverEmbedding)
		if err != nil {
			return fmt.Errorf("failed to decode embedding for vinyl %d: %w", v.VinylID, err)
		}
		k.embeddingLookup[v.VinylID] = emb
	}

	// Load ALL user plays from all users
	allUserPlays, err := k.queries.GetAllUserVinylPlays(k.ctx)
	if err != nil {
		return err
	}
	k.userNumPlays = make(map[int64]map[int64]int)
	for _, play := range allUserPlays {
		if k.userNumPlays[play.UserID] == nil {
			k.userNumPlays[play.UserID] = make(map[int64]int)
		}
		k.userNumPlays[play.UserID][play.VinylID] = int(play.Plays)
	}

	// Build initial vinyl index
	k.rebuildIndex()

	return nil
}

const dbFileName = "vinylkeeper.db"

func sqlitePoolSetting(envKey string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return 0, fmt.Errorf("%s is required", envKey)
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", envKey, err)
	}
	if value < 1 {
		return 0, fmt.Errorf("%s must be >= 1 (got %d)", envKey, value)
	}

	return value, nil
}

func databasePath() (string, error) {
	if path := os.Getenv("DB_PATH"); path != "" {
		return path, nil
	}

	return "", fmt.Errorf("DB_PATH is required; expected canonical database path (for example /data/%s in containers or ./data/%s locally)", dbFileName, dbFileName)
}

func mapVinylRecords(rows []vinyl.GetAllVinylRecordsRow) []vinyl.VinylRecord {
	items := make([]vinyl.VinylRecord, 0, len(rows))
	for _, row := range rows {
		items = append(items, mapVinylRecord(row))
	}
	return items
}

func mapVinylRecord(row interface{}) vinyl.VinylRecord {
	switch r := row.(type) {
	case vinyl.GetAllVinylRecordsRow:
		return vinyl.VinylRecord{
			VinylID:           r.VinylID,
			VinylTitle:        r.VinylTitle,
			VinylArtist:       r.VinylArtist,
			VinylPressingYear: r.VinylPressingYear,
			MasterID:          r.MasterID,
			Genres:            r.Genres,
			Styles:            r.Styles,
			Country:           r.Country,
			Released:          r.Released,
			RecentPrice:       r.RecentPrice,
			ImageExtension:    r.ImageExtension,
			CoverRawBlob:      r.CoverRawBlob,
			CoverEmbedding:    r.CoverEmbedding,
		}
	case vinyl.GetVinylRecordByIDRow:
		return vinyl.VinylRecord{
			VinylID:           r.VinylID,
			VinylTitle:        r.VinylTitle,
			VinylArtist:       r.VinylArtist,
			VinylPressingYear: r.VinylPressingYear,
			MasterID:          r.MasterID,
			Genres:            r.Genres,
			Styles:            r.Styles,
			Country:           r.Country,
			Released:          r.Released,
			RecentPrice:       r.RecentPrice,
			ImageExtension:    r.ImageExtension,
			CoverRawBlob:      r.CoverRawBlob,
			CoverEmbedding:    r.CoverEmbedding,
		}
	default:
		return vinyl.VinylRecord{}
	}
}

func stringPtrIfNonEmpty(v string) *string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func ptrDateToday() *string {
	v := time.Now().Format("2006-01-02")
	return &v
}

func columnExists(ctx context.Context, db *sql.DB, tableName, columnName string) (bool, error) {
	query := fmt.Sprintf("SELECT 1 FROM pragma_table_info('%s') WHERE name = ? LIMIT 1", tableName)
	var one int
	err := db.QueryRowContext(ctx, query, columnName).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func ensureSchemaCompatibility(ctx context.Context, db *sql.DB) error {
	hasMasterID, err := columnExists(ctx, db, "vinyl_unique", "master_id")
	if err != nil {
		return fmt.Errorf("check vinyl_unique.master_id: %w", err)
	}
	if !hasMasterID {
		if _, err := db.ExecContext(ctx, "ALTER TABLE vinyl_unique ADD COLUMN master_id INTEGER"); err != nil {
			return fmt.Errorf("add vinyl_unique.master_id: %w", err)
		}
		hasMasterID = true
	}

	hasLegacyMasterID, err := columnExists(ctx, db, "vinyl_unique", "discogs_master_id")
	if err != nil {
		return fmt.Errorf("check vinyl_unique.discogs_master_id: %w", err)
	}
	if hasMasterID && hasLegacyMasterID {
		if _, err := db.ExecContext(ctx, `
			UPDATE vinyl_unique
			SET master_id = COALESCE(master_id, discogs_master_id)
			WHERE master_id IS NULL
			  AND discogs_master_id IS NOT NULL
		`); err != nil {
			return fmt.Errorf("backfill vinyl_unique.master_id: %w", err)
		}
	}

	return nil
}

// initializeQueries creates or loads the DB and assigns to k.queries
func (k *keeper) initializeQueries(ctx context.Context) error {
	dbPath, err := databasePath()
	if err != nil {
		return err
	}

	maxOpenSQLite, err := sqlitePoolSetting("MAX_OPEN_SQLITE")
	if err != nil {
		return err
	}
	maxIdleSQLite, err := sqlitePoolSetting("MAX_IDLE_SQLITE")
	if err != nil {
		return err
	}
	if maxIdleSQLite > maxOpenSQLite {
		return fmt.Errorf("MAX_IDLE_SQLITE (%d) cannot be greater than MAX_OPEN_SQLITE (%d)", maxIdleSQLite, maxOpenSQLite)
	}

	dir := filepath.Dir(dbPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db directory %q: %w", dir, err)
		}
	}

	openPath := dbPath + "?_pragma=foreign_keys(1)"
	if strings.Contains(dbPath, "?") {
		openPath = dbPath + "&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", openPath)
	if err != nil {
		return fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(maxOpenSQLite)
	db.SetMaxIdleConns(maxIdleSQLite)
	// Foreign key enforcement is configured via DSN pragma so it applies to each
	// new connection in the database/sql pool. Apply schema on each start so
	// existing databases pick up additive table changes.
	if _, err = db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureSchemaCompatibility(ctx, db); err != nil {
		db.Close()
		return fmt.Errorf("ensure schema compatibility: %w", err)
	}
	queries, err := vinyl.Prepare(ctx, db)
	if err != nil {
		db.Close()
		return fmt.Errorf("prepare queries: %w", err)
	}
	k.db = db
	k.queries = queries
	return nil
}
func (k *keeper) checkIfExists(vinylID int64) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, exists := k.vinylLookup[vinylID]
	if exists {
		return true
	}
	return false
}

func (k *keeper) GetVinyl(vinylID int64) *vinyl.VinylRecord {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, exists := k.vinylLookup[vinylID]
	if !exists {
		return nil
	}
	return &v
}

func cosineSimilarity(a, b Embedding) float64 {
	if len(a) != len(b) {
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

// FindClosestVinyl finds the vinyl that is closest to the input embedding
// input embedding is usually going to be from the user's image
func (k *keeper) FindClosestVinyl(input Embedding) vinyl.VinylRecord {
	vinyls := k.FindClosestVinyls(input, 1)
	if len(vinyls) == 0 {
		return vinyl.VinylRecord{}
	}
	return vinyls[0]
}

func (k *keeper) FindClosestVinyls(input Embedding, n int) []vinyl.VinylRecord {
	if n <= 0 {
		return []vinyl.VinylRecord{}
	}

	type scoredVinyl struct {
		vinylID    int64
		similarity float64
	}

	k.mu.RLock()
	defer k.mu.RUnlock()

	scored := make([]scoredVinyl, 0, len(k.embeddingLookup))
	for vID, embedding := range k.embeddingLookup {
		scored = append(scored, scoredVinyl{
			vinylID:    vID,
			similarity: cosineSimilarity(input, embedding),
		})
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].similarity == scored[j].similarity {
			return scored[i].vinylID < scored[j].vinylID
		}
		return scored[i].similarity > scored[j].similarity
	})

	if n > len(scored) {
		n = len(scored)
	}

	result := make([]vinyl.VinylRecord, 0, n)
	for i := 0; i < n; i++ {
		result = append(result, k.vinylLookup[scored[i].vinylID])
	}

	return result
}

// GetVinylIndex returns the vinyl index, rebuilding it if necessary
func (k *keeper) GetVinylIndex() *vinyl.VinylIndex {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.needsRebuild {
		k.rebuildIndexLocked()
	}

	return k.vinylIndex
}

// rebuildIndex rebuilds the vinyl index with proper locking
func (k *keeper) rebuildIndex() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rebuildIndexLocked()
}

// rebuildIndexLocked rebuilds the index (must be called with lock held)
func (k *keeper) rebuildIndexLocked() {
	vinyls := make([]vinyl.VinylRecord, 0, len(k.vinylLookup))
	for _, v := range k.vinylLookup {
		vinyls = append(vinyls, v)
	}
	k.vinylIndex = vinyl.BuildVinylIndex(vinyls)
	k.needsRebuild = false
}
