package types

import (
	"fmt"
	"strings"

	"github.com/ninesl/vinyl-keeper/app/router/values"
	"github.com/ninesl/vinyl-keeper/app/vinyl"
)

type Vinyl interface {
	ID() int64
	ReleaseID() int64
	Title() string
	Artist() string
	Genres() *string
	Styles() *string
	Year() int64
	Country() *string
	ReleaseDate() string
	RecentPrice() *float64
	ImageExtension() string
	CoverRawBlob() []byte
	CoverEmbedding() []byte
	NumPlays() *int64
	DateAdded() *string
	LastPlayedDate() *string
	Image() string
	BlobImageURL() string
	ThumbImageURL() *string
	HasBlob() bool
	PressingLabel() *string
	Notes() *string
}

type VinylView struct {
	Record            vinyl.VinylRecord
	ReleaseIDVal      int64
	PlaysVal          *int64
	AddedVal          *string
	LastVal           *string
	ImageVal          *string
	HasBlobVal        bool
	LabelVal          *string
	ReleaseFormatVal  *string
	ReleaseCountryVal *string
	NotesVal          *string
}

func FromVinylRecord(record vinyl.VinylRecord) VinylView {
	return VinylView{Record: record, ReleaseIDVal: 0}
}

func FromVinylWithPlayData(v vinyl.VinylWithPlayData) VinylView {
	plays := v.Plays
	return VinylView{
		Record:            v.VinylRecord,
		ReleaseIDVal:      v.ReleaseID,
		PlaysVal:          &plays,
		AddedVal:          v.FirstPlayed,
		LastVal:           v.LastPlayed,
		ImageVal:          v.CoverURI,
		HasBlobVal:        v.HasBlob,
		LabelVal:          v.Label,
		ReleaseFormatVal:  v.ReleaseFormat,
		ReleaseCountryVal: v.ReleaseCountry,
		NotesVal:          v.Notes,
	}
}

func FromReleaseCandidate(v vinyl.ReleaseCandidate) VinylView {
	return VinylView{
		Record:            v.VinylRecord,
		ReleaseIDVal:      v.ReleaseID,
		ImageVal:          v.CoverURI,
		HasBlobVal:        v.HasBlob,
		LabelVal:          v.Label,
		ReleaseFormatVal:  v.ReleaseFormat,
		ReleaseCountryVal: v.ReleaseCountry,
		NotesVal:          v.Notes,
	}
}

func composePressingLabel(label, releaseFormat, country *string) *string {
	parts := make([]string, 0, 3)
	if label != nil {
		if v := strings.TrimSpace(*label); v != "" {
			parts = append(parts, v)
		}
	}
	if releaseFormat != nil {
		if v := strings.TrimSpace(*releaseFormat); v != "" {
			parts = append(parts, v)
		}
	}
	if country != nil {
		if v := strings.TrimSpace(*country); v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	joined := strings.Join(parts, " / ")
	return &joined
}

func (v VinylView) ID() int64        { return v.Record.VinylID }
func (v VinylView) ReleaseID() int64 { return v.ReleaseIDVal }
func (v VinylView) Title() string    { return v.Record.VinylTitle }
func (v VinylView) Artist() string   { return v.Record.VinylArtist }
func (v VinylView) Genres() *string  { return v.Record.Genres }
func (v VinylView) Styles() *string  { return v.Record.Styles }
func (v VinylView) Year() int64      { return v.Record.VinylPressingYear }
func (v VinylView) Country() *string {
	if v.ReleaseCountryVal != nil {
		return v.ReleaseCountryVal
	}
	return v.Record.Country
}
func (v VinylView) ReleaseDate() string     { return v.Record.Released }
func (v VinylView) RecentPrice() *float64   { return v.Record.RecentPrice }
func (v VinylView) ImageExtension() string  { return v.Record.ImageExtension }
func (v VinylView) CoverRawBlob() []byte    { return v.Record.CoverRawBlob }
func (v VinylView) CoverEmbedding() []byte  { return v.Record.CoverEmbedding }
func (v VinylView) NumPlays() *int64        { return v.PlaysVal }
func (v VinylView) DateAdded() *string      { return v.AddedVal }
func (v VinylView) LastPlayedDate() *string { return v.LastVal }
func (v VinylView) Image() string {
	// Global default: always use blob-backed image routes.
	// Thumb URLs are only consumed by pressing chooser fallback via ThumbImageURL().
	return v.BlobImageURL()
}
func (v VinylView) BlobImageURL() string {
	if v.ReleaseIDVal > 0 {
		return fmt.Sprintf(values.EndpointCover+"/%d/%d", v.Record.VinylID, v.ReleaseIDVal)
	}
	return fmt.Sprintf(values.EndpointCover+"/%d", v.Record.VinylID)
}
func (v VinylView) ThumbImageURL() *string {
	if v.ImageVal != nil {
		if src := strings.TrimSpace(*v.ImageVal); src != "" {
			return &src
		}
	}
	return nil
}
func (v VinylView) HasBlob() bool { return v.HasBlobVal || v.ReleaseIDVal == 0 }
func (v VinylView) PressingLabel() *string {
	return composePressingLabel(v.LabelVal, v.ReleaseFormatVal, v.Country())
}
func (v VinylView) Notes() *string { return v.NotesVal }
