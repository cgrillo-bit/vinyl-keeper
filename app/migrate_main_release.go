package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type discogsReleaseResp struct {
	ID          int      `json:"id"`
	Year        int      `json:"year"`
	Released    string   `json:"released"`
	Country     string   `json:"country"`
	ResourceURL string   `json:"resource_url"`
	LowestPrice *float64 `json:"lowest_price"`
	Notes       *string  `json:"notes"`
	Images      []struct {
		Type string `json:"type"`
		URI  string `json:"uri"`
	} `json:"images"`
}

type mainReleaseBackfill struct {
	ReleaseID      int
	Released       string
	Country        string
	PriceUpdated   string
	ResourceURI    string
	ImageExtension string
	RawCoverData   []byte
	CoverEmbedding []byte
	LowestPrice    *float64
	Notes          *string
}

type backfillTimings struct {
	FetchVersions      time.Duration
	PersistVersions    time.Duration
	CheckPrimaryExists time.Duration
	SetPrimaryFlag     time.Duration
	FetchRelease       time.Duration
	FetchMasterFallback time.Duration
	DownloadImage      time.Duration
	Embed              time.Duration
}

type discogsMasterVersion struct {
	ID       int    `json:"id"`
	Label    string `json:"label"`
	Country  string `json:"country"`
	Format   string `json:"format"`
	Released string `json:"released"`
	Thumb    string `json:"thumb"`
}

type discogsMasterVersionsResp struct {
	Pagination struct {
		Page  int `json:"page"`
		Pages int `json:"pages"`
	} `json:"pagination"`
	Versions []discogsMasterVersion `json:"versions"`
}

type migrationCandidate struct {
	VinylID  int64
	MasterID int
}

type masterBackfillCacheEntry struct {
	Versions []discogsMasterVersion
	Payload  *mainReleaseBackfill
}

