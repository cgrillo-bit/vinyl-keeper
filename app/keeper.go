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

type releaseScanRow struct {
	VinylID           int64
	VinylTitle        string
	VinylArtist       string
	MasterID          sql.NullInt64
	Styles            sql.NullString
	Genres            sql.NullString
	VinylPressingYear sql.NullInt64
	Country           sql.NullString
	Released          string
	RecentPrice       sql.NullFloat64
	ImageExtension    string
	CoverEmbedding    []byte
	ReleaseID         int64
	Label             sql.NullString
	ReleaseFormat     sql.NullString
	ReleaseCountry    sql.NullString
	CoverURI          sql.NullString
	HasBlob           int64
	Notes             sql.NullString
}

type myVinylRow struct {
	VinylID           int64
	Plays             int64
	FirstPlayed       string
	LastPlayed        string
	ReleaseID         int64
	VinylPressingYear sql.NullInt64
	ReleaseCountry    sql.NullString
	Released          sql.NullString
	ImageExtension    sql.NullString
	RecentPrice       sql.NullFloat64
	Label             sql.NullString
	ReleaseFormat     sql.NullString
	CoverURI          sql.NullString
	HasBlob           int64
	Notes             sql.NullString
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
	rows, err := k.db.QueryContext(k.ctx, `
		WITH plays AS (
			SELECT
				vp.user_id,
				vp.vinyl_id,
				CAST(SUM(CASE WHEN vp.play >= 1 THEN 1 ELSE 0 END) AS INTEGER) AS plays,
				CAST(MIN(vp.played_date) AS TEXT) AS first_played,
				CAST(MAX(vp.played_date) AS TEXT) AS last_played,
				(
					SELECT vp2.release_id
					FROM vinyl_plays vp2
					WHERE vp2.user_id = vp.user_id
					  AND vp2.vinyl_id = vp.vinyl_id
					ORDER BY vp2.played_date DESC, vp2.play DESC
					LIMIT 1
				) AS release_id
			FROM vinyl_plays vp
			WHERE vp.user_id = ?
			GROUP BY vp.user_id, vp.vinyl_id
		)
		SELECT
			p.vinyl_id,
			p.plays,
			p.first_played,
			p.last_played,
			p.release_id,
			COALESCE(vrc.released_year, CAST(substr(vr.released, 1, 4) AS INTEGER)) AS vinyl_pressing_year,
			COALESCE(NULLIF(vrc.country, ''), vr.country) AS release_country,
			vr.released,
			vr.image_extension,
			vr.lowest_price,
			vrc.label,
			vrc.release_format,
			vrc.cover_uri,
			CASE WHEN length(vr.cover_raw_blob) > 0 THEN 1 ELSE 0 END AS has_blob,
			vr.notes
		FROM plays p
		JOIN vinyl_release vr
		  ON vr.vinyl_id = p.vinyl_id
		 AND vr.release_id = p.release_id
		LEFT JOIN vinyl_releases_check vrc
		  ON vrc.vinyl_id = p.vinyl_id
		 AND vrc.release_id = p.release_id
		ORDER BY p.last_played DESC
	`, userID)
	if err != nil {
		log.Printf("[Keeper] failed to fetch user vinyl rows for user %d: %v", userID, err)
		return []vinyl.VinylWithPlayData{}
	}
	defer rows.Close()

	k.mu.RLock()
	defer k.mu.RUnlock()

	result := make([]vinyl.VinylWithPlayData, 0)
	for rows.Next() {
		var row myVinylRow
		if err := rows.Scan(
			&row.VinylID,
			&row.Plays,
			&row.FirstPlayed,
			&row.LastPlayed,
			&row.ReleaseID,
			&row.VinylPressingYear,
			&row.ReleaseCountry,
			&row.Released,
			&row.ImageExtension,
			&row.RecentPrice,
			&row.Label,
			&row.ReleaseFormat,
			&row.CoverURI,
			&row.HasBlob,
			&row.Notes,
		); err != nil {
			log.Printf("[Keeper] failed scanning user vinyl row for user %d: %v", userID, err)
			continue
		}

		vinylUnique, ok := k.vinylLookup[row.VinylID]
		if !ok {
			log.Printf("[Keeper] data consistency warning: vinyl_id %d found in vinyl_plays but not in vinylLookup", row.VinylID)
			continue
		}

		if row.Released.Valid {
			vinylUnique.Released = row.Released.String
		}
		if row.ImageExtension.Valid {
			vinylUnique.ImageExtension = row.ImageExtension.String
		}
		vinylUnique.RecentPrice = nullableFloat64(row.RecentPrice)

		firstPlayed := stringPtrIfNonEmpty(row.FirstPlayed)
		lastPlayed := stringPtrIfNonEmpty(row.LastPlayed)
		result = append(result, vinyl.VinylWithPlayData{
			VinylRecord:    vinylUnique,
			Plays:          row.Plays,
			FirstPlayed:    firstPlayed,
			LastPlayed:     lastPlayed,
			ReleaseID:      row.ReleaseID,
			ReleaseFormat:  nullableString(row.ReleaseFormat),
			ReleaseCountry: nullableString(row.ReleaseCountry),
			Label:          nullableString(row.Label),
			CoverURI:       nullableString(row.CoverURI),
			HasBlob:        row.HasBlob == 1,
			Notes:          nullableString(row.Notes),
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("[Keeper] failed iterating user vinyl rows for user %d: %v", userID, err)
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
		log.Printf("[Keeper] warning: failed to decode embedding for vinyl %d from DB row: %v", vinylRecord.VinylID, err)
		emb, err = EmbeddingFromBlob(args.CoverEmbedding)
		if err != nil {
			log.Printf("[Keeper] warning: failed to decode fallback embedding for vinyl %d: %v", vinylRecord.VinylID, err)
			emb = nil
		}
	}

	// Update in-memory caches with proper locking
	k.mu.Lock()
	k.vinylLookup[vinylRecord.VinylID] = vinylRecord
	if emb != nil {
		k.embeddingLookup[vinylRecord.VinylID] = emb
	}
	k.needsRebuild = true // Mark index for rebuild
	k.mu.Unlock()

	return vinylRecord, nil
}

func (k *keeper) GetPrimaryReleaseID(vinylID int64) (int64, error) {
	return k.queries.GetPrimaryReleaseID(k.ctx, vinylID)
}

func (k *keeper) FindExistingVinyl(artist, album string, masterID *int64) *vinyl.VinylRecord {
	if masterID != nil {
		v, err := k.findExistingVinylByMasterID(*masterID)
		if err != nil {
			log.Printf("[Keeper] find existing vinyl by master_id=%d failed: %v", *masterID, err)
			return nil
		}
		if v != nil {
			return v
		}
	}

	rows := k.AllVinyl()
	if masterID != nil {
		for i := range rows {
			if rows[i].MasterID != nil && *rows[i].MasterID == *masterID {
				v := rows[i]
				return &v
			}
		}
	}

	artist = strings.TrimSpace(artist)
	album = strings.TrimSpace(album)
	if artist == "" || album == "" {
		return nil
	}

	for i := range rows {
		if strings.EqualFold(strings.TrimSpace(rows[i].VinylArtist), artist) && strings.EqualFold(strings.TrimSpace(rows[i].VinylTitle), album) {
			v := rows[i]
			return &v
		}
	}

	return nil
}

func (k *keeper) findExistingVinylByMasterID(masterID int64) (*vinyl.VinylRecord, error) {
	var vinylID int64
	err := k.db.QueryRowContext(k.ctx, `
		SELECT vu.vinyl_id
		FROM vinyl_unique vu
		JOIN vinyl_release vr ON vr.vinyl_id = vu.vinyl_id AND vr.master_release = 1
		WHERE vu.master_id = ?
		LIMIT 1
	`, masterID).Scan(&vinylID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	v := k.GetVinyl(vinylID)
	if v == nil {
		return nil, nil
	}
	return v, nil
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
func resolveDiscogsMasterIDs(albumTitle, artist string, httpClient *http.Client) ([]int, error) {
	albumTitle = strings.TrimSpace(albumTitle)
	artist = strings.TrimSpace(artist)
	if albumTitle == "" || artist == "" {
		return nil, fmt.Errorf("must provide strings for both albumTitle and artist")
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
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer searchResp.Body.Close()

	if searchResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(searchResp.Body)
		return nil, fmt.Errorf("search returned status %d: %s", searchResp.StatusCode, string(body))
	}

	var searchResult discogsSearchResp
	if err := json.NewDecoder(searchResp.Body).Decode(&searchResult); err != nil {
		return nil, fmt.Errorf("failed to decode search response: %w", err)
	}

	if len(searchResult.Results) == 0 {
		return nil, fmt.Errorf("no results found for album '%s' by artist '%s'", albumTitle, artist)
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
		return nil, fmt.Errorf("no master_id found in search results for album '%s' by artist '%s'", albumTitle, artist)
	}

	return masterIDs, nil
}

func FindDiscogsMasterID(albumTitle, artist string) (int, error) {
	httpClient := &http.Client{Timeout: time.Second * 10}
	masterIDs, err := resolveDiscogsMasterIDs(albumTitle, artist, httpClient)
	if err != nil {
		return 0, err
	}
	return masterIDs[0], nil
}

func requestDiscogs(albumTitle, artist string) (discogsResp, error) {
	httpClient := &http.Client{Timeout: time.Second * 10}
	masterIDs, err := resolveDiscogsMasterIDs(albumTitle, artist, httpClient)
	if err != nil {
		return discogsResp{}, err
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

func (k *keeper) PlayRecordRelease(vinylID, releaseID, userID int64) error {
	if !k.checkIfExists(vinylID) {
		return fmt.Errorf("%d does not exist in keeper as a UniqueVinyl", vinylID)
	}

	if err := k.queries.EnsureOwnershipPlay(k.ctx, vinyl.EnsureOwnershipPlayParams{
		UserID:     userID,
		VinylID:    vinylID,
		ReleaseID:  releaseID,
		PlayedDate: time.Now().Format("2006-01-02"),
	}); err != nil {
		return fmt.Errorf("failed to ensure ownership play for vinyl %d release %d: %w", vinylID, releaseID, err)
	}

	nextPlay, err := k.queries.NextPlayNumber(k.ctx, vinyl.NextPlayNumberParams{
		UserID:    userID,
		VinylID:   vinylID,
		ReleaseID: releaseID,
	})
	if err != nil {
		return fmt.Errorf("failed to resolve next play number for vinyl %d release %d: %w", vinylID, releaseID, err)
	}

	if err := k.queries.InsertVinylPlay(k.ctx, vinyl.InsertVinylPlayParams{
		UserID:     userID,
		VinylID:    vinylID,
		ReleaseID:  releaseID,
		Play:       nextPlay,
		PlayedDate: time.Now().Format("2006-01-02"),
	}); err != nil {
		return fmt.Errorf("failed to record play for vinyl %d release %d: %w", vinylID, releaseID, err)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.userNumPlays[userID] == nil {
		k.userNumPlays[userID] = make(map[int64]int)
	}
	k.userNumPlays[userID][vinylID] = int(nextPlay)
	return nil
}

func (k *keeper) FindClosestReleaseCandidates(input Embedding, n int, userID int64) []vinyl.ReleaseCandidate {
	if n <= 0 {
		return []vinyl.ReleaseCandidate{}
	}

	ownedPressings := map[int64]map[int64]struct{}{}
	if userID >= 0 {
		rowsOwned, err := k.db.QueryContext(k.ctx, `
			SELECT DISTINCT vinyl_id, release_id
			FROM vinyl_plays
			WHERE user_id = ?
		`, userID)
		if err != nil {
			log.Printf("[Keeper] failed to query owned pressings for user %d: %v", userID, err)
		} else {
			for rowsOwned.Next() {
				var ownedVinylID int64
				var ownedReleaseID int64
				if scanErr := rowsOwned.Scan(&ownedVinylID, &ownedReleaseID); scanErr != nil {
					log.Printf("[Keeper] failed scanning owned pressing row for user %d: %v", userID, scanErr)
					continue
				}
				if ownedPressings[ownedVinylID] == nil {
					ownedPressings[ownedVinylID] = map[int64]struct{}{}
				}
				ownedPressings[ownedVinylID][ownedReleaseID] = struct{}{}
			}
			if err := rowsOwned.Err(); err != nil {
				log.Printf("[Keeper] failed iterating owned pressings for user %d: %v", userID, err)
			}
			rowsOwned.Close()
		}
	}

	rows, err := k.db.QueryContext(k.ctx, `
		SELECT
			vu.vinyl_id,
			vu.vinyl_title,
			vu.vinyl_artist,
			vu.master_id,
			vu.styles,
			vu.genres,
			COALESCE(vrc.released_year, CAST(substr(vr.released, 1, 4) AS INTEGER)) AS vinyl_pressing_year,
			COALESCE(NULLIF(vrc.country, ''), vr.country) AS country,
			vr.released,
			vr.lowest_price,
			vr.image_extension,
			vr.cover_embedding,
			vr.release_id,
			vrc.label,
			vrc.release_format,
			COALESCE(NULLIF(vrc.country, ''), vr.country) AS release_country,
			vrc.cover_uri,
			CASE WHEN length(vr.cover_raw_blob) > 0 THEN 1 ELSE 0 END AS has_blob,
			vr.notes
		FROM vinyl_release vr
		JOIN vinyl_unique vu ON vu.vinyl_id = vr.vinyl_id
		LEFT JOIN vinyl_releases_check vrc
			ON vrc.vinyl_id = vr.vinyl_id
		   AND vrc.release_id = vr.release_id
		WHERE length(vr.cover_embedding) > 0
		  AND length(vr.cover_raw_blob) > 0
	`)
	if err != nil {
		log.Printf("[Keeper] failed to query release candidates: %v", err)
		return []vinyl.ReleaseCandidate{}
	}
	defer rows.Close()

	type scoredCandidate struct {
		candidate vinyl.ReleaseCandidate
		score     float64
	}
	scored := make([]scoredCandidate, 0)

	for rows.Next() {
		var row releaseScanRow
		if err := rows.Scan(
			&row.VinylID,
			&row.VinylTitle,
			&row.VinylArtist,
			&row.MasterID,
			&row.Styles,
			&row.Genres,
			&row.VinylPressingYear,
			&row.Country,
			&row.Released,
			&row.RecentPrice,
			&row.ImageExtension,
			&row.CoverEmbedding,
			&row.ReleaseID,
			&row.Label,
			&row.ReleaseFormat,
			&row.ReleaseCountry,
			&row.CoverURI,
			&row.HasBlob,
			&row.Notes,
		); err != nil {
			log.Printf("[Keeper] failed scanning release row: %v", err)
			continue
		}

		emb, err := EmbeddingFromBlob(row.CoverEmbedding)
		if err != nil {
			continue
		}

		if ownedReleases, ownsVinyl := ownedPressings[row.VinylID]; ownsVinyl {
			if _, ownsPressing := ownedReleases[row.ReleaseID]; !ownsPressing {
				continue
			}
		}

		sim := cosineSimilarity(input, emb)
		scored = append(scored, scoredCandidate{
			candidate: vinyl.ReleaseCandidate{
				VinylRecord: vinyl.VinylRecord{
					VinylID:           row.VinylID,
					VinylTitle:        row.VinylTitle,
					VinylArtist:       row.VinylArtist,
					MasterID:          nullableInt64(row.MasterID),
					Genres:            nullableString(row.Genres),
					Styles:            nullableString(row.Styles),
					Country:           nullableString(row.Country),
					Released:          row.Released,
					RecentPrice:       nullableFloat64(row.RecentPrice),
					ImageExtension:    row.ImageExtension,
					CoverEmbedding:    row.CoverEmbedding,
					VinylPressingYear: nullableInt64Value(row.VinylPressingYear),
				},
				ReleaseID:      row.ReleaseID,
				Label:          nullableString(row.Label),
				ReleaseFormat:  nullableString(row.ReleaseFormat),
				ReleaseCountry: nullableString(row.ReleaseCountry),
				CoverURI:       nullableString(row.CoverURI),
				HasBlob:        row.HasBlob == 1,
				Notes:          nullableString(row.Notes),
				Similarity:     sim,
			},
			score: sim,
		})
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			if scored[i].candidate.VinylID == scored[j].candidate.VinylID {
				return scored[i].candidate.ReleaseID < scored[j].candidate.ReleaseID
			}
			return scored[i].candidate.VinylID < scored[j].candidate.VinylID
		}
		return scored[i].score > scored[j].score
	})

	if n > len(scored) {
		n = len(scored)
	}
	out := make([]vinyl.ReleaseCandidate, 0, n)
	for i := 0; i < n; i++ {
		candidate := scored[i].candidate
		candidate.Similarity = scored[i].score
		out = append(out, candidate)
	}
	return out
}

func (k *keeper) GetReleaseCandidate(vinylID, releaseID int64) (*vinyl.ReleaseCandidate, error) {
	row := k.db.QueryRowContext(k.ctx, `
		SELECT
			vu.vinyl_id,
			vu.vinyl_title,
			vu.vinyl_artist,
			vu.master_id,
			vu.styles,
			vu.genres,
			COALESCE(vrc.released_year, CAST(substr(vr.released, 1, 4) AS INTEGER)) AS vinyl_pressing_year,
			COALESCE(NULLIF(vrc.country, ''), vr.country) AS country,
			vr.released,
			vr.lowest_price,
			vr.image_extension,
			vr.cover_embedding,
			vr.release_id,
			vrc.label,
			vrc.release_format,
			COALESCE(NULLIF(vrc.country, ''), vr.country) AS release_country,
			vrc.cover_uri,
			CASE WHEN length(vr.cover_raw_blob) > 0 THEN 1 ELSE 0 END AS has_blob,
			vr.notes
		FROM vinyl_release vr
		JOIN vinyl_unique vu ON vu.vinyl_id = vr.vinyl_id
		LEFT JOIN vinyl_releases_check vrc
			ON vrc.vinyl_id = vr.vinyl_id
		   AND vrc.release_id = vr.release_id
		WHERE vr.vinyl_id = ? AND vr.release_id = ?
		LIMIT 1
	`, vinylID, releaseID)

	var scanned releaseScanRow
	if err := row.Scan(
		&scanned.VinylID,
		&scanned.VinylTitle,
		&scanned.VinylArtist,
		&scanned.MasterID,
		&scanned.Styles,
		&scanned.Genres,
		&scanned.VinylPressingYear,
		&scanned.Country,
		&scanned.Released,
		&scanned.RecentPrice,
		&scanned.ImageExtension,
		&scanned.CoverEmbedding,
		&scanned.ReleaseID,
		&scanned.Label,
		&scanned.ReleaseFormat,
		&scanned.ReleaseCountry,
		&scanned.CoverURI,
		&scanned.HasBlob,
		&scanned.Notes,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	return &vinyl.ReleaseCandidate{
		VinylRecord: vinyl.VinylRecord{
			VinylID:           scanned.VinylID,
			VinylTitle:        scanned.VinylTitle,
			VinylArtist:       scanned.VinylArtist,
			MasterID:          nullableInt64(scanned.MasterID),
			Genres:            nullableString(scanned.Genres),
			Styles:            nullableString(scanned.Styles),
			Country:           nullableString(scanned.Country),
			Released:          scanned.Released,
			RecentPrice:       nullableFloat64(scanned.RecentPrice),
			ImageExtension:    scanned.ImageExtension,
			CoverEmbedding:    scanned.CoverEmbedding,
			VinylPressingYear: nullableInt64Value(scanned.VinylPressingYear),
		},
		ReleaseID:      scanned.ReleaseID,
		Label:          nullableString(scanned.Label),
		ReleaseFormat:  nullableString(scanned.ReleaseFormat),
		ReleaseCountry: nullableString(scanned.ReleaseCountry),
		CoverURI:       nullableString(scanned.CoverURI),
		HasBlob:        scanned.HasBlob == 1,
		Notes:          nullableString(scanned.Notes),
	}, nil
}

func (k *keeper) ListPressingOptions(vinylID, userID int64) ([]vinyl.ReleaseOption, error) {
	rows, err := k.db.QueryContext(k.ctx, `
		SELECT
			vu.vinyl_id,
			vu.vinyl_title,
			vu.vinyl_artist,
			vu.master_id,
			vu.styles,
			vu.genres,
			COALESCE(vrc.released_year, 0) AS vinyl_pressing_year,
			NULLIF(vrc.country, '') AS country,
			COALESCE(vr.released, '') AS released,
			vr.lowest_price,
			COALESCE(vr.image_extension, '') AS image_extension,
			vrc.release_id,
			NULLIF(vrc.label, '') AS label,
			NULLIF(vrc.release_format, '') AS release_format,
			NULLIF(vrc.country, '') AS release_country,
			NULLIF(vrc.cover_uri, '') AS cover_uri,
			CASE WHEN length(vr.cover_raw_blob) > 0 THEN 1 ELSE 0 END AS has_blob,
			vr.notes,
			CASE WHEN vrc.release_id = (
				SELECT vp.release_id
				FROM vinyl_plays vp
				WHERE vp.user_id = ? AND vp.vinyl_id = ?
				ORDER BY vp.played_date DESC, vp.play DESC
				LIMIT 1
			) THEN 1 ELSE 0 END AS is_current
		FROM vinyl_releases_check vrc
		JOIN vinyl_unique vu ON vu.vinyl_id = vrc.vinyl_id
		LEFT JOIN vinyl_release vr
			ON vr.vinyl_id = vrc.vinyl_id
		   AND vr.release_id = vrc.release_id
		WHERE vrc.vinyl_id = ?
		ORDER BY vrc.released_year ASC, vrc.release_id ASC
	`, userID, vinylID, vinylID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]vinyl.ReleaseOption, 0)
	for rows.Next() {
		var row releaseScanRow
		var isCurrent int64
		if err := rows.Scan(
			&row.VinylID,
			&row.VinylTitle,
			&row.VinylArtist,
			&row.MasterID,
			&row.Styles,
			&row.Genres,
			&row.VinylPressingYear,
			&row.Country,
			&row.Released,
			&row.RecentPrice,
			&row.ImageExtension,
			&row.ReleaseID,
			&row.Label,
			&row.ReleaseFormat,
			&row.ReleaseCountry,
			&row.CoverURI,
			&row.HasBlob,
			&row.Notes,
			&isCurrent,
		); err != nil {
			return nil, err
		}

		result = append(result, vinyl.ReleaseOption{
			ReleaseCandidate: vinyl.ReleaseCandidate{
				VinylRecord: vinyl.VinylRecord{
					VinylID:           row.VinylID,
					VinylTitle:        row.VinylTitle,
					VinylArtist:       row.VinylArtist,
					MasterID:          nullableInt64(row.MasterID),
					Genres:            nullableString(row.Genres),
					Styles:            nullableString(row.Styles),
					Country:           nullableString(row.Country),
					Released:          row.Released,
					RecentPrice:       nullableFloat64(row.RecentPrice),
					ImageExtension:    row.ImageExtension,
					VinylPressingYear: nullableInt64Value(row.VinylPressingYear),
				},
				ReleaseID:      row.ReleaseID,
				Label:          nullableString(row.Label),
				ReleaseFormat:  nullableString(row.ReleaseFormat),
				ReleaseCountry: nullableString(row.ReleaseCountry),
				CoverURI:       nullableString(row.CoverURI),
				HasBlob:        row.HasBlob == 1,
				Notes:          nullableString(row.Notes),
			},
			IsCurrent: isCurrent == 1,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func (k *keeper) ensureReleaseMaterialized(vinylID, releaseID int64) error {
	var existingEmbLen int64
	var existingCoverLen int64
	err := k.db.QueryRowContext(k.ctx, `
		SELECT length(cover_embedding), length(cover_raw_blob)
		FROM vinyl_release
		WHERE vinyl_id = ? AND release_id = ?
		LIMIT 1
	`, vinylID, releaseID).Scan(&existingEmbLen, &existingCoverLen)
	if err == nil && existingEmbLen > 0 && existingCoverLen > 0 {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	v := k.GetVinyl(vinylID)
	if v == nil {
		return fmt.Errorf("vinyl %d not found", vinylID)
	}

	masterID := 0
	if v.MasterID != nil {
		masterID = int(*v.MasterID)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	payload, _, err := buildMainReleasePayload(masterID, int(releaseID), httpClient)
	if err != nil {
		return err
	}

	currentMaster := int64(0)
	_ = k.db.QueryRowContext(k.ctx, `
		SELECT master_release
		FROM vinyl_release
		WHERE vinyl_id = ? AND release_id = ?
		LIMIT 1
	`, vinylID, releaseID).Scan(&currentMaster)

	_, err = k.db.ExecContext(k.ctx, `
		INSERT INTO vinyl_release(
			vinyl_id, release_id, lowest_price, price_last_updated, country, notes, released,
			master_release, resource_uri, image_extension, cover_raw_blob, cover_embedding
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(vinyl_id, release_id) DO UPDATE SET
			lowest_price = excluded.lowest_price,
			price_last_updated = excluded.price_last_updated,
			country = excluded.country,
			notes = excluded.notes,
			released = excluded.released,
			resource_uri = excluded.resource_uri,
			image_extension = excluded.image_extension,
			cover_raw_blob = excluded.cover_raw_blob,
			cover_embedding = excluded.cover_embedding
	`,
		vinylID,
		releaseID,
		payload.LowestPrice,
		time.Now().Format("2006-01-02"),
		payload.Country,
		payload.Notes,
		payload.Released,
		currentMaster,
		payload.ResourceURI,
		payload.ImageExtension,
		payload.RawCoverData,
		payload.CoverEmbedding,
	)
	if err != nil {
		return err
	}

	if err := k.reloadVinylCaches(); err != nil {
		return err
	}
	return nil
}

func (k *keeper) ChangeUserPressing(vinylID, releaseID, userID int64) error {
	if err := k.ensureReleaseMaterialized(vinylID, releaseID); err != nil {
		return fmt.Errorf("materialize release %d for vinyl %d: %w", releaseID, vinylID, err)
	}

	tx, err := k.db.BeginTx(k.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(k.ctx, `
		SELECT play, played_date
		FROM vinyl_plays
		WHERE user_id = ? AND vinyl_id = ?
		ORDER BY played_date ASC, play ASC
	`, userID, vinylID)
	if err != nil {
		return err
	}

	ownershipDate := ""
	playDates := make([]string, 0)
	for rows.Next() {
		var play int64
		var playedDate string
		if err := rows.Scan(&play, &playedDate); err != nil {
			rows.Close()
			return err
		}
		if play == 0 {
			if ownershipDate == "" || playedDate < ownershipDate {
				ownershipDate = playedDate
			}
			continue
		}
		playDates = append(playDates, playedDate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	if ownershipDate == "" {
		if len(playDates) > 0 {
			ownershipDate = playDates[0]
		} else {
			ownershipDate = time.Now().Format("2006-01-02")
		}
	}

	if _, err := tx.ExecContext(k.ctx, `
		DELETE FROM vinyl_plays
		WHERE user_id = ? AND vinyl_id = ?
	`, userID, vinylID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(k.ctx, `
		INSERT INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date)
		VALUES (?, ?, ?, 0, ?)
	`, userID, vinylID, releaseID, ownershipDate); err != nil {
		return err
	}

	for i, playedDate := range playDates {
		if _, err := tx.ExecContext(k.ctx, `
			INSERT INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date)
			VALUES (?, ?, ?, ?, ?)
		`, userID, vinylID, releaseID, int64(i+1), playedDate); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	k.mu.Lock()
	if k.userNumPlays[userID] == nil {
		k.userNumPlays[userID] = make(map[int64]int)
	}
	k.userNumPlays[userID][vinylID] = len(playDates)
	k.mu.Unlock()

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

	if err := k.reloadVinylCaches(); err != nil {
		return err
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

func (k *keeper) reloadVinylCaches() error {
	vinyls, err := k.queries.GetAllVinylRecords(k.ctx)
	if err != nil {
		return err
	}

	lookup := make(map[int64]vinyl.VinylRecord, len(vinyls))
	embeddings := make(map[int64]Embedding, len(vinyls))
	for _, row := range vinyls {
		v := mapVinylRecord(row)
		lookup[v.VinylID] = v
		emb, err := EmbeddingFromBlob(v.CoverEmbedding)
		if err != nil {
			return fmt.Errorf("failed to decode embedding for vinyl %d: %w", v.VinylID, err)
		}
		embeddings[v.VinylID] = emb
	}

	k.mu.Lock()
	k.vinylLookup = lookup
	k.embeddingLookup = embeddings
	k.needsRebuild = true
	k.mu.Unlock()

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

func nullableString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	trimmed := strings.TrimSpace(v.String)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	val := v.Int64
	return &val
}

func nullableInt64Value(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func nullableFloat64(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	val := v.Float64
	return &val
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

type tableColumnInfo struct {
	Name    string
	NotNull bool
}

func tableColumns(ctx context.Context, db *sql.DB, tableName string) (map[string]tableColumnInfo, error) {
	query := fmt.Sprintf("SELECT name, \"notnull\" FROM pragma_table_info('%s')", tableName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := make(map[string]tableColumnInfo)
	for rows.Next() {
		var info tableColumnInfo
		var notNullInt int64
		if err := rows.Scan(&info.Name, &notNullInt); err != nil {
			return nil, err
		}
		info.NotNull = notNullInt == 1
		columns[info.Name] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func rebuildVinylUniqueCanonical(ctx context.Context, db *sql.DB, columns map[string]tableColumnInfo) error {
	masterExpr := "NULL"
	if _, ok := columns["master_id"]; ok {
		masterExpr = "master_id"
	}
	if _, ok := columns["discogs_master_id"]; ok {
		if masterExpr == "NULL" {
			masterExpr = "discogs_master_id"
		} else {
			masterExpr = "COALESCE(master_id, discogs_master_id)"
		}
	}

	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disable foreign keys for vinyl_unique rebuild: %w", err)
	}
	defer db.ExecContext(ctx, "PRAGMA foreign_keys = ON")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE vinyl_unique_new(
			vinyl_id INTEGER PRIMARY KEY AUTOINCREMENT,
			vinyl_title TEXT NOT NULL,
			vinyl_artist TEXT NOT NULL,
			master_id INTEGER,
			styles TEXT,
			genres TEXT,
			UNIQUE (vinyl_title, vinyl_artist)
		)
	`); err != nil {
		return fmt.Errorf("create vinyl_unique_new: %w", err)
	}

	insertStmt := fmt.Sprintf(`
		INSERT INTO vinyl_unique_new(vinyl_id, vinyl_title, vinyl_artist, master_id, styles, genres)
		SELECT vinyl_id, vinyl_title, vinyl_artist, %s, styles, genres
		FROM vinyl_unique
	`, masterExpr)
	if _, err := tx.ExecContext(ctx, insertStmt); err != nil {
		return fmt.Errorf("copy vinyl_unique into canonical table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DROP TABLE vinyl_unique"); err != nil {
		return fmt.Errorf("drop legacy vinyl_unique: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE vinyl_unique_new RENAME TO vinyl_unique"); err != nil {
		return fmt.Errorf("rename vinyl_unique_new: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, "PRAGMA foreign_key_check"); err != nil {
		return fmt.Errorf("foreign key check after vinyl_unique rebuild: %w", err)
	}

	return nil
}

func ensureSchemaCompatibility(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumns(ctx, db, "vinyl_unique")
	if err != nil {
		return fmt.Errorf("read vinyl_unique columns: %w", err)
	}

	legacyColumns := []string{
		"discogs_master_id",
		"vinyl_pressing_year",
		"first_pressing",
		"image_extension",
		"cover_raw_blob",
		"cover_embedding",
	}
	rebuildNeeded := false
	for _, column := range legacyColumns {
		if _, exists := columns[column]; exists {
			rebuildNeeded = true
			break
		}
	}
	if _, hasMasterID := columns["master_id"]; !hasMasterID {
		rebuildNeeded = true
	}

	if rebuildNeeded {
		if err := rebuildVinylUniqueCanonical(ctx, db, columns); err != nil {
			return fmt.Errorf("rebuild canonical vinyl_unique table: %w", err)
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

func (k *keeper) GetReleaseCover(vinylID, releaseID int64) ([]byte, string, bool) {
	var imageExtension string
	var cover []byte
	err := k.db.QueryRowContext(k.ctx, `
		SELECT image_extension, cover_raw_blob
		FROM vinyl_release
		WHERE vinyl_id = ? AND release_id = ?
		LIMIT 1
	`, vinylID, releaseID).Scan(&imageExtension, &cover)
	if err != nil {
		return nil, "", false
	}
	if len(cover) == 0 {
		return nil, "", false
	}
	return cover, imageExtension, true
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
