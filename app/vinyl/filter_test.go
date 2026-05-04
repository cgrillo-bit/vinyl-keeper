package vinyl

import (
	"testing"
)

func TestBuildVinylIndex(t *testing.T) {
	genres1 := "Rock,Pop"
	styles1 := "Alternative Rock,Indie Pop"
	genres2 := "Electronic"
	styles2 := "Techno,House"

	vinyls := []VinylRecord{
		{
			VinylID:           1,
			VinylTitle:        "Album A",
			VinylArtist:       "Artist One",
			VinylPressingYear: 2020,
			Genres:            &genres1,
			Styles:            &styles1,
		},
		{
			VinylID:           2,
			VinylTitle:        "Album B",
			VinylArtist:       "Artist Two",
			VinylPressingYear: 2021,
			Genres:            &genres2,
			Styles:            &styles2,
		},
		{
			VinylID:           3,
			VinylTitle:        "Album C",
			VinylArtist:       "Artist One",
			VinylPressingYear: 2019,
			Genres:            &genres1,
			Styles:            nil,
		},
	}

	index := BuildVinylIndex(vinyls)

	// Check artists
	if len(index.Artists) != 2 {
		t.Errorf("expected 2 artists, got %d", len(index.Artists))
	}

	// Check genres
	if len(index.Genres) != 3 {
		t.Errorf("expected 3 genres (Rock, Pop, Electronic), got %d", len(index.Genres))
	}

	// Check styles
	if len(index.Styles) != 4 {
		t.Errorf("expected 4 styles, got %d", len(index.Styles))
	}

	// Check artist map (case-insensitive)
	artistOneIDs := index.ArtistMap["artist one"]
	if len(artistOneIDs) != 2 {
		t.Errorf("expected 2 vinyls for 'artist one', got %d", len(artistOneIDs))
	}

	// Check genre map
	rockIDs := index.GenreMap["rock"]
	if len(rockIDs) != 2 {
		t.Errorf("expected 2 vinyls with 'rock' genre, got %d", len(rockIDs))
	}
}

func TestFilterVinylUnique(t *testing.T) {
	genres1 := "Rock,Pop"
	styles1 := "Alternative Rock"
	genres2 := "Electronic"

	vinyls := []VinylRecord{
		{
			VinylID:           1,
			VinylTitle:        "Album A",
			VinylArtist:       "Artist One",
			VinylPressingYear: 2020,
			Genres:            &genres1,
			Styles:            &styles1,
		},
		{
			VinylID:           2,
			VinylTitle:        "Album B",
			VinylArtist:       "Artist Two",
			VinylPressingYear: 2021,
			Genres:            &genres2,
			Styles:            nil,
		},
		{
			VinylID:           3,
			VinylTitle:        "Album C",
			VinylArtist:       "Artist One",
			VinylPressingYear: 2019,
			Genres:            &genres1,
			Styles:            nil,
		},
	}

	index := BuildVinylIndex(vinyls)

	t.Run("filter by artist", func(t *testing.T) {
		criteria := FilterCriteria{
			Artist: "Artist One",
		}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 2 {
			t.Errorf("expected 2 vinyls for 'Artist One', got %d", len(filtered))
		}
	})

	t.Run("filter by genre", func(t *testing.T) {
		criteria := FilterCriteria{
			Genres: []string{"Rock"},
		}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 2 {
			t.Errorf("expected 2 vinyls with 'Rock' genre, got %d", len(filtered))
		}
	})

	t.Run("filter by artist and genre", func(t *testing.T) {
		criteria := FilterCriteria{
			Artist: "Artist One",
			Genres: []string{"Rock"},
		}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 2 {
			t.Errorf("expected 2 vinyls for 'Artist One' with 'Rock', got %d", len(filtered))
		}
	})

	t.Run("filter by multiple genres (OR)", func(t *testing.T) {
		criteria := FilterCriteria{
			Genres: []string{"Rock", "Electronic"},
		}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 3 {
			t.Errorf("expected 3 vinyls with 'Rock' OR 'Electronic', got %d", len(filtered))
		}
	})

	t.Run("filter by style", func(t *testing.T) {
		criteria := FilterCriteria{
			Styles: []string{"Alternative Rock"},
		}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 1 {
			t.Errorf("expected 1 vinyl with 'Alternative Rock' style, got %d", len(filtered))
		}
	})

	t.Run("no filters returns all (sorted)", func(t *testing.T) {
		criteria := FilterCriteria{}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 3 {
			t.Errorf("expected 3 vinyls with no filters, got %d", len(filtered))
		}
		// Check sorting by artist then title
		// Artist One: Album A, Album C
		// Artist Two: Album B
		if filtered[0].VinylArtist != "Artist One" || filtered[0].VinylTitle != "Album A" {
			t.Errorf("expected first vinyl to be 'Artist One - Album A', got '%s - %s'", filtered[0].VinylArtist, filtered[0].VinylTitle)
		}
		if filtered[1].VinylArtist != "Artist One" || filtered[1].VinylTitle != "Album C" {
			t.Errorf("expected second vinyl to be 'Artist One - Album C', got '%s - %s'", filtered[1].VinylArtist, filtered[1].VinylTitle)
		}
		if filtered[2].VinylArtist != "Artist Two" || filtered[2].VinylTitle != "Album B" {
			t.Errorf("expected third vinyl to be 'Artist Two - Album B', got '%s - %s'", filtered[2].VinylArtist, filtered[2].VinylTitle)
		}
	})

	t.Run("no matching filter returns empty", func(t *testing.T) {
		criteria := FilterCriteria{
			Artist: "Nonexistent Artist",
		}
		filtered := FilterVinyl(vinyls, criteria, index)
		if len(filtered) != 0 {
			t.Errorf("expected 0 vinyls for nonexistent artist, got %d", len(filtered))
		}
	})
}

func TestFilterVinylWithPlayData(t *testing.T) {
	genres1 := "Rock"
	firstPlayed := "2024-01-01"
	lastPlayed := "2024-01-15"

	vinyls := []VinylWithPlayData{
		{
			VinylRecord: VinylRecord{
				VinylID:           1,
				VinylTitle:        "Album A",
				VinylArtist:       "Artist One",
				VinylPressingYear: 2020,
				Genres:            &genres1,
			},
			Plays:       5,
			FirstPlayed: &firstPlayed,
			LastPlayed:  &lastPlayed,
		},
		{
			VinylRecord: VinylRecord{
				VinylID:           2,
				VinylTitle:        "Album B",
				VinylArtist:       "Artist Two",
				VinylPressingYear: 2021,
				Genres:            nil,
			},
			Plays:       1,
			FirstPlayed: &firstPlayed,
			LastPlayed:  &lastPlayed,
		},
	}

	index := BuildVinylIndex([]VinylRecord{vinyls[0].VinylRecord, vinyls[1].VinylRecord})

	t.Run("filter with play data", func(t *testing.T) {
		criteria := FilterCriteria{
			Genres: []string{"Rock"},
		}
		filtered := FilterVinylWithPlayData(vinyls, criteria, index)
		if len(filtered) != 1 {
			t.Errorf("expected 1 vinyl with 'Rock' genre, got %d", len(filtered))
		}
		if filtered[0].Plays != 5 {
			t.Errorf("expected play data to be preserved, got %d plays", filtered[0].Plays)
		}
	})
}