func (k *keeper) MigrateMainReleaseEmbeddings() error {
	if k.db == nil {
		return fmt.Errorf("database not initialized")
	}

	if err := ensureVinylReleaseTable(k.ctx, k.db); err != nil {
		return err
	}
	if err := ensureVinylReleasesCheckTable(k.ctx, k.db); err != nil {
		return err
	}

	masterIDColumn, err := findMasterIDColumn(k.ctx, k.db)
	if err != nil {
		return err
	}

	query := fmt.Sprintf(
		"SELECT vinyl_id, %s FROM vinyl_unique WHERE %s IS NOT NULL ORDER BY vinyl_id ASC",
		masterIDColumn,
		masterIDColumn,
	)
	countQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM vinyl_unique WHERE %s IS NOT NULL",
		masterIDColumn,
	)

	var candidateCount int
	if err := k.db.QueryRowContext(k.ctx, countQuery).Scan(&candidateCount); err != nil {
		return fmt.Errorf("count migration candidates: %w", err)
	}
	log.Printf("[migration] starting release migration candidates=%d", candidateCount)

	rows, err := k.db.QueryContext(k.ctx, query)
	if err != nil {
		return fmt.Errorf("query migration candidates: %w", err)
	}

	candidates := make([]migrationCandidate, 0, candidateCount)
	for rows.Next() {
		var vinylID int64
		var masterIDRaw sql.NullInt64
		if err := rows.Scan(&vinylID, &masterIDRaw); err != nil {
			rows.Close()
			return fmt.Errorf("scan migration candidate row: %w", err)
		}
		if !masterIDRaw.Valid || masterIDRaw.Int64 <= 0 {
			continue
		}
		candidates = append(candidates, migrationCandidate{
			VinylID:  vinylID,
			MasterID: int(masterIDRaw.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate migration candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close migration candidates rows: %w", err)
	}
	candidateCount = len(candidates)
	log.Printf("[migration] loaded valid candidates=%d", candidateCount)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	startedAt := time.Now()

	total := 0
	migrated := 0
	skipped := 0
	failed := 0
	masterCache := make(map[int]masterBackfillCacheEntry)

	for _, candidate := range candidates {
		total++
		vinylID := candidate.VinylID
		masterID := candidate.MasterID
		log.Printf("[migration] [%d/%d] processing vinyl_id=%d master_id=%d", total, candidateCount, vinylID, masterID)

		var alreadyDone bool
		for {
			var err error
			alreadyDone, err = migrationAlreadyDoneForVinyl(k.ctx, k.db, vinylID)
			if err == nil {
				break
			}
			failed++
			log.Printf("[migration] [%d/%d] failed precheck vinyl_id=%d: %v", total, candidateCount, vinylID, err)
			log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
			time.Sleep(discogsFailureRequestInterval)
		}
		if alreadyDone {
			skipped++
			log.Printf("[migration] [%d/%d] skipped vinyl_id=%d (already has vinyl_release + vinyl_releases_check)", total, candidateCount, vinylID)
			continue
		}

		attempt := 1
		for {
			rowStarted := time.Now()
			cacheEntry, fromCache := masterCache[masterID]
			timings := backfillTimings{}

			if !fromCache {
				versionsStarted := time.Now()
				versions, err := fetchAllVinylVersions(masterID, httpClient)
				timings.FetchVersions = time.Since(versionsStarted)
				if err != nil {
					failed++
					log.Printf("[migration] [%d/%d] attempt=%d fetch versions failed vinyl_id=%d master_id=%d: %v", total, candidateCount, attempt, vinylID, masterID, err)
					log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
					time.Sleep(discogsFailureRequestInterval)
					attempt++
					continue
				}
				cacheEntry = masterBackfillCacheEntry{Versions: versions}
				masterCache[masterID] = cacheEntry
			} else {
				log.Printf("[migration] cache hit master_id=%d release_id=%d", masterID, cacheEntry.Versions[0].ID)
			}

			releaseID := cacheEntry.Versions[0].ID

			persistStarted := time.Now()
			if err := upsertVinylReleasesCheck(k.ctx, k.db, vinylID, cacheEntry.Versions); err != nil {
				failed++
				log.Printf("[migration] [%d/%d] attempt=%d upsert releases_check failed vinyl_id=%d master_id=%d: %v", total, candidateCount, attempt, vinylID, masterID, err)
				log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
				time.Sleep(discogsFailureRequestInterval)
				attempt++
				continue
			}
			timings.PersistVersions = time.Since(persistStarted)

			releaseExistsStarted := time.Now()
			releaseExists, err := primaryReleaseExists(k.ctx, k.db, vinylID, releaseID)
			timings.CheckPrimaryExists = time.Since(releaseExistsStarted)
			if err != nil {
				failed++
				log.Printf("[migration] [%d/%d] attempt=%d check release failed vinyl_id=%d release_id=%d: %v", total, candidateCount, attempt, vinylID, releaseID, err)
				log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
				time.Sleep(discogsFailureRequestInterval)
				attempt++
				continue
			}

			if releaseExists {
				setPrimaryStarted := time.Now()
				if err := setPrimaryReleaseFlag(k.ctx, k.db, vinylID, releaseID); err != nil {
					failed++
					log.Printf("[migration] [%d/%d] attempt=%d set primary failed vinyl_id=%d release_id=%d: %v", total, candidateCount, attempt, vinylID, releaseID, err)
					log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
					time.Sleep(discogsFailureRequestInterval)
					attempt++
					continue
				}
				timings.SetPrimaryFlag = time.Since(setPrimaryStarted)
				migrated++
				rowDuration := time.Since(rowStarted)
				elapsed := time.Since(startedAt)
				avgPerRow := elapsed / time.Duration(total)
				remaining := candidateCount - total
				eta := time.Duration(0)
				if remaining > 0 {
					eta = avgPerRow * time.Duration(remaining)
				}
				log.Printf(
					"[migration] [%d/%d] migrated vinyl_id=%d master_id=%d -> release_id=%d row=%s (versions=%s persist_versions=%s check_release=%s set_primary=%s deep_fetch=skipped) migrated=%d skipped=%d failed=%d elapsed=%s eta=%s",
					total,
					candidateCount,
					vinylID,
					masterID,
					releaseID,
					rowDuration.Round(time.Millisecond),
					timings.FetchVersions.Round(time.Millisecond),
					timings.PersistVersions.Round(time.Millisecond),
					timings.CheckPrimaryExists.Round(time.Millisecond),
					timings.SetPrimaryFlag.Round(time.Millisecond),
					migrated,
					skipped,
					failed,
					elapsed.Truncate(time.Second),
					eta.Truncate(time.Second),
				)
				break
			}

			if cacheEntry.Payload == nil {
				payload, payloadTimings, err := buildMainReleasePayload(masterID, releaseID, httpClient)
				if err != nil {
					failed++
					log.Printf("[migration] [%d/%d] attempt=%d deep fetch failed vinyl_id=%d master_id=%d release_id=%d: %v", total, candidateCount, attempt, vinylID, masterID, releaseID, err)
					log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
					time.Sleep(discogsFailureRequestInterval)
					attempt++
					continue
				}
				timings.FetchRelease = payloadTimings.FetchRelease
				timings.FetchMasterFallback = payloadTimings.FetchMasterFallback
				timings.DownloadImage = payloadTimings.DownloadImage
				timings.Embed = payloadTimings.Embed
				cacheEntry.Payload = &payload
				masterCache[masterID] = cacheEntry
			}

			upsertStarted := time.Now()
			if err := upsertMainRelease(k.ctx, k.db, vinylID, *cacheEntry.Payload); err != nil {
				failed++
				log.Printf("[migration] [%d/%d] attempt=%d upsert main release failed vinyl_id=%d master_id=%d release_id=%d: %v", total, candidateCount, attempt, vinylID, masterID, releaseID, err)
				log.Printf("[migration] cooldown waiting %s before retry", discogsFailureRequestInterval)
				time.Sleep(discogsFailureRequestInterval)
				attempt++
				continue
			}
			upsertDuration := time.Since(upsertStarted)

			migrated++
			rowDuration := time.Since(rowStarted)
			elapsed := time.Since(startedAt)
			avgPerRow := elapsed / time.Duration(total)
			remaining := candidateCount - total
			eta := time.Duration(0)
			if remaining > 0 {
				eta = avgPerRow * time.Duration(remaining)
			}
			log.Printf(
				"[migration] [%d/%d] migrated vinyl_id=%d master_id=%d -> release_id=%d row=%s (versions=%s persist_versions=%s check_release=%s release=%s master_fallback=%s image=%s embed=%s upsert=%s) migrated=%d skipped=%d failed=%d elapsed=%s eta=%s",
				total,
				candidateCount,
				vinylID,
				masterID,
				releaseID,
				rowDuration.Round(time.Millisecond),
				timings.FetchVersions.Round(time.Millisecond),
				timings.PersistVersions.Round(time.Millisecond),
				timings.CheckPrimaryExists.Round(time.Millisecond),
				timings.FetchRelease.Round(time.Millisecond),
				timings.FetchMasterFallback.Round(time.Millisecond),
				timings.DownloadImage.Round(time.Millisecond),
				timings.Embed.Round(time.Millisecond),
				upsertDuration.Round(time.Millisecond),
				migrated,
				skipped,
				failed,
				elapsed.Truncate(time.Second),
				eta.Truncate(time.Second),
			)
			break
		}
	}

	log.Printf("[migration] done: total=%d migrated=%d skipped=%d failed_attempts=%d elapsed=%s", total, migrated, skipped, failed, time.Since(startedAt).Truncate(time.Second))

	return nil
}


func buildMainReleasePayload(masterID, releaseID int, httpClient *http.Client) (mainReleaseBackfill, backfillTimings, error) {
	timings := backfillTimings{}

	stepStarted := time.Now()
	releaseResult, err := fetchDiscogsRelease(releaseID, httpClient)
	timings.FetchRelease = time.Since(stepStarted)
	if err != nil {
		return mainReleaseBackfill{}, timings, err
	}

	var masterResult *discogsMasterResp
	loadMasterFallback := func() (*discogsMasterResp, error) {
		if masterResult != nil {
			return masterResult, nil
		}
		started := time.Now()
		m, err := fetchDiscogsMaster(masterID, httpClient)
		timings.FetchMasterFallback = time.Since(started)
		if err != nil {
			return nil, err
		}
		masterResult = &m
		return masterResult, nil
	}

	imageURI := pickDiscogsImageURI(releaseResult.Images)
	if imageURI == "" {
		masterFallback, err := loadMasterFallback()
		if err != nil {
			return mainReleaseBackfill{}, timings, fmt.Errorf("fetch master %d for image fallback: %w", masterID, err)
		}
		imageURI = pickDiscogsImageURI(masterFallback.Images)
	}
	if imageURI == "" {
		return mainReleaseBackfill{}, timings, fmt.Errorf("no image URI found for master %d release %d", masterID, releaseResult.ID)
	}

	stepStarted = time.Now()
	rawImageData, err := downloadDiscogsImage(imageURI, httpClient)
	timings.DownloadImage = time.Since(stepStarted)
	if err != nil {
		return mainReleaseBackfill{}, timings, err
	}

	stepStarted = time.Now()
	embedding, err := RequestEmbedding(rawImageData)
	timings.Embed = time.Since(stepStarted)
	if err != nil {
		return mainReleaseBackfill{}, timings, fmt.Errorf("generate embedding for master %d release %d: %w", masterID, releaseResult.ID, err)
	}

	released := strings.TrimSpace(releaseResult.Released)
	if released == "" {
		released = fmt.Sprintf("%d", releaseResult.Year)
	}
	if released == "0" || released == "" {
		masterFallback, err := loadMasterFallback()
		if err != nil {
			return mainReleaseBackfill{}, timings, fmt.Errorf("fetch master %d for released fallback: %w", masterID, err)
		}
		released = fmt.Sprintf("%d", masterFallback.Year)
	}
	if strings.TrimSpace(releaseResult.ResourceURL) == "" {
		releaseResult.ResourceURL = fmt.Sprintf("https://api.discogs.com/releases/%d", releaseResult.ID)
	}

	return mainReleaseBackfill{
		ReleaseID:      releaseResult.ID,
		Released:       released,
		Country:        strings.TrimSpace(releaseResult.Country),
		PriceUpdated:   time.Now().Format("2006-01-02"),
		ResourceURI:    releaseResult.ResourceURL,
		ImageExtension: imageExtensionFromURI(imageURI),
		RawCoverData:   rawImageData,
		CoverEmbedding: EmbeddingToBlob(embedding),
		LowestPrice:    releaseResult.LowestPrice,
		Notes:          releaseResult.Notes,
	}, timings, nil
}

func fetchAllVinylVersions(masterID int, httpClient *http.Client) ([]discogsMasterVersion, error) {
	baseURL := fmt.Sprintf("https://api.discogs.com/masters/%d/versions", masterID)
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse versions url for %d: %w", masterID, err)
	}

	query := parsedURL.Query()
	query.Set("format", "Vinyl")
	query.Set("type", "master")
	query.Set("sort", "released")
	query.Set("sort_order", "asc")
	query.Set("per_page", "100")
	query.Set("page", "1")
	parsedURL.RawQuery = query.Encode()

	firstPage, err := fetchDiscogsVersionsPage(parsedURL.String(), masterID, httpClient)
	if err != nil {
		return nil, err
	}

	versions := make([]discogsMasterVersion, 0, len(firstPage.Versions))
	versions = append(versions, firstPage.Versions...)

	for page := 2; page <= firstPage.Pagination.Pages; page++ {
		query.Set("page", fmt.Sprintf("%d", page))
		parsedURL.RawQuery = query.Encode()
		nextPage, err := fetchDiscogsVersionsPage(parsedURL.String(), masterID, httpClient)
		if err != nil {
			return nil, err
		}
		versions = append(versions, nextPage.Versions...)
	}

	if len(versions) == 0 {
		msg := fmt.Sprintf("master %d returned no vinyl versions; this should never happen", masterID)
		log.Printf("[migration] FATAL: %s", msg)
		panic(msg)
	}
	if versions[0].ID <= 0 {
		msg := fmt.Sprintf("master %d returned invalid first version id=%d; this should never happen", masterID, versions[0].ID)
		log.Printf("[migration] FATAL: %s", msg)
		panic(msg)
	}

	return versions, nil
}

func fetchDiscogsVersionsPage(rawURL string, masterID int, httpClient *http.Client) (discogsMasterVersionsResp, error) {
	resp, err := discogsGET(rawURL, httpClient)
	if err != nil {
		return discogsMasterVersionsResp{}, fmt.Errorf("versions request failed for %d: %w", masterID, err)
	}
	defer resp.Body.Close()

	var out discogsMasterVersionsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return discogsMasterVersionsResp{}, fmt.Errorf("decode versions response for %d: %w", masterID, err)
	}
	if out.Pagination.Pages < 1 {
		out.Pagination.Pages = 1
	}
	return out, nil
}

