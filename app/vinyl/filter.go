package vinyl

import (
	"sort"
	"strings"
)

type FrontendVinyl interface {
	ID() int64
	Title() string
	Artist() string
	PressingYear() int64
	GenresValue() *string
	StylesValue() *string
	CountryValue() *string
	ReleaseDateValue() string
	RecentPriceValue() *float64
	ImageExtensionValue() string
	CoverRawBlobValue() []byte
	CoverEmbeddingValue() []byte
}

// VinylRecord is the frontend-facing model assembled from sqlc rows.
type VinylRecord struct {
	VinylID           int64
	VinylTitle        string
	VinylArtist       string
	VinylPressingYear int64
	MasterID          *int64
	Genres            *string
	Styles            *string
	Country           *string
	Released          string
	RecentPrice       *float64
	ImageExtension    string
	CoverRawBlob      []byte
	CoverEmbedding    []byte
}

func (v VinylRecord) ID() int64                   { return v.VinylID }
func (v VinylRecord) Title() string               { return v.VinylTitle }
func (v VinylRecord) Artist() string              { return v.VinylArtist }
func (v VinylRecord) PressingYear() int64         { return v.VinylPressingYear }
func (v VinylRecord) GenresValue() *string        { return v.Genres }
func (v VinylRecord) StylesValue() *string        { return v.Styles }
func (v VinylRecord) CountryValue() *string       { return v.Country }
func (v VinylRecord) ReleaseDateValue() string    { return v.Released }
func (v VinylRecord) RecentPriceValue() *float64  { return v.RecentPrice }
func (v VinylRecord) ImageExtensionValue() string { return v.ImageExtension }
func (v VinylRecord) CoverRawBlobValue() []byte   { return v.CoverRawBlob }
func (v VinylRecord) CoverEmbeddingValue() []byte {
	return v.CoverEmbedding
}

// VinylWithPlayData combines vinyl record data with user play statistics.
type VinylWithPlayData struct {
	VinylRecord
	Plays       int64
	FirstPlayed *string
	LastPlayed  *string
}

// VinylIndex holds precomputed lookup maps for efficient filtering
type VinylIndex struct {
	// Lookup maps: lowercase name -> set of vinyl IDs
	ArtistMap map[string]map[int64]struct{}
	GenreMap  map[string]map[int64]struct{}
	StyleMap  map[string]map[int64]struct{}

	// Sorted lists for UI dropdowns (title-cased for display)
	Artists []string
	Genres  []string
	Styles  []string
}

// FilterCriteria specifies how to filter and sort vinyl
type FilterCriteria struct {
	Artist string   // exact match (case-insensitive)
	Genres []string // match ANY of these genres (OR within category)
	Styles []string // match ANY of these styles (OR within category)
	// Categories are combined with AND logic
}

// BuildVinylIndex creates a new index from a slice of vinyl records.
func BuildVinylIndex(vinyls []VinylRecord) *VinylIndex {
	idx := &VinylIndex{
		ArtistMap: make(map[string]map[int64]struct{}),
		GenreMap:  make(map[string]map[int64]struct{}),
		StyleMap:  make(map[string]map[int64]struct{}),
	}

	artistSet := make(map[string]string) // lowercase -> original case
	genreSet := make(map[string]string)
	styleSet := make(map[string]string)

	for _, v := range vinyls {
		// Artist (always present)
		artistKey := strings.ToLower(strings.TrimSpace(v.VinylArtist))
		if idx.ArtistMap[artistKey] == nil {
			idx.ArtistMap[artistKey] = make(map[int64]struct{})
		}
		idx.ArtistMap[artistKey][v.VinylID] = struct{}{}
		artistSet[artistKey] = v.VinylArtist

		// Genres (nullable, comma-separated)
		if v.Genres != nil && *v.Genres != "" {
			genres := strings.Split(*v.Genres, ",")
			for _, g := range genres {
				g = strings.TrimSpace(g)
				if g == "" {
					continue
				}
				genreKey := strings.ToLower(g)
				if idx.GenreMap[genreKey] == nil {
					idx.GenreMap[genreKey] = make(map[int64]struct{})
				}
				idx.GenreMap[genreKey][v.VinylID] = struct{}{}
				genreSet[genreKey] = g
			}
		}

		// Styles (nullable, comma-separated)
		if v.Styles != nil && *v.Styles != "" {
			styles := strings.Split(*v.Styles, ",")
			for _, s := range styles {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				styleKey := strings.ToLower(s)
				if idx.StyleMap[styleKey] == nil {
					idx.StyleMap[styleKey] = make(map[int64]struct{})
				}
				idx.StyleMap[styleKey][v.VinylID] = struct{}{}
				styleSet[styleKey] = s
			}
		}
	}

	// Build sorted lists (using original case)
	idx.Artists = make([]string, 0, len(artistSet))
	for _, artist := range artistSet {
		idx.Artists = append(idx.Artists, artist)
	}
	sort.Strings(idx.Artists)

	idx.Genres = make([]string, 0, len(genreSet))
	for _, genre := range genreSet {
		idx.Genres = append(idx.Genres, genre)
	}
	sort.Strings(idx.Genres)

	idx.Styles = make([]string, 0, len(styleSet))
	for _, style := range styleSet {
		idx.Styles = append(idx.Styles, style)
	}
	sort.Strings(idx.Styles)

	return idx
}

