package types

import "github.com/ninesl/vinyl-keeper/app/vinyl"

type Vinyl interface {
	ID() int64
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
}

type VinylView struct {
	Record   vinyl.VinylRecord
	PlaysVal *int64
	AddedVal *string
	LastVal  *string
}

func FromVinylRecord(record vinyl.VinylRecord) VinylView {
	return VinylView{Record: record}
}

func FromVinylWithPlayData(v vinyl.VinylWithPlayData) VinylView {
	plays := v.Plays
	return VinylView{
		Record:   v.VinylRecord,
		PlaysVal: &plays,
		AddedVal: v.FirstPlayed,
		LastVal:  v.LastPlayed,
	}
}

func (v VinylView) ID() int64               { return v.Record.VinylID }
func (v VinylView) Title() string           { return v.Record.VinylTitle }
func (v VinylView) Artist() string          { return v.Record.VinylArtist }
func (v VinylView) Genres() *string         { return v.Record.Genres }
func (v VinylView) Styles() *string         { return v.Record.Styles }
func (v VinylView) Year() int64             { return v.Record.VinylPressingYear }
func (v VinylView) Country() *string        { return v.Record.Country }
func (v VinylView) ReleaseDate() string     { return v.Record.Released }
func (v VinylView) RecentPrice() *float64   { return v.Record.RecentPrice }
func (v VinylView) ImageExtension() string  { return v.Record.ImageExtension }
func (v VinylView) CoverRawBlob() []byte    { return v.Record.CoverRawBlob }
func (v VinylView) CoverEmbedding() []byte  { return v.Record.CoverEmbedding }
func (v VinylView) NumPlays() *int64        { return v.PlaysVal }
func (v VinylView) DateAdded() *string      { return v.AddedVal }
func (v VinylView) LastPlayedDate() *string { return v.LastVal }