func fetchDiscogsMaster(masterID int, httpClient *http.Client) (discogsMasterResp, error) {
	masterURL := fmt.Sprintf("https://api.discogs.com/masters/%d", masterID)
	resp, err := discogsGET(masterURL, httpClient)
	if err != nil {
		return discogsMasterResp{}, fmt.Errorf("master request failed for %d: %w", masterID, err)
	}
	defer resp.Body.Close()

	var out discogsMasterResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return discogsMasterResp{}, fmt.Errorf("decode master response for %d: %w", masterID, err)
	}

	return out, nil
}

func fetchDiscogsRelease(releaseID int, httpClient *http.Client) (discogsReleaseResp, error) {
	releaseURL := fmt.Sprintf("https://api.discogs.com/releases/%d", releaseID)
	resp, err := discogsGET(releaseURL, httpClient)
	if err != nil {
		return discogsReleaseResp{}, fmt.Errorf("release request failed for %d: %w", releaseID, err)
	}
	defer resp.Body.Close()

	var out discogsReleaseResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return discogsReleaseResp{}, fmt.Errorf("decode release response for %d: %w", releaseID, err)
	}

	if out.ID == 0 {
		out.ID = releaseID
	}

	return out, nil
}

func discogsGET(rawURL string, httpClient *http.Client) (*http.Response, error) {
	log.Printf("[Discogs] GET %s", rawURL)
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vinyl-keeper/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	return resp, nil
}