// FilterVinyl filters and sorts vinyl records based on criteria.
func FilterVinyl(vinyls []VinylRecord, criteria FilterCriteria, index *VinylIndex) []VinylRecord {
	// Start with all IDs, then intersect with filter criteria
	matchingIDs := getMatchingIDs(criteria, index)

	// Filter vinyl to matching IDs
	filtered := make([]VinylRecord, 0)
	for _, v := range vinyls {
		if _, ok := matchingIDs[v.VinylID]; ok {
			filtered = append(filtered, v)
		}
	}

	// Sort by artist name, then album name.
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].VinylArtist != filtered[j].VinylArtist {
			return filtered[i].VinylArtist < filtered[j].VinylArtist
		}
		return filtered[i].VinylTitle < filtered[j].VinylTitle
	})

	return filtered
}

// FilterVinylWithPlayData filters and sorts VinylWithPlayData records based on criteria
func FilterVinylWithPlayData(vinyls []VinylWithPlayData, criteria FilterCriteria, index *VinylIndex) []VinylWithPlayData {
	// Start with all IDs, then intersect with filter criteria
	matchingIDs := getMatchingIDs(criteria, index)

	// Filter vinyl to matching IDs
	filtered := make([]VinylWithPlayData, 0)
	for _, v := range vinyls {
		if _, ok := matchingIDs[v.VinylID]; ok {
			filtered = append(filtered, v)
		}
	}

	// Sort by artist name, then album name (access via embedded VinylUnique)
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].VinylArtist != filtered[j].VinylArtist {
			return filtered[i].VinylArtist < filtered[j].VinylArtist
		}
		return filtered[i].VinylTitle < filtered[j].VinylTitle
	})

	return filtered
}

// getMatchingIDs returns the set of vinyl IDs that match all filter criteria
// Categories are combined with AND logic, values within a category use OR logic
func getMatchingIDs(criteria FilterCriteria, index *VinylIndex) map[int64]struct{} {
	var result map[int64]struct{}

	// Helper to intersect two sets
	intersect := func(a, b map[int64]struct{}) map[int64]struct{} {
		if a == nil {
			return b
		}
		if b == nil {
			return a
		}
		intersection := make(map[int64]struct{})
		for id := range a {
			if _, ok := b[id]; ok {
				intersection[id] = struct{}{}
			}
		}
		return intersection
	}

	// Helper to union multiple sets (OR within category)
	union := func(sets []map[int64]struct{}) map[int64]struct{} {
		if len(sets) == 0 {
			return nil
		}
		unified := make(map[int64]struct{})
		for _, set := range sets {
			for id := range set {
				unified[id] = struct{}{}
			}
		}
		return unified
	}

	// Filter by artist (exact match)
	if criteria.Artist != "" {
		artistKey := strings.ToLower(strings.TrimSpace(criteria.Artist))
		if artistSet, ok := index.ArtistMap[artistKey]; ok {
			result = intersect(result, artistSet)
		} else {
			// No match found, return empty set
			return make(map[int64]struct{})
		}
	}

	// Filter by genres (OR within genres, AND with other criteria)
	if len(criteria.Genres) > 0 {
		genreSets := make([]map[int64]struct{}, 0)
		for _, genre := range criteria.Genres {
			genreKey := strings.ToLower(strings.TrimSpace(genre))
			if genreSet, ok := index.GenreMap[genreKey]; ok {
				genreSets = append(genreSets, genreSet)
			}
		}
		if len(genreSets) > 0 {
			result = intersect(result, union(genreSets))
		} else {
			// No matching genres found
			return make(map[int64]struct{})
		}
	}

	// Filter by styles (OR within styles, AND with other criteria)
	if len(criteria.Styles) > 0 {
		styleSets := make([]map[int64]struct{}, 0)
		for _, style := range criteria.Styles {
			styleKey := strings.ToLower(strings.TrimSpace(style))
			if styleSet, ok := index.StyleMap[styleKey]; ok {
				styleSets = append(styleSets, styleSet)
			}
		}
		if len(styleSets) > 0 {
			result = intersect(result, union(styleSets))
		} else {
			// No matching styles found
			return make(map[int64]struct{})
		}
	}

	// If no filters were applied, return all vinyl IDs from index
	if result == nil {
		result = make(map[int64]struct{})
		for _, idSet := range index.ArtistMap {
			for id := range idSet {
				result[id] = struct{}{}
			}
		}
	}

	return result
}
