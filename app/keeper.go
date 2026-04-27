package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

type Keeper interface {
	RegisterVinylUnique(args vinyl.RegisterVinylParams) (vinyl.VinylUnique, error) // FIXME: this is not discogs? uncertain schema for this yet

	KeepRecord(vinylID, userID int64) error // makes an entry for the record, returns an error if exists already
	PlayRecord(vinylID, userID int64) error // ++ to the numPlays of the vinylID, saves the record if not already logged
	NumPlays(vinylID, userID int64) int     // Number of plays this vinylID has had for this user
	AllVinyl() []vinyl.VinylUnique
	MyVinyl(userID int64) []vinyl.VinylWithPlayData // returns all vinyl user has played, ordered by last_played DESC
	DeleteVinyl(vinylID int64) error                // removes vinyl from DB and in-memory caches
}

type keeper struct {
	ctx             context.Context
	queries         *vinyl.Queries
	vinylLookup     map[int64]vinyl.VinylUnique
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

func (k *keeper) AllVinyl() []vinyl.VinylUnique {
	v, err := k.queries.GetAllVinyls(k.ctx)
	if err != nil {
		log.Printf("[Keeper] failed to fetch all vinyl: %v", err)
		return []vinyl.VinylUnique{}
	}

	return v
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
			log.Printf("[Keeper] data consistency warning: vinyl_id %d found in user_vinyl_plays but not in vinylLookup", play.VinylID)
			continue
		}
		result = append(result, vinyl.VinylWithPlayData{
			VinylUnique: vinylUnique,
			Plays:       play.Plays,
			FirstPlayed: play.FirstPlayed,
			LastPlayed:  play.LastPlayed,
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

func (k *keeper) RegisterVinylUnique(args vinyl.RegisterVinylParams) (vinyl.VinylUnique, error) {
	vinylUnique, err := k.queries.RegisterVinyl(k.ctx, args)
	if err != nil {
		return vinyl.VinylUnique{}, err
	}

	// Decode embedding from the returned vinyl record
	emb, err := EmbeddingFromBlob(vinylUnique.CoverEmbedding)
	if err != nil {
		return vinyl.VinylUnique{}, fmt.Errorf("failed to decode embedding for vinyl %d: %w", vinylUnique.VinylID, err)
	}

	// Update in-memory caches with proper locking
	k.mu.Lock()
	k.vinylLookup[vinylUnique.VinylID] = vinylUnique
	k.embeddingLookup[vinylUnique.VinylID] = emb
	k.needsRebuild = true // Mark index for rebuild
	k.mu.Unlock()

	return vinylUnique, nil
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
	title, artist string
	rawCoverData  []byte
	extension     string
	releaseYear   int
	genres        string
	styles        string
}

type discogsSearchResp struct {
	Results []struct {
		MasterID int `json:"master_id"`
	} `json:"results"`
}

type discogsMasterResp struct {
	Title   string   `json:"title"`
	Year    int      `json:"year"`
	Genres  []string `json:"genres"`
	Styles  []string `json:"styles"`
	Artists []struct {
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

	log.Printf("Discogs search: %s", searchURL)
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

	masterID := searchResult.Results[0].MasterID
	if masterID == 0 {
		return discogsResp{}, fmt.Errorf("no master_id found in search results")
	}

	return requestMasterDiscogs(masterID)
}

func requestMasterDiscogs(masterID int) (discogsResp, error) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}
	// Step 2: Get master release details
	masterURL := fmt.Sprintf("https://api.discogs.com/masters/%d", masterID)
	log.Printf("Discogs master: %s", masterURL)
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

	// Find primary image URI
	var imageURI string
	for i := 0; i < len(masterResult.Images); i++ {
		if masterResult.Images[i].Type == "primary" {
			imageURI = masterResult.Images[i].URI
			break
		}
	}

	if imageURI == "" {
		return discogsResp{}, fmt.Errorf("no primary image found for master %d", masterID)
	}

	// Step 3: Download the image
	log.Printf("Discogs image: %s", imageURI)
	imageResp, err := httpClient.Get(imageURI)
	if err != nil {
		return discogsResp{}, fmt.Errorf("image download failed: %w", err)
	}
	defer imageResp.Body.Close()

	if imageResp.StatusCode != http.StatusOK {
		return discogsResp{}, fmt.Errorf("image download returned status %d", imageResp.StatusCode)
	}

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
		title:        masterResult.Title,
		artist:       artistStr,
		rawCoverData: rawImageData,
		extension:    extension,
		releaseYear:  masterResult.Year,
		genres:       genresStr,
		styles:       stylesStr,
	}, nil
}

func RegisterUniqueVinylMasterID(masterID int) (vinyl.RegisterVinylParams, error) {
	resp, err := requestMasterDiscogs(masterID)
	if err != nil {
		return vinyl.RegisterVinylParams{}, err
	}
	return registerParams(resp)
}

func RegisterUniqueVinylAlbumArtist(albumTitle, artist string) (vinyl.RegisterVinylParams, error) {
	// get raw image data []byte and string image extension .png, .jpg etc
	resp, err := requestDiscogs(albumTitle, artist)
	if err != nil {
		return vinyl.RegisterVinylParams{}, err
	}
	return registerParams(resp)
}

func registerParams(resp discogsResp) (vinyl.RegisterVinylParams, error) {
	emb, err := RequestEmbedding(resp.rawCoverData)
	if err != nil {
		return vinyl.RegisterVinylParams{}, fmt.Errorf("failed to generate embedding: %w", err)
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

	return vinyl.RegisterVinylParams{
		VinylTitle:        resp.title,
		VinylArtist:       resp.artist,
		VinylPressingYear: int64(resp.releaseYear), // default to initial pressing
		FirstPressing:     1,                       // default to true for now for 'master' release this will be tracked via discogs in the future
		DiscogsMasterID:   &masterID,
		Styles:            stylesPtr,
		Genres:            genresPtr,
		CoverRawBlob:      resp.rawCoverData,
		CoverEmbedding:    EmbeddingToBlob(emb),
		ImageExtension:    resp.extension,
	}, nil
}

func (k *keeper) KeepRecord(vinylID, userID int64) error {
	if !k.checkIfExists(vinylID) {
		return fmt.Errorf("%d does not exist in keeper as a UniqueVinyl", vinylID)
	}
	result, err := k.queries.RecordVinylCollection(k.ctx, vinyl.RecordVinylCollectionParams{
		UserID:  userID,
		VinylID: vinylID,
	})
	if err != nil {
		return fmt.Errorf("failed to record vinyl %d for user %d: %w", vinylID, userID, err)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.userNumPlays[userID] == nil {
		k.userNumPlays[userID] = make(map[int64]int)
	}
	k.userNumPlays[userID][vinylID] = int(result.Plays)
	return nil
}

// PlayRecord makes an entry given the VinylID to the user's collection
func (k *keeper) PlayRecord(vinylID, userID int64) error {
	if !k.checkIfExists(vinylID) {
		return fmt.Errorf("%d does not exist in keeper as a UniqueVinyl", vinylID)
	}
	play, err := k.queries.PlayVinylCollection(k.ctx, vinyl.PlayVinylCollectionParams{
		VinylID: vinylID, UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("failed to record play for vinyl %d: %w", vinylID, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.userNumPlays[userID] == nil {
		k.userNumPlays[userID] = make(map[int64]int)
	}
	k.userNumPlays[userID][vinylID] = int(play.Plays)
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
	vinyls, err := k.queries.GetAllVinyls(k.ctx)
	if err != nil {
		return err
	}
	k.vinylLookup = make(map[int64]vinyl.VinylUnique)
	k.embeddingLookup = make(map[int64]Embedding)
	for _, v := range vinyls {
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

	_, err = os.Stat(dbPath)
	isNew := errors.Is(err, fs.ErrNotExist)
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
	// new connection in the database/sql pool.
	if isNew {
		if _, err = db.ExecContext(ctx, schemaSQL); err != nil {
			db.Close()
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	queries, err := vinyl.Prepare(ctx, db)
	if err != nil {
		db.Close()
		return fmt.Errorf("prepare queries: %w", err)
	}
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

func (k *keeper) GetVinyl(vinylID int64) *vinyl.VinylUnique {
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
func (k *keeper) FindClosestVinyl(input Embedding) vinyl.VinylUnique {
	vinyls := k.FindClosestVinyls(input, 1)
	if len(vinyls) == 0 {
		return vinyl.VinylUnique{}
	}
	return vinyls[0]
}

func (k *keeper) FindClosestVinyls(input Embedding, n int) []vinyl.VinylUnique {
	if n <= 0 {
		return []vinyl.VinylUnique{}
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

	result := make([]vinyl.VinylUnique, 0, n)
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
	vinyls := make([]vinyl.VinylUnique, 0, len(k.vinylLookup))
	for _, v := range k.vinylLookup {
		vinyls = append(vinyls, v)
	}
	k.vinylIndex = vinyl.BuildVinylIndex(vinyls)
	k.needsRebuild = false
}