func downloadDiscogsImage(imageURI string, httpClient *http.Client) ([]byte, error) {
	waitForDiscogsRequestSlot()
	resp, err := discogsGET(imageURI, httpClient)
	if err != nil {
		recordDiscogsRequestResult(false)
		return nil, fmt.Errorf("image download failed for %s: %w", imageURI, err)
	}
	defer resp.Body.Close()
	recordDiscogsRequestResult(true)

	rawImageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read image data from %s: %w", imageURI, err)
	}

	if len(rawImageData) == 0 {
		return nil, fmt.Errorf("empty image response from %s", imageURI)
	}

	return rawImageData, nil
}

func versionReleasedYear(v discogsMasterVersion) int {
	released := strings.TrimSpace(v.Released)
	if released == "" {
		return 0
	}
	year, err := strconv.Atoi(released)
	if err != nil {
		return 0
	}
	if year < 0 {
		return 0
	}
	return year
}

func upsertVinylReleasesCheck(ctx context.Context, db *sql.DB, vinylID int64, versions []discogsMasterVersion) error {
	for _, v := range versions {
		year := versionReleasedYear(v)
		if v.ID <= 0 {
			return fmt.Errorf("invalid release_id=%d", v.ID)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO vinyl_releases_check(vinyl_id, release_id, label, country, release_format, released_year, cover_uri)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(vinyl_id, release_id) DO UPDATE SET
				label = excluded.label,
				country = excluded.country,
				release_format = excluded.release_format,
				released_year = excluded.released_year,
				cover_uri = excluded.cover_uri
		`,
			vinylID,
			v.ID,
			strings.TrimSpace(v.Label),
			strings.TrimSpace(v.Country),
			strings.TrimSpace(v.Format),
			year,
			strings.TrimSpace(v.Thumb),
		); err != nil {
			return fmt.Errorf("upsert vinyl_releases_check release_id=%d: %w", v.ID, err)
		}
	}
	return nil
}

func primaryReleaseExists(ctx context.Context, db *sql.DB, vinylID int64, releaseID int) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `
		SELECT 1
		FROM vinyl_release
		WHERE vinyl_id = ? AND release_id = ?
		LIMIT 1
	`, vinylID, releaseID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func migrationAlreadyDoneForVinyl(ctx context.Context, db *sql.DB, vinylID int64) (bool, error) {
	var hasPrimary int
	err := db.QueryRowContext(ctx, `
		SELECT CASE WHEN EXISTS(
			SELECT 1 FROM vinyl_release WHERE vinyl_id = ? LIMIT 1
		) THEN 1 ELSE 0 END
	`, vinylID).Scan(&hasPrimary)
	if err != nil {
		return false, err
	}

	var hasCheck int
	err = db.QueryRowContext(ctx, `
		SELECT CASE WHEN EXISTS(
			SELECT 1 FROM vinyl_releases_check WHERE vinyl_id = ? LIMIT 1
		) THEN 1 ELSE 0 END
	`, vinylID).Scan(&hasCheck)
	if err != nil {
		return false, err
	}

	return hasPrimary == 1 && hasCheck == 1, nil
}

func setPrimaryReleaseFlag(ctx context.Context, db *sql.DB, vinylID int64, releaseID int) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE vinyl_release
		SET master_release = CASE WHEN release_id = ? THEN 1 ELSE 0 END
		WHERE vinyl_id = ?
	`, releaseID, vinylID); err != nil {
		return err
	}
	return nil
}

func pickDiscogsImageURI(images []struct {
	Type string `json:"type"`
	URI  string `json:"uri"`
}) string {
	for i := range images {
		if strings.EqualFold(images[i].Type, "primary") && strings.TrimSpace(images[i].URI) != "" {
			return images[i].URI
		}
	}
	for i := range images {
		if strings.TrimSpace(images[i].URI) != "" {
			return images[i].URI
		}
	}
	return ""
}

func imageExtensionFromURI(rawURI string) string {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(parsed.Path)), ".")
	return ext
}

func findMasterIDColumn(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(vinyl_unique)")
	if err != nil {
		return "", fmt.Errorf("inspect vinyl_unique columns: %w", err)
	}
	defer rows.Close()

	foundMasterID := false
	foundDiscogsMasterID := false
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return "", fmt.Errorf("scan table_info(vinyl_unique): %w", err)
		}

		switch name {
		case "master_id":
			foundMasterID = true
		case "discogs_master_id":
			foundDiscogsMasterID = true
		}
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate table_info(vinyl_unique): %w", err)
	}

	if foundMasterID {
		return "master_id", nil
	}
	if foundDiscogsMasterID {
		return "discogs_master_id", nil
	}

	return "", fmt.Errorf("vinyl_unique has neither master_id nor discogs_master_id")
}

func ensureVinylReleaseTable(ctx context.Context, db *sql.DB) error {
	const stmt = `
CREATE TABLE IF NOT EXISTS vinyl_release(
	vinyl_id INTEGER NOT NULL,
	release_id INTEGER NOT NULL,
	lowest_price REAL,
	price_last_updated TEXT,
	country TEXT,
	notes TEXT,
	released TEXT NOT NULL,
	master_release INTEGER NOT NULL,
	resource_uri TEXT NOT NULL,
	image_extension TEXT NOT NULL,
	cover_raw_blob BLOB NOT NULL,
	cover_embedding BLOB NOT NULL,
	PRIMARY KEY (vinyl_id, release_id),
	FOREIGN KEY (vinyl_id) REFERENCES vinyl_unique(vinyl_id) ON DELETE CASCADE
);`

	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("ensure vinyl_release table: %w", err)
	}

	return nil
}

func ensureVinylReleasesCheckTable(ctx context.Context, db *sql.DB) error {
	const createStmt = `
CREATE TABLE IF NOT EXISTS vinyl_releases_check(
	vinyl_id INTEGER NOT NULL,
	release_id INTEGER NOT NULL,
	label TEXT NOT NULL,
	country TEXT NOT NULL,
	release_format TEXT NOT NULL,
	released_year INTEGER NOT NULL,
	cover_uri TEXT NOT NULL,
	PRIMARY KEY (vinyl_id, release_id),
	FOREIGN KEY (vinyl_id) REFERENCES vinyl_unique(vinyl_id) ON DELETE CASCADE
);`

	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("ensure vinyl_releases_check table: %w", err)
	}

	hasFormat, err := columnExists(ctx, db, "vinyl_releases_check", "release_format")
	if err != nil {
		return fmt.Errorf("check vinyl_releases_check.release_format: %w", err)
	}
	if !hasFormat {
		if _, err := db.ExecContext(ctx, "ALTER TABLE vinyl_releases_check ADD COLUMN release_format TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add vinyl_releases_check.release_format: %w", err)
		}
	}

	hasLegacyReleaseFK := false
	fkRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_list(vinyl_releases_check)")
	if err != nil {
		return fmt.Errorf("inspect vinyl_releases_check foreign keys: %w", err)
	}
	for fkRows.Next() {
		var id int
		var seq int
		var tableName string
		var fromCol string
		var toCol string
		var onUpdate string
		var onDelete string
		var match string
		if err := fkRows.Scan(&id, &seq, &tableName, &fromCol, &toCol, &onUpdate, &onDelete, &match); err != nil {
			fkRows.Close()
			return fmt.Errorf("scan vinyl_releases_check foreign keys: %w", err)
		}
		if fromCol == "release_id" {
			hasLegacyReleaseFK = true
		}
	}
	if err := fkRows.Err(); err != nil {
		fkRows.Close()
		return fmt.Errorf("iterate vinyl_releases_check foreign keys: %w", err)
	}
	if err := fkRows.Close(); err != nil {
		return fmt.Errorf("close vinyl_releases_check foreign key cursor: %w", err)
	}

	if !hasLegacyReleaseFK {
		return nil
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS vinyl_releases_check_rebuild(
			vinyl_id INTEGER NOT NULL,
			release_id INTEGER NOT NULL,
			label TEXT NOT NULL,
			country TEXT NOT NULL,
			release_format TEXT NOT NULL,
			released_year INTEGER NOT NULL,
			cover_uri TEXT NOT NULL,
			PRIMARY KEY (vinyl_id, release_id),
			FOREIGN KEY (vinyl_id) REFERENCES vinyl_unique(vinyl_id) ON DELETE CASCADE
		)
	`); err != nil {
		return fmt.Errorf("create vinyl_releases_check rebuild table: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO vinyl_releases_check_rebuild(vinyl_id, release_id, label, country, release_format, released_year, cover_uri)
		SELECT vinyl_id, release_id, label, country, COALESCE(release_format, ''), released_year, cover_uri
		FROM vinyl_releases_check
		ON CONFLICT(vinyl_id, release_id) DO UPDATE SET
			label = excluded.label,
			country = excluded.country,
			release_format = excluded.release_format,
			released_year = excluded.released_year,
			cover_uri = excluded.cover_uri
	`); err != nil {
		return fmt.Errorf("copy vinyl_releases_check rows to rebuild table: %w", err)
	}

	if _, err := db.ExecContext(ctx, "DROP TABLE vinyl_releases_check"); err != nil {
		return fmt.Errorf("drop legacy vinyl_releases_check table: %w", err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE vinyl_releases_check_rebuild RENAME TO vinyl_releases_check"); err != nil {
		return fmt.Errorf("rename rebuilt vinyl_releases_check table: %w", err)
	}

	return nil
}

func upsertMainRelease(ctx context.Context, db *sql.DB, vinylID int64, payload mainReleaseBackfill) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		"UPDATE vinyl_release SET master_release = 0 WHERE vinyl_id = ? AND release_id <> ?",
		vinylID,
		payload.ReleaseID,
	); err != nil {
		return fmt.Errorf("clear existing master_release flag: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO vinyl_release (
			vinyl_id, release_id, lowest_price, price_last_updated, country, notes, released, master_release,
			resource_uri, image_extension, cover_raw_blob, cover_embedding
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(vinyl_id, release_id) DO UPDATE SET
			lowest_price = excluded.lowest_price,
			price_last_updated = excluded.price_last_updated,
			country = excluded.country,
			notes = excluded.notes,
			released = excluded.released,
			master_release = 1,
			resource_uri = excluded.resource_uri,
			image_extension = excluded.image_extension,
			cover_raw_blob = excluded.cover_raw_blob,
			cover_embedding = excluded.cover_embedding`,
		vinylID,
		payload.ReleaseID,
		payload.LowestPrice,
		payload.PriceUpdated,
		payload.Country,
		payload.Notes,
		payload.Released,
		payload.ResourceURI,
		payload.ImageExtension,
		payload.RawCoverData,
		payload.CoverEmbedding,
	); err != nil {
		return fmt.Errorf("upsert vinyl_release row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}

	return nil
}
